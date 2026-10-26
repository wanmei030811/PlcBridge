// 后台目标健康巡检（runHealthLoop → probeTargets）的测试。
//
// 覆盖 2026-10-25 修复的第二个问题：
// "influxdb 服务停掉很久后，状态才不能自动刷新，只有触发诊断后，才能显示异常"。
//
// 根因：Target 的连接状态只在 Send() 或手动诊断时才会被刷新。
// 上游 PLC 离线 → forward 队列为空 → Send 从不执行 → 状态永远是"在线"，
// 唯一的探活入口是用户手动点"诊断"。
//
// 修复：新增后台健康巡检循环，定期对所有 Target client 调 HealthCheck()。
package forward

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// healthProbeClient 记录 HealthCheck 调用次数的假 client。
type healthProbeClient struct {
	mu        sync.Mutex
	calls     int
	lastErr   error
	healthy   bool
	connected bool
}

func (f *healthProbeClient) Send(ctx context.Context, batch []target.PointValue) error { return nil }

func (f *healthProbeClient) HealthCheck(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.lastErr
}

func (f *healthProbeClient) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *healthProbeClient) LastError() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastErr == nil {
		return ""
	}
	return f.lastErr.Error()
}

func (f *healthProbeClient) ProbeConnect(ctx context.Context) error { return nil }
func (f *healthProbeClient) Stats() map[string]any                  { return map[string]any{} }

func (f *healthProbeClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newHealthTestWorkers 构造一个带假 client 的 Workers。
func newHealthTestWorkers(t *testing.T, cfg *platform.Config, clients map[string]target.TargetClient) *Workers {
	t.Helper()
	store := config.NewStore(t.TempDir())
	pools := pool.NewPools(pool.Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})
	pools.Start(context.Background(), nil)
	t.Cleanup(func() { pools.Stop(time.Second) })

	w := NewWorkers(store, pools, target.NewRouter(store))
	w.mu.Lock()
	w.cache = &clientCache{cfg: cfg, clients: clients}
	w.mu.Unlock()
	return w
}

// TestProbeTargets_CallsHealthCheckOnAllClients 验证 probeTargets 会对所有 client 探活。
//
// 回归场景：之前 forward 侧没有任何后台探活 —— 只有手动诊断才刷新状态。
func TestProbeTargets_CallsHealthCheckOnAllClients(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "ch_influx", Role: "forward", Type: "influxdb", Address: "http://127.0.0.1:8086"},
		},
		Devices: []*platform.Device{
			{
				DeviceBizID: "dev1",
				Enabled:     true,
				Targets: []*platform.Target{
					{TargetBizID: "t1", ForwardChannelBizID: "ch_influx", Topic: "m"},
					{TargetBizID: "t2", ForwardChannelBizID: "ch_influx", Topic: "m"},
				},
			},
		},
	}
	c1 := &healthProbeClient{}
	c2 := &healthProbeClient{}
	w := newHealthTestWorkers(t, cfg, map[string]target.TargetClient{"t1": c1, "t2": c2})

	w.probeTargets(context.Background())

	if c1.callCount() != 1 {
		t.Errorf("c1 HealthCheck 调用次数 = %d, want 1", c1.callCount())
	}
	if c2.callCount() != 1 {
		t.Errorf("c2 HealthCheck 调用次数 = %d, want 1", c2.callCount())
	}
}

// TestProbeTargets_ConcurrentWithGetOrCreate 验证并发读写 clientCache 不 panic。
//
// 回归场景：后台巡检读 cache.clients 的同时，配置热重载可能在写同一张 map。
// Go 的 map 并发读写会直接触发 fatal error（进程崩溃，不可恢复）。
func TestProbeTargets_ConcurrentWithGetOrCreate(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "ch_influx", Role: "forward", Type: "influxdb", Address: "http://127.0.0.1:8086"},
		},
		Devices: []*platform.Device{
			{
				DeviceBizID: "dev1",
				Enabled:     true,
				Targets: []*platform.Target{
					{TargetBizID: "t1", ForwardChannelBizID: "ch_influx", Topic: "m"},
				},
			},
		},
	}
	w := newHealthTestWorkers(t, cfg, map[string]target.TargetClient{
		"t1": &healthProbeClient{},
	})

	var wg sync.WaitGroup
	// 读者：模拟后台巡检（跑固定轮数后退出，避免测试挂死）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			w.probeTargets(context.Background())
		}
	}()
	// 写者：模拟配置热重载时新增 client
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			w.cache.mu.Lock()
			w.cache.clients["t2"] = &healthProbeClient{}
			w.cache.mu.Unlock()
		}
	}()
	// 用 done channel + 超时守护：一旦出现死锁，测试快速失败（而不是耗尽 go test 默认 10m 超时）。
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("TestProbeTargets_ConcurrentWithGetOrCreate 超时：疑似 clientCache 读写死锁")
	}
}
