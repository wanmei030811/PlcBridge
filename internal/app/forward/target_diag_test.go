// Plan3 3.4：Target 诊断快照（TargetSnapshot）的字段契约测试。
//
// 覆盖 2026-10-24 补齐的两件事：
//  1. `type` 字段语义修正 —— 之前错误地填了 Target 的序列化格式（json/line_protocol），
//     现在改为填转发协议类型（mqtt / influxdb），格式单独放到 `format` 字段。
//  2. 新增 `last_error` 字段 —— 前端诊断面板要靠它区分"离线"和"异常"。
package forward

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// fakeTargetClient 是 target.TargetClient 的测试替身。
type fakeTargetClient struct {
	connected bool
	lastErr   string
}

func (f *fakeTargetClient) Send(ctx context.Context, batch []target.PointValue) error { return nil }
func (f *fakeTargetClient) HealthCheck(ctx context.Context) error                     { return nil }
func (f *fakeTargetClient) IsConnected() bool                                         { return f.connected }
func (f *fakeTargetClient) LastError() string                                         { return f.lastErr }
func (f *fakeTargetClient) ProbeConnect(ctx context.Context) error                    { return nil }
func (f *fakeTargetClient) Stats() map[string]any {
	return map[string]any{
		"connection_state": "Connected",
		"sent_count":       int64(42),
	}
}

// newDiagTestWorkers 构造一个带假 client 的 Workers（直接塞 cache，不走 spawnAll）。
func newDiagTestWorkers(t *testing.T, cfg *platform.Config, clients map[string]target.TargetClient) *Workers {
	t.Helper()
	store := config.NewStore(t.TempDir())
	pools := pool.NewPools(pool.Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})
	pools.Start(context.Background(), nil)
	t.Cleanup(func() { pools.Stop(time.Second) })

	router := target.NewRouter(store)
	w := NewWorkers(store, pools, router)
	w.mu.Lock()
	w.cache = &clientCache{cfg: cfg, clients: clients}
	w.mu.Unlock()
	return w
}

// diagTargets 解析 TargetSnapshot 的输出。
func diagTargets(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var out struct {
		Targets []map[string]any `json:"targets"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode snapshot: %v (raw=%s)", err, string(raw))
	}
	return out.Targets
}

// TestTargetSnapshot_TypeIsProtocolAndFormatIsSeparate 验证 type / format 字段语义分离。
//
// 回归场景：之前 type 填的是 tgt.Format（json/line_protocol/raw），
// 前端拿它当协议名展示会显示 "json" 而不是 "mqtt"。
func TestTargetSnapshot_TypeIsProtocolAndFormatIsSeparate(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "ch_mqtt", Role: "forward", Type: "mqtt", Address: "tcp://127.0.0.1:1883"},
		},
		Devices: []*platform.Device{
			{
				DeviceBizID: "dev1",
				Enabled:     true,
				Targets: []*platform.Target{
					{
						TargetBizID:         "t1",
						ForwardChannelBizID: "ch_mqtt",
						Topic:               "plcbridge/dev1",
						Format:              "json",
					},
				},
			},
		},
	}
	clients := map[string]target.TargetClient{
		"t1": &fakeTargetClient{connected: true},
	}

	w := newDiagTestWorkers(t, cfg, clients)
	targets := diagTargets(t, w.TargetSnapshot())

	if len(targets) != 1 {
		t.Fatalf("targets 数量 = %d, want 1", len(targets))
	}
	got := targets[0]
	if got["type"] != "mqtt" {
		t.Errorf("type = %v, want mqtt（应是转发协议类型，不是序列化格式）", got["type"])
	}
	if got["format"] != "json" {
		t.Errorf("format = %v, want json", got["format"])
	}
}

// TestTargetSnapshot_IncludesLastError 验证 last_error 字段透传。
//
// 前端诊断面板靠它区分"离线但无故障"（灰）与"异常"（红）。
func TestTargetSnapshot_IncludesLastError(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "ch_mqtt", Role: "forward", Type: "mqtt", Address: "tcp://127.0.0.1:1883"},
		},
		Devices: []*platform.Device{
			{
				DeviceBizID: "dev1",
				Enabled:     true,
				Targets: []*platform.Target{
					{TargetBizID: "t1", ForwardChannelBizID: "ch_mqtt", Topic: "x", Format: "json"},
				},
			},
		},
	}
	clients := map[string]target.TargetClient{
		"t1": &fakeTargetClient{connected: false, lastErr: "connection reset by peer"},
	}

	w := newDiagTestWorkers(t, cfg, clients)
	targets := diagTargets(t, w.TargetSnapshot())

	if len(targets) != 1 {
		t.Fatalf("targets 数量 = %d, want 1", len(targets))
	}
	got := targets[0]
	if got["last_error"] != "connection reset by peer" {
		t.Errorf("last_error = %v, want %q", got["last_error"], "connection reset by peer")
	}
	if got["connected"] != false {
		t.Errorf("connected = %v, want false", got["connected"])
	}
	// 协议自报的 stats 应原样透传
	stats, ok := got["stats"].(map[string]any)
	if !ok {
		t.Fatalf("stats 类型 = %T, want map", got["stats"])
	}
	// JSON 解到 any 后数字是 float64
	if stats["sent_count"] != float64(42) {
		t.Errorf("stats.sent_count = %v, want 42", stats["sent_count"])
	}
}

// TestTargetSnapshot_UnknownChannelTypeIsEmpty 验证 channel 找不到时 type 为空串（不 panic）。
func TestTargetSnapshot_UnknownChannelTypeIsEmpty(t *testing.T) {
	cfg := &platform.Config{
		Version: 1,
		Devices: []*platform.Device{
			{
				DeviceBizID: "dev1",
				Enabled:     true,
				Targets: []*platform.Target{
					{TargetBizID: "t1", ForwardChannelBizID: "missing_ch", Topic: "x", Format: "json"},
				},
			},
		},
	}
	clients := map[string]target.TargetClient{
		"t1": &fakeTargetClient{connected: true},
	}

	w := newDiagTestWorkers(t, cfg, clients)
	targets := diagTargets(t, w.TargetSnapshot())

	if len(targets) != 1 {
		t.Fatalf("targets 数量 = %d, want 1", len(targets))
	}
	if got := targets[0]["type"]; got != "" {
		t.Errorf("type = %v, want 空字符串", got)
	}
}
