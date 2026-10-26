package write

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// newWriteTestConfig 构造一个带 write-enabled source 的最小 Config。
//
// 关键点：channel 的 Role 必须是 "collect"（lookupFirstCollectChannelCfg 只认 collect），
// device 必须 Enabled 且 source 必须 WriteEnabled，否则 spawnAll 会跳过该 device。
func newWriteTestConfig(channelBizID, deviceBizID string) *platform.Config {
	return &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{
				ChannelBizID: channelBizID,
				Type:         "hb-test-s7",
				Role:         "collect",
				Address:      "127.0.0.1:102",
			},
		},
		Devices: []*platform.Device{
			{
				DeviceBizID: deviceBizID,
				Name:        "write-test-device",
				Enabled:     true,
				Sources: []*platform.Source{
					{
						SourceBizID:    "src-" + deviceBizID,
						ChannelBizID:   channelBizID,
						CollectEnabled: true,
						WriteEnabled:   true,
						Points: []*platform.Point{
							{
								PointBizID:     "pt-1",
								Address:        "M0.0",
								DataType:       "bool",
								CollectEnabled: true,
								WriteEnabled:   true,
							},
						},
					},
				},
			},
		},
	}
}

// TestWriteWorker_ConsumesEnqueuedCommand 是 P0 回归测试。
//
// 历史 bug：spawnAll 在配置加载时就跑，此时 device 的 write queue 还没被
// Enqueue 创建 -> runWriter 拿到 nil queue 直接 return -> writer 永久退出
// -> 写命令入队后无人消费 -> API 一直超时。
//
// 本测试不预先创建 queue，直接 Enqueue 后断言能在超时前拿到结果，
// 验证 writer 确实在消费队列。
func TestWriteWorker_ConsumesEnqueuedCommand(t *testing.T) {
	cli := &fakeChannel{}
	registerTestPlugin(t, "hb-test-s7", cli)

	cfg := newWriteTestConfig("ch-wr-1", "dev-wr-1")
	store := config.NewStore("")
	store.SyncFrom(cfg)

	pools := pool.NewPools(pool.Config{WriteSize: 4})
	pools.Start(context.Background(), nil)
	defer pools.Stop(2 * time.Second)

	reg := protocol.NewChannelRegistry()
	w := NewWorkers(store, pools, reg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 启动 supervisor（内部 5s ticker 会触发 spawnAll）。
	w.Start(ctx)

	// 关键：先 Enqueue —— 此时 queue 尚未被创建，必须由 writer 自己 ensureQueue。
	cmd := Command{
		TraceID:     "t-write-1",
		DeviceBizID: "dev-wr-1",
		PointBizID:  "pt-1",
		Value:       true,
		DataType:    "bool",
		TimeoutMs:   2000,
		ResultChan:  make(chan Result, 1),
	}
	if err := w.Enqueue(cmd); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// supervisor 的 ticker 是 5s，等待 writer 被 spawn 并消费。
	select {
	case res := <-cmd.ResultChan:
		if res.Result != "success" {
			t.Fatalf("写结果 = %q, want success (error=%q)", res.Result, res.Error)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("写命令超时：writer 未消费队列（write worker 未被正确 spawn）")
	}
}

// TestEnqueue_CreatesQueueLazily 验证 Enqueue 会在 queue 不存在时创建它，
// 且后续 Enqueue 复用同一个 channel（保证 FIFO 顺序不被破坏）。
func TestEnqueue_CreatesQueueLazily(t *testing.T) {
	w := &Workers{queues: make(map[string]chan Command)}

	q1 := w.ensureQueue("dev-a")
	q2 := w.ensureQueue("dev-a")
	if q1 != q2 {
		t.Fatalf("ensureQueue 返回了不同的 channel，FIFO 顺序会被破坏")
	}
	if cap(q1) != 1024 {
		t.Fatalf("queue cap = %d, want 1024", cap(q1))
	}
}

// TestSpawnAll_QueuesSurviveReload 验证 reload 时 pruneQueues 不会把
// 仍然启用的 device 的 queue 误删（否则 writer 会卡在旧 channel 上）。
func TestSpawnAll_QueuesSurviveReload(t *testing.T) {
	w := &Workers{queues: make(map[string]chan Command)}
	cfg := newWriteTestConfig("ch-wr-2", "dev-wr-2")

	w.ensureQueue("dev-wr-2")
	w.pruneQueues(cfg)

	w.queuesMu.RLock()
	_, ok := w.queues["dev-wr-2"]
	w.queuesMu.RUnlock()
	if !ok {
		t.Fatalf("pruneQueues 删除了仍然启用的 device queue")
	}
}

// TestEnqueue_RejectsUnwritableDevices 验证入队前就拒绝"没有 write worker
// 可以消费"的命令。
//
// 不拒绝的话：命令静静躺在队列里，API 一直等到 504 网关超时。
// 用户看到的错误（"网关超时"）与真实原因（"设备没开回写"）完全无关，
// 现场排查会被带偏。
func TestEnqueue_RejectsUnwritableDevices(t *testing.T) {
	cli := &fakeChannel{}
	registerTestPlugin(t, "hb-test-s7", cli)

	cfg := newWriteTestConfig("ch-wr-3", "dev-wr-3")
	// 设备停用
	disabled := newWriteTestConfig("ch-wr-4", "dev-wr-4")
	disabled.Devices[0].Enabled = false
	// 回写未启用
	noWrite := newWriteTestConfig("ch-wr-5", "dev-wr-5")
	noWrite.Devices[0].Sources[0].WriteEnabled = false

	store := config.NewStore("")
	store.SyncFrom(cfg)
	// 把另外两个设备塞进同一个 config
	merged := newWriteTestConfig("ch-wr-3", "dev-wr-3")
	merged.Devices = append(merged.Devices, disabled.Devices...)
	merged.Devices = append(merged.Devices, noWrite.Devices...)
	store.SyncFrom(merged)

	pools := pool.NewPools(pool.Config{WriteSize: 4})
	pools.Start(context.Background(), nil)
	defer pools.Stop(2 * time.Second)

	w := NewWorkers(store, pools, protocol.NewChannelRegistry())

	cases := []struct {
		name       string
		deviceBiz  string
		wantReason string
	}{
		{"设备停用", "dev-wr-4", "设备已停用"},
		{"回写未启用", "dev-wr-5", "未启用回写"},
		{"设备不存在", "dev-nope", "设备不存在"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := w.Enqueue(Command{
				TraceID:     "t-" + tc.deviceBiz,
				DeviceBizID: tc.deviceBiz,
				PointBizID:  "pt-1",
				Value:       true,
				DataType:    "bool",
				ResultChan:  make(chan Result, 1),
			})
			if err == nil {
				t.Fatalf("Enqueue 应返回错误（%s），却成功了", tc.wantReason)
			}
			var nw *NotWritableError
			if !errors.As(err, &nw) {
				t.Fatalf("err = %v (%T)，want *NotWritableError", err, err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("err = %q, want 含 %q", err.Error(), tc.wantReason)
			}
		})
	}
}
