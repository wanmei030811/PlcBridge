package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// fakeChannelCounting 是带计数的 mock ProtocolChannel。
// 记录 Disconnect 被调用次数，用于验证引用计数归零后才真正断连。
type fakeChannelCounting struct {
	bizID         string
	disconnectCnt atomic.Int32
	connectCnt    atomic.Int32
	healthy       bool
	mu            sync.Mutex
}

func (f *fakeChannelCounting) Read(_ context.Context, _ string, _ string) (Value, error) {
	return Value{Quality: "good"}, nil
}
func (f *fakeChannelCounting) BatchRead(_ context.Context, _ []*platform.Point) (map[string]Value, error) {
	return nil, nil
}
func (f *fakeChannelCounting) Write(_ context.Context, _ string, _ any, _ string) error {
	return nil
}
func (f *fakeChannelCounting) BatchWrite(_ context.Context, _ []WriteOp) error { return nil }
func (f *fakeChannelCounting) Connect(_ context.Context) error {
	f.connectCnt.Add(1)
	return nil
}
func (f *fakeChannelCounting) Reconnect(_ context.Context) error { return nil }
func (f *fakeChannelCounting) Disconnect() error {
	f.disconnectCnt.Add(1)
	return nil
}
func (f *fakeChannelCounting) HealthCheck(_ context.Context) error { return nil }
func (f *fakeChannelCounting) IsConnected() bool                   { return f.healthy }
func (f *fakeChannelCounting) ProbeActive(_ context.Context) error { return nil }
func (f *fakeChannelCounting) LastActiveAt() time.Time             { return time.Now() }
func (f *fakeChannelCounting) LastError() string                   { return "" }
func (f *fakeChannelCounting) LastErrorAt() time.Time              { return time.Time{} }
func (f *fakeChannelCounting) Stats() map[string]any               { return map[string]any{} }

// fakePluginCounting 返回一个固定 bizID 的 fakeChannelCounting。
type fakePluginCounting struct {
	cli *fakeChannelCounting
}

func (p *fakePluginCounting) Name() string                      { return "fake" }
func (p *fakePluginCounting) Version() string                   { return "0.1" }
func (p *fakePluginCounting) ConfigSchema() json.RawMessage     { return nil }
func (p *fakePluginCounting) Capabilities() Capability          { return Capability{} }
func (p *fakePluginCounting) ParseConfig(_ []byte) (any, error) { return nil, nil }
func (p *fakePluginCounting) Init(_ context.Context) error      { return nil }
func (p *fakePluginCounting) Close() error                      { return nil }
func (p *fakePluginCounting) CreateChannel(_ *platform.Channel) (ProtocolChannel, error) {
	return p.cli, nil
}

// === Phase 1.3 新增 ChannelRegistry 核心测试 ===

// TestChannelRegistry_AcquireRefCount 验证同一 channel 多次 Acquire 共享同一连接。
// 释放第 N 次时 refs != 0，不应 Disconnect。
func TestChannelRegistry_AcquireRefCount(t *testing.T) {
	Unregister("fake")
	cli := &fakeChannelCounting{healthy: true}
	Register(&fakePluginCounting{cli: cli})
	defer Unregister("fake")

	r := NewChannelRegistry()
	ch := &platform.Channel{ChannelBizID: "c1", Role: "collect", Type: "fake", Address: "test"}

	// 3 次 Acquire
	for i := 0; i < 3; i++ {
		_, err := r.Acquire(context.Background(), ch)
		if err != nil {
			t.Fatalf("Acquire #%d: %v", i, err)
		}
	}
	if cli.connectCnt.Load() != 1 {
		t.Errorf("expected 1 Connect, got %d", cli.connectCnt.Load())
	}

	// 释放 2 次，refs 应为 1
	r.Release("c1", cli)
	r.Release("c1", cli)
	if cli.disconnectCnt.Load() != 0 {
		t.Errorf("refs=1 时不应 Disconnect, got %d", cli.disconnectCnt.Load())
	}

	// 再 release 1 次，refs=0，**但有 5s 宽限期**，短时间 poll 不应 Disconnect
	r.Release("c1", cli)
	if cli.disconnectCnt.Load() != 0 {
		t.Errorf("disconnectGrace 期间不应立即 Disconnect, got %d", cli.disconnectCnt.Load())
	}
}

// TestChannelRegistry_ReleaseWrongCli 验证 Release 时 cli 不匹配直接返回。
// 防止"配置热重载竞态"：老 worker 持旧 cli 句柄，不能误减新 entry 引用计数。
func TestChannelRegistry_ReleaseWrongCli(t *testing.T) {
	Unregister("fake")
	cli1 := &fakeChannelCounting{healthy: true}
	Register(&fakePluginCounting{cli: cli1})
	defer Unregister("fake")

	r := NewChannelRegistry()
	ch := &platform.Channel{ChannelBizID: "c1", Role: "collect", Type: "fake", Address: "test"}
	_, err := r.Acquire(context.Background(), ch)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// 拿一个新的 cli 句柄（模拟热重载后 Acquire 拿新 cli，但旧 worker 仍持旧 cli）
	cli2 := &fakeChannelCounting{healthy: true}

	// 用 cli2 release → 应该 noop
	r.Release("c1", cli2)

	// 真实 cli 仍 1 ref，没有 disconnect
	if cli1.disconnectCnt.Load() != 0 {
		t.Errorf("wrong cli Release 不应影响正确 entry")
	}
}

// TestChannelRegistry_InvalidateClearsFailed 验证 Invalidate 同时清 failed 表。
// 设计要点：避免 Invalidate 后 failed 表仍展示旧错误。
func TestChannelRegistry_InvalidateClearsFailed(t *testing.T) {
	Unregister("fake-fail")
	Register(&fakePluginFail{shouldFail: true})
	defer Unregister("fake-fail")

	r := NewChannelRegistry()
	ch := &platform.Channel{ChannelBizID: "fail1", Role: "collect", Type: "fake-fail", Address: "x"}

	// 第一次 Acquire 失败 → failed 表有记录
	_, _ = r.Acquire(context.Background(), ch)
	if _, exists := r.failed["fail1"]; !exists {
		t.Fatal("expected failed entry after failed Acquire")
	}

	// Invalidate → failed 表清空
	r.Invalidate("fail1")
	if _, exists := r.failed["fail1"]; exists {
		t.Errorf("Invalidate 应清 failed 表")
	}
}

// TestChannelRegistry_CloseAll 清空所有 entries（无失败记录）。
func TestChannelRegistry_CloseAll(t *testing.T) {
	Unregister("fake")
	cli := &fakeChannelCounting{healthy: true}
	Register(&fakePluginCounting{cli: cli})
	defer Unregister("fake")

	r := NewChannelRegistry()
	ch := &platform.Channel{ChannelBizID: "c1", Role: "collect", Type: "fake", Address: "x"}
	_, _ = r.Acquire(context.Background(), ch)

	r.CloseAll()
	if len(r.entries) != 0 {
		t.Errorf("CloseAll 后 entries 应清空, got %d", len(r.entries))
	}
	// CloseAll 同步调用 Disconnect（不同于 Release 的延迟断开）
	if cli.disconnectCnt.Load() != 1 {
		t.Errorf("CloseAll 应同步 Disconnect, got %d", cli.disconnectCnt.Load())
	}
}

// TestChannelRegistry_AcquireNilChannel 验证 nil channel 返回错误而非 panic。
func TestChannelRegistry_AcquireNilChannel(t *testing.T) {
	r := NewChannelRegistry()
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("nil channel 不应 panic: %v", rec)
		}
	}()
	_, err := r.Acquire(context.Background(), nil)
	if err == nil {
		t.Errorf("nil channel 应返回 error")
	}
}

// TestChannelRegistry_AcquireUnknownType 验证协议未注册时返回错误。
func TestChannelRegistry_AcquireUnknownType(t *testing.T) {
	r := NewChannelRegistry()
	ch := &platform.Channel{ChannelBizID: "c1", Role: "collect", Type: "nonexistent"}
	_, err := r.Acquire(context.Background(), ch)
	if err == nil {
		t.Errorf("未注册协议应返回 error")
	}
}

// === 辅助：失败 fake plugin ===

type fakePluginFail struct {
	shouldFail bool
}

func (p *fakePluginFail) Name() string                      { return "fake-fail" }
func (p *fakePluginFail) Version() string                   { return "0.1" }
func (p *fakePluginFail) ConfigSchema() json.RawMessage     { return nil }
func (p *fakePluginFail) Capabilities() Capability          { return Capability{} }
func (p *fakePluginFail) ParseConfig(_ []byte) (any, error) { return nil, nil }
func (p *fakePluginFail) Init(_ context.Context) error      { return nil }
func (p *fakePluginFail) Close() error                      { return nil }
func (p *fakePluginFail) CreateChannel(_ *platform.Channel) (ProtocolChannel, error) {
	return &fakeChannelFailing{shouldFail: p.shouldFail}, nil
}

type fakeChannelFailing struct {
	shouldFail bool
}

func (f *fakeChannelFailing) Read(_ context.Context, _ string, _ string) (Value, error) {
	return Value{}, errors.New("nope")
}
func (f *fakeChannelFailing) BatchRead(_ context.Context, _ []*platform.Point) (map[string]Value, error) {
	return nil, nil
}
func (f *fakeChannelFailing) Write(_ context.Context, _ string, _ any, _ string) error {
	return nil
}
func (f *fakeChannelFailing) BatchWrite(_ context.Context, _ []WriteOp) error { return nil }
func (f *fakeChannelFailing) Connect(_ context.Context) error {
	if f.shouldFail {
		return errors.New("connect failed")
	}
	return nil
}
func (f *fakeChannelFailing) Reconnect(_ context.Context) error { return nil }
func (f *fakeChannelFailing) Disconnect() error                 { return nil }
func (f *fakeChannelFailing) HealthCheck(_ context.Context) error {
	return nil
}
func (f *fakeChannelFailing) IsConnected() bool { return !f.shouldFail }
func (f *fakeChannelFailing) ProbeActive(_ context.Context) error {
	return nil
}
func (f *fakeChannelFailing) LastActiveAt() time.Time {
	return time.Now()
}
func (f *fakeChannelFailing) LastError() string {
	if f.shouldFail {
		return "connect failed"
	}
	return ""
}
func (f *fakeChannelFailing) LastErrorAt() time.Time { return time.Time{} }
func (f *fakeChannelFailing) Stats() map[string]any  { return map[string]any{} }
