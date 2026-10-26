// HeartbeatSupervisor / flip 的单元测试。
//
// 重点验证：flip() 必须真的调 ProtocolChannel.Write() 把 0/1 写入 heartbeat_address。
// 历史 bug：早期实现只翻内存变量 w.lastValue，没有触发任何 Write，导致 S7 通道
// 心跳机制形同虚设（Plan §6.3.3）。
package write

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// fakeChannel 实现 protocol.ProtocolChannel，记录所有 Write 调用。
type fakeChannel struct {
	addr     string
	dataType string
	mu       sync.Mutex
	writes   []writeCall
	writeErr error
	closed   atomic.Bool
}

type writeCall struct {
	Address  string
	Value    any
	DataType string
}

func (f *fakeChannel) Read(_ context.Context, _ string, _ string) (protocol.Value, error) {
	return protocol.Value{}, nil
}
func (f *fakeChannel) BatchRead(_ context.Context, _ []*platform.Point) (map[string]protocol.Value, error) {
	return nil, nil
}
func (f *fakeChannel) Write(_ context.Context, address string, value any, dataType string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, writeCall{address, value, dataType})
	return nil
}
func (f *fakeChannel) BatchWrite(_ context.Context, _ []protocol.WriteOp) error { return nil }
func (f *fakeChannel) Connect(_ context.Context) error                          { return nil }
func (f *fakeChannel) Reconnect(_ context.Context) error                        { return nil }
func (f *fakeChannel) Disconnect() error                                        { return nil }
func (f *fakeChannel) HealthCheck(_ context.Context) error                      { return nil }
func (f *fakeChannel) IsConnected() bool                                        { return true }
func (f *fakeChannel) ProbeActive(_ context.Context) error                      { return nil }
func (f *fakeChannel) LastActiveAt() time.Time                                  { return time.Time{} }
func (f *fakeChannel) LastError() string                                        { return "" }
func (f *fakeChannel) LastErrorAt() time.Time                                   { return time.Time{} }
func (f *fakeChannel) Stats() map[string]any {
	return map[string]any{"connection_state": "Connected", "fake": true}
}

// fakePlugin 实现 ProtocolPlugin，返回我们注入的 fakeChannel。
type fakePlugin struct {
	name string
	cli  *fakeChannel
}

func (p *fakePlugin) Name() string                      { return p.name }
func (p *fakePlugin) Version() string                   { return "test-1.0" }
func (p *fakePlugin) ConfigSchema() json.RawMessage     { return nil }
func (p *fakePlugin) ParseConfig(_ []byte) (any, error) { return nil, nil }
func (p *fakePlugin) Init(_ context.Context) error      { return nil }
func (p *fakePlugin) Close() error                      { return nil }
func (p *fakePlugin) Capabilities() protocol.Capability { return protocol.Capability{} }
func (p *fakePlugin) CreateChannel(_ *platform.Channel) (protocol.ProtocolChannel, error) {
	return p.cli, nil
}

// TestHeartbeatFlip_ActuallyWritesPLC 验证 flip 调用了 cli.Write，并写入正确的
// 0/1 翻转值到 heartbeat_address。这是 P0-1 修复的回归测试。
func TestHeartbeatFlip_ActuallyWritesPLC(t *testing.T) {
	cli := &fakeChannel{}
	registerTestPlugin(t, "hb-test-s7", cli)

	cfg := newTestConfigWithHeartbeat("ch-1", "M0.0", "hb-test-s7")
	store := config.NewStore("")
	store.SyncFrom(cfg)

	pools := pool.NewPools(pool.Config{WriteSize: 4})
	pools.Start(context.Background(), nil)
	defer pools.Stop(2 * time.Second)

	reg := protocol.NewChannelRegistry()
	_ = newHeartbeatSupervisor(store, pools, reg)

	// 直接调一次 flip（不走完整 worker 生命周期）
	w := &heartbeatWorker{
		channel:    cfg.Channels[0],
		sigCh:      make(chan struct{}, 1),
		lastValue:  0,
		debounceMs: 50,
	}
	// 取 channel 句柄
	acquired, err := reg.Acquire(context.Background(), cfg.Channels[0])
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// 翻 3 次，期望 3 次 Write（值 1, 0, 1）
	for i := 0; i < 3; i++ {
		w.flip(context.Background(), acquired)
	}

	cli.mu.Lock()
	defer cli.mu.Unlock()
	if len(cli.writes) != 3 {
		t.Fatalf("Write 调用次数 = %d, want 3", len(cli.writes))
	}
	expectedValues := []any{uint8(1), uint8(0), uint8(1)}
	for i, c := range cli.writes {
		if c.Address != "M0.0" {
			t.Errorf("write[%d].Address = %q, want M0.0", i, c.Address)
		}
		if c.DataType != "bool" {
			t.Errorf("write[%d].DataType = %q, want bool", i, c.DataType)
		}
		if c.Value != expectedValues[i] {
			t.Errorf("write[%d].Value = %v, want %v", i, c.Value, expectedValues[i])
		}
	}
}

// TestHeartbeatFlip_WriteError_Continues 验证 Write 失败时 worker 不退出，
// 只记 WARN，下一次 flip 仍继续尝试。
func TestHeartbeatFlip_WriteError_Continues(t *testing.T) {
	cli := &fakeChannel{}
	registerTestPlugin(t, "hb-test-s7-err", cli)

	cfg := newTestConfigWithHeartbeat("ch-err", "M0.1", "hb-test-s7-err")
	store := config.NewStore("")
	store.SyncFrom(cfg)
	_ = store

	reg := protocol.NewChannelRegistry()
	acquired, err := reg.Acquire(context.Background(), cfg.Channels[0])
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// 让 Write 返回错误
	cli.writeErr = &testError{msg: "fake PLC error"}

	w := &heartbeatWorker{
		channel:    cfg.Channels[0],
		sigCh:      make(chan struct{}, 1),
		lastValue:  0,
		debounceMs: 50,
	}
	// 多次 flip 不应 panic
	for i := 0; i < 5; i++ {
		w.flip(context.Background(), acquired)
	}

	// 错误清除后 Write 立即恢复
	cli.writeErr = nil
	w.flip(context.Background(), acquired)
	cli.mu.Lock()
	defer cli.mu.Unlock()
	if len(cli.writes) != 1 {
		t.Errorf("恢复后 Write 调用次数 = %d, want 1", len(cli.writes))
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// newTestConfigWithHeartbeat 构造一个带 heartbeat 配置的最小 Config。
func newTestConfigWithHeartbeat(channelBizID, address, chType string) *platform.Config {
	return &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{
				ChannelBizID:     channelBizID,
				Type:             chType,
				Role:             "collect",
				Address:          "127.0.0.1:102",
				HeartbeatEnabled: true,
				HeartbeatAddress: address,
				HeartbeatMaxMs:   100,
			},
		},
	}
}

// registerTestPlugin 注册 fake plugin；测试结束清理避免污染其他包。
func registerTestPlugin(t *testing.T, name string, cli *fakeChannel) {
	t.Helper()
	if regErr := protocol.Register(&fakePlugin{name: name, cli: cli}); regErr != nil {
		// 名字冲突：可能其他测试已注册。先 Get，看是否可复用。
		if _, getErr := protocol.Get(name); getErr != nil {
			t.Fatalf("Register(%s) 失败且 Get 也失败: reg=%v get=%v", name, regErr, getErr)
		}
	}
	t.Cleanup(func() { protocol.Unregister(name) })
}

// 避免 atomic 引用告警
var _ = atomic.Bool{}
