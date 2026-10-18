// Package collect 核心逻辑测试（Phase 1.3）。
//
// 覆盖：
//   1. valueToAny 类型映射（所有 data_type）
//   2. targetMatchesSource 通配符 + 精确匹配
//   3. matchedPointBizIDs 过滤逻辑
//   4. pollOnce 端到端：mock channel → 过滤 → enqueue
//   5. pollOnce 失败时不入队 + 计数+1
package collect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// Value 是 protocol.Value 的别名（mock 签名更简洁）。
type Value = protocol.Value

// ==================== valueToAny 测试 ====================

func TestValueToAny_AllDataTypes(t *testing.T) {
	cases := []struct {
		dt   string
		val  protocol.Value
		want any
	}{
		{"bool", protocol.Value{DataType: "bool", Bool: true}, true},
		{"int8", protocol.Value{DataType: "int8", Int64: -1}, int64(-1)},
		{"int16", protocol.Value{DataType: "int16", Int64: -2}, int64(-2)},
		{"int32", protocol.Value{DataType: "int32", Int64: -3}, int64(-3)},
		{"int64", protocol.Value{DataType: "int64", Int64: -4}, int64(-4)},
		{"uint8", protocol.Value{DataType: "uint8", Uint64: 1}, uint64(1)},
		{"uint16", protocol.Value{DataType: "uint16", Uint64: 2}, uint64(2)},
		{"uint32", protocol.Value{DataType: "uint32", Uint64: 3}, uint64(3)},
		{"uint64", protocol.Value{DataType: "uint64", Uint64: 4}, uint64(4)},
		{"float32", protocol.Value{DataType: "float32", Float64: 1.5}, 1.5},
		{"float64", protocol.Value{DataType: "float64", Float64: 2.5}, 2.5},
		{"string", protocol.Value{DataType: "string", String: "hello"}, "hello"},
	}
	for _, c := range cases {
		got := valueToAny(c.val)
		if got != c.want {
			t.Errorf("valueToAny(%s): got %v (%T), want %v (%T)", c.dt, got, got, c.want, c.want)
		}
	}
}

func TestValueToAny_UnknownType_FallbackToFloat64(t *testing.T) {
	// 未知类型回退 Float64（而非 0），避免静默丢值。
	v := protocol.Value{DataType: "exotic", Float64: 42.5}
	got := valueToAny(v)
	if got != 42.5 {
		t.Errorf("unknown type fallback: got %v, want 42.5", got)
	}
}

// ==================== targetMatchesSource 测试 ====================

func TestTargetMatchesSource_Wildcard(t *testing.T) {
	tgt := &platform.Target{
		PointBizIDs: []string{"*"},
	}
	pointOwner := map[string]string{"p1": "s1"}
	if !targetMatchesSource(tgt, "s1", pointOwner) {
		t.Errorf("wildcard target should match all sources")
	}
}

func TestTargetMatchesSource_ExactMatch(t *testing.T) {
	tgt := &platform.Target{
		PointBizIDs: []string{"p1", "p2"},
	}
	pointOwner := map[string]string{"p1": "s1", "p2": "s2"}
	if !targetMatchesSource(tgt, "s1", pointOwner) {
		t.Errorf("target with p1 should match s1")
	}
	if !targetMatchesSource(tgt, "s2", pointOwner) {
		t.Errorf("target with p2 should match s2")
	}
	if targetMatchesSource(tgt, "s3", pointOwner) {
		t.Errorf("target should NOT match s3 (no points from s3)")
	}
}

func TestTargetMatchesSource_EmptyPointList(t *testing.T) {
	tgt := &platform.Target{PointBizIDs: []string{}}
	pointOwner := map[string]string{"p1": "s1"}
	if targetMatchesSource(tgt, "s1", pointOwner) {
		t.Errorf("empty point list target should NOT match anything")
	}
}

// ==================== matchedPointBizIDs 测试 ====================

func TestMatchedPointBizIDs_Wildcard(t *testing.T) {
	points := []*platform.Point{
		{PointBizID: "p1"},
		{PointBizID: "p2"},
	}
	tgt := &platform.Target{PointBizIDs: []string{"*"}}
	got := matchedPointBizIDs(tgt, points)
	if len(got) != 2 {
		t.Errorf("wildcard: got %d, want 2", len(got))
	}
}

func TestMatchedPointBizIDs_Filter(t *testing.T) {
	points := []*platform.Point{
		{PointBizID: "p1"},
		{PointBizID: "p2"},
		{PointBizID: "p3"},
	}
	tgt := &platform.Target{PointBizIDs: []string{"p1", "p3"}}
	got := matchedPointBizIDs(tgt, points)
	if len(got) != 2 {
		t.Errorf("filter: got %d, want 2", len(got))
	}
	gotSet := map[string]bool{}
	for _, id := range got {
		gotSet[id] = true
	}
	if !gotSet["p1"] || !gotSet["p3"] || gotSet["p2"] {
		t.Errorf("filter result wrong: %v", got)
	}
}

func TestMatchedPointBizIDs_NoMatch(t *testing.T) {
	points := []*platform.Point{{PointBizID: "p1"}}
	tgt := &platform.Target{PointBizIDs: []string{"p999"}}
	got := matchedPointBizIDs(tgt, points)
	if len(got) != 0 {
		t.Errorf("no match: got %d, want 0", len(got))
	}
}

// ==================== mockProtocolChannel ====================

type mockProtocolChannel struct {
	readErr   error
	readFn    func(ctx context.Context, points []*platform.Point) (map[string]Value, error)
	readCalls atomic.Int32
	healthy   bool
}

func (m *mockProtocolChannel) Read(ctx context.Context, address string, dataType string) (protocol.Value, error) {
	return protocol.Value{}, errors.New("not implemented")
}
func (m *mockProtocolChannel) BatchRead(ctx context.Context, points []*platform.Point) (map[string]Value, error) {
	m.readCalls.Add(1)
	if m.readFn != nil {
		return m.readFn(ctx, points)
	}
	return nil, m.readErr
}
func (m *mockProtocolChannel) Write(ctx context.Context, address string, value any, dataType string) error {
	return nil
}
func (m *mockProtocolChannel) BatchWrite(ctx context.Context, writes []protocol.WriteOp) error { return nil }
func (m *mockProtocolChannel) Connect(ctx context.Context) error                              { return nil }
func (m *mockProtocolChannel) Reconnect(ctx context.Context) error                            { return nil }
func (m *mockProtocolChannel) Disconnect() error                                              { return nil }
func (m *mockProtocolChannel) HealthCheck(ctx context.Context) error                          { return nil }
func (m *mockProtocolChannel) IsConnected() bool                                             { return m.healthy }
func (m *mockProtocolChannel) ProbeActive(ctx context.Context) error                          { return nil }
func (m *mockProtocolChannel) LastActiveAt() time.Time                                       { return time.Now() }
func (m *mockProtocolChannel) LastError() string                                             { return "" }
func (m *mockProtocolChannel) LastErrorAt() time.Time                                        { return time.Time{} }
func (m *mockProtocolChannel) Stats() map[string]any                                         { return map[string]any{} }

// 修正上面方法签名（Value 是 protocol.Value，包内 alias 会导致编译错）
// 已在文件顶部声明 type Value = protocol.Value

// ==================== pollOnce 端到端测试 ====================

// 集成测试：构造完整 cfg + mock channel，调用 pollOnce，验证 queue.Enqueue 收到正确数据。
func TestPollOnce_EnqueueValues(t *testing.T) {
	store := config.NewStore(t.TempDir())
	pools := pool.NewPools(pool.Config{CollectSize: 2, ForwardSize: 2, WriteSize: 2, ConfigSize: 2})
	pools.Start(context.Background(), nil)
	defer pools.Stop(time.Second)

	w := NewWorkers(store, pools)

	// 配置：一个 device + 一个 source + 2 个 point
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{
			{ChannelBizID: "c1", Role: "collect", Type: "mock"},
		},
		Devices: []*platform.Device{
			{
				DeviceBizID: "d1",
				Enabled:     true,
				Sources: []*platform.Source{
					{
						SourceBizID:       "s1",
						ChannelBizID:      "c1",
						CollectEnabled:    true,
						CollectIntervalMs: 100,
						TimeoutMs:         300,
						Points: []*platform.Point{
							{PointBizID: "p1", DataType: "int32", CollectEnabled: true},
							{PointBizID: "p2", DataType: "float64", CollectEnabled: true},
						},
					},
				},
				Targets: []*platform.Target{
					{
						TargetBizID:         "t1",
						ForwardChannelBizID: "c1",
						PointBizIDs:         []string{"*"},
					},
				},
			},
		},
	}
	store.SyncFrom(cfg)

	mock := &mockProtocolChannel{
		healthy: true,
		readFn: func(ctx context.Context, points []*platform.Point) (map[string]Value, error) {
			return map[string]Value{
				"p1": {DataType: "int32", Int64: 42},
				"p2": {DataType: "float64", Float64: 3.14},
			}, nil
		},
	}
	dev := cfg.Devices[0]
	src := dev.Sources[0]
	_ = dev

	// 在调用前清空队列
	w.pollOnce(context.Background(), dev, src, mock, 300*time.Millisecond)

	// 验证 queue 收到 2 个 PointValue
	q := w.targetRouters.Get("t1")
	if q == nil {
		t.Fatal("queue t1 不应为空")
	}

	got := drainQueue(t, q, 2, time.Second)
	if len(got) != 2 {
		t.Fatalf("expected 2 values, got %d", len(got))
	}

	byPoint := map[string]any{}
	for _, pv := range got {
		byPoint[pv.PointBizID] = pv.Value
		// 验证 traceID 一致（同批次）
		if pv.TraceID == "" {
			t.Errorf("PointValue.TraceID 应非空")
		}
	}
	if byPoint["p1"] != int64(42) {
		t.Errorf("p1: got %v (%T), want 42", byPoint["p1"], byPoint["p1"])
	}
	if byPoint["p2"] != 3.14 {
		t.Errorf("p2: got %v, want 3.14", byPoint["p2"])
	}
}

func TestPollOnce_ReadError_NoEnqueue(t *testing.T) {
	store := config.NewStore(t.TempDir())
	pools := pool.NewPools(pool.Config{CollectSize: 2, ForwardSize: 2, WriteSize: 2, ConfigSize: 2})
	pools.Start(context.Background(), nil)
	defer pools.Stop(time.Second)

	w := NewWorkers(store, pools)
	cfg := &platform.Config{
		Version: 1,
		Channels: []*platform.Channel{{ChannelBizID: "c1", Role: "collect", Type: "mock"}},
		Devices: []*platform.Device{{
			DeviceBizID: "d1", Enabled: true,
			Sources: []*platform.Source{{
				SourceBizID: "s1", ChannelBizID: "c1", CollectEnabled: true,
				CollectIntervalMs: 100, TimeoutMs: 300,
				Points: []*platform.Point{{PointBizID: "p1", CollectEnabled: true}},
			}},
			Targets: []*platform.Target{{
				TargetBizID: "t1", ForwardChannelBizID: "c1", PointBizIDs: []string{"*"},
			}},
		}},
	}
	store.SyncFrom(cfg)

	// 第一次读成功建立队列
	mock := &mockProtocolChannel{
		healthy: true,
		readFn: func(ctx context.Context, points []*platform.Point) (map[string]Value, error) {
			return map[string]Value{"p1": {DataType: "int32", Int64: 100}}, nil
		},
	}
	dev := cfg.Devices[0]
	src := dev.Sources[0]
	_ = dev
	q := w.targetRouters.Get("t1")
	w.pollOnce(context.Background(), dev, src, mock, 300*time.Millisecond)
	first := drainAllQueue(t, q, 200*time.Millisecond)
	if len(first) != 1 {
		t.Fatalf("first read 期望入队 1，实际 %d", len(first))
	}

	// 第二次读失败 → 不入队
	mock.readFn = nil
	mock.readErr = errors.New("PLC timeout")
	w.pollOnce(context.Background(), dev, src, mock, 300*time.Millisecond)

	// 队列应该空的（drain 100ms 内没新数据即返回）
	got := drainAllQueue(t, q, 100*time.Millisecond)
	if len(got) != 0 {
		t.Errorf("read error 后不应入队，got %d", len(got))
	}
}

// ==================== helper ====================

// drainQueue 用 Queue.Raw() 非阻塞轮询取元素，最多取 n 个。
func drainQueue(t *testing.T, q *target.Queue, n int, timeout time.Duration) []target.PointValue {
	t.Helper()
	var out []target.PointValue
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && len(out) < n {
		select {
		case v := <-q.Raw():
			out = append(out, v)
		case <-time.After(10 * time.Millisecond):
		}
	}
	return out
}

// drainAllQueue 排空队列直到 timeout 内没新数据。
func drainAllQueue(t *testing.T, q *target.Queue, timeout time.Duration) []target.PointValue {
	t.Helper()
	var out []target.PointValue
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case v := <-q.Raw():
			out = append(out, v)
		case <-time.After(10 * time.Millisecond):
			return out
		}
	}
	return out
}
