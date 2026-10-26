// Package collect 核心逻辑测试（Phase 1.3）。
//
// 覆盖：
//  1. Value.AsGoNative 类型映射（所有 data_type）
//  2. targetMatchesSource 通配符 + 精确匹配
//  3. matchedPointBizIDs 过滤逻辑
//  4. pollOnce 端到端：mock channel → 过滤 → enqueue
//  5. pollOnce 失败时不入队 + 计数+1
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

// ==================== AsGoNative 测试 ====================

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
		{"bytes", protocol.Value{DataType: "bytes", Bytes: []byte{0x01, 0x02}}, []byte{0x01, 0x02}},
	}
	for _, c := range cases {
		got := c.val.AsGoNative()
		// bytes 是 slice，不可直接用 == 比较（会 panic: comparing uncomparable type）。
		if c.dt == "bytes" {
			gotBytes, ok1 := got.([]byte)
			wantBytes, ok2 := c.want.([]byte)
			if !ok1 || !ok2 || len(gotBytes) != len(wantBytes) {
				t.Errorf("AsGoNative(%s): got %v (%T), want %v (%T)", c.dt, got, got, c.want, c.want)
				continue
			}
			for i := range gotBytes {
				if gotBytes[i] != wantBytes[i] {
					t.Errorf("AsGoNative(%s): got %v, want %v", c.dt, gotBytes, wantBytes)
					break
				}
			}
			continue
		}
		if got != c.want {
			t.Errorf("AsGoNative(%s): got %v (%T), want %v (%T)", c.dt, got, got, c.want, c.want)
		}
	}
}

func TestValueToAny_UnknownType_ReturnsNil(t *testing.T) {
	// 未知类型在 Plan2 主题 C 5.1 中改为返回 nil（业务层做降级 / 丢日志）。
	v := protocol.Value{DataType: "exotic", Float64: 42.5}
	got := v.AsGoNative()
	if got != nil {
		t.Errorf("unknown type should fallback to nil: got %v", got)
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
func (m *mockProtocolChannel) BatchWrite(ctx context.Context, writes []protocol.WriteOp) error {
	return nil
}
func (m *mockProtocolChannel) Connect(ctx context.Context) error     { return nil }
func (m *mockProtocolChannel) Reconnect(ctx context.Context) error   { return nil }
func (m *mockProtocolChannel) Disconnect() error                     { return nil }
func (m *mockProtocolChannel) HealthCheck(ctx context.Context) error { return nil }
func (m *mockProtocolChannel) IsConnected() bool                     { return m.healthy }
func (m *mockProtocolChannel) ProbeActive(ctx context.Context) error { return nil }
func (m *mockProtocolChannel) LastActiveAt() time.Time               { return time.Now() }
func (m *mockProtocolChannel) LastError() string                     { return "" }
func (m *mockProtocolChannel) LastErrorAt() time.Time                { return time.Time{} }
func (m *mockProtocolChannel) Stats() map[string]any                 { return map[string]any{} }

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
		Version:  1,
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

// ==================== SourceHealth 测试（Plan3 3.2） ====================

// newHealthTestWorkers 建一个带单个 Source / 单点位 / 单 Target 的 workers。
func newHealthTestWorkers(t *testing.T) (*Workers, *platform.Device, *platform.Source, *mockProtocolChannel) {
	t.Helper()
	store := config.NewStore(t.TempDir())
	pools := pool.NewPools(pool.Config{CollectSize: 2, ForwardSize: 2, WriteSize: 2, ConfigSize: 2})
	pools.Start(context.Background(), nil)
	t.Cleanup(func() { pools.Stop(time.Second) })

	w := NewWorkers(store, pools)
	cfg := &platform.Config{
		Version:  1,
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
	mock := &mockProtocolChannel{healthy: true}
	return w, cfg.Devices[0], cfg.Devices[0].Sources[0], mock
}

// TestPollOnce_MarksHealthOK 成功采集后健康度应为 ok，并记下 last_success_ms。
func TestPollOnce_MarksHealthOK(t *testing.T) {
	w, dev, src, mock := newHealthTestWorkers(t)
	mock.readFn = func(ctx context.Context, points []*platform.Point) (map[string]Value, error) {
		return map[string]Value{"p1": {DataType: "int32", Int64: 7}}, nil
	}
	w.pollOnce(context.Background(), dev, src, mock, 300*time.Millisecond)

	h, ok := w.SourceHealthSnapshot()["s1"]
	if !ok {
		t.Fatal("成功采集后应有 s1 的健康度记录")
	}
	if h.Status != SourceHealthOK {
		t.Errorf("status = %q, want %q", h.Status, SourceHealthOK)
	}
	if h.LastSuccessMs == 0 {
		t.Error("LastSuccessMs 应为非 0")
	}
	if h.ConsecutiveErrors != 0 {
		t.Errorf("成功一次后 ConsecutiveErrors 应归零，实际 %d", h.ConsecutiveErrors)
	}
	if h.IntervalMs != src.CollectIntervalMs {
		t.Errorf("IntervalMs = %d, want %d", h.IntervalMs, src.CollectIntervalMs)
	}
}

// TestPollOnce_FailureKeepsStaleValue 核心行为：
// 读失败后缓存**保留旧值**（不清空），但健康度标记为 error，
// 且 last_success_ms 仍指向最后一次成功的时间 —— 前端据此判断"这是旧值"。
func TestPollOnce_FailureKeepsStaleValue(t *testing.T) {
	w, dev, src, mock := newHealthTestWorkers(t)
	mock.readFn = func(ctx context.Context, points []*platform.Point) (map[string]Value, error) {
		return map[string]Value{"p1": {DataType: "int32", Int64: 100}}, nil
	}
	w.pollOnce(context.Background(), dev, src, mock, 300*time.Millisecond)

	successAt := w.SourceHealthSnapshot()["s1"].LastSuccessMs
	cachedAt := w.PointValueCache()["s1"]["p1"].Timestamp

	// 连续失败 3 次
	mock.readFn = nil
	mock.readErr = errors.New("PLC timeout")
	for i := 0; i < 3; i++ {
		w.pollOnce(context.Background(), dev, src, mock, 300*time.Millisecond)
	}

	h := w.SourceHealthSnapshot()["s1"]
	if h.Status != SourceHealthError {
		t.Errorf("status = %q, want %q", h.Status, SourceHealthError)
	}
	if h.ConsecutiveErrors != 3 {
		t.Errorf("ConsecutiveErrors = %d, want 3", h.ConsecutiveErrors)
	}
	if h.LastSuccessMs != successAt {
		t.Errorf("失败不应改写 LastSuccessMs：got %d, want %d", h.LastSuccessMs, successAt)
	}
	if h.LastError == "" {
		t.Error("LastError 应记录错误描述")
	}

	// 关键：旧值仍在缓存里（未被清空），但时间戳仍是上次成功的那次
	vals := w.PointValueCache()["s1"]
	if len(vals) != 1 {
		t.Fatalf("失败后缓存应仍保留 1 个旧值，实际 %d", len(vals))
	}
	if got := vals["p1"].Value; got != int64(100) {
		t.Errorf("旧值应保留，实际 %v", got)
	}
	if vals["p1"].Timestamp != cachedAt {
		t.Errorf("旧值时间戳不应被失败刷新：got %d, want %d", vals["p1"].Timestamp, cachedAt)
	}
}

// TestPollOnce_RecoveryResetsErrorCount 失败后恢复成功，计数归零、status 回 ok。
func TestPollOnce_RecoveryResetsErrorCount(t *testing.T) {
	w, dev, src, mock := newHealthTestWorkers(t)
	mock.readFn = func(ctx context.Context, points []*platform.Point) (map[string]Value, error) {
		return map[string]Value{"p1": {DataType: "int32", Int64: 1}}, nil
	}
	w.pollOnce(context.Background(), dev, src, mock, 300*time.Millisecond)

	mock.readFn = nil
	mock.readErr = errors.New("boom")
	w.pollOnce(context.Background(), dev, src, mock, 300*time.Millisecond)
	if got := w.SourceHealthSnapshot()["s1"].ConsecutiveErrors; got != 1 {
		t.Fatalf("ConsecutiveErrors = %d, want 1", got)
	}

	mock.readFn = func(ctx context.Context, points []*platform.Point) (map[string]Value, error) {
		return map[string]Value{"p1": {DataType: "int32", Int64: 2}}, nil
	}
	mock.readErr = nil
	w.pollOnce(context.Background(), dev, src, mock, 300*time.Millisecond)

	h := w.SourceHealthSnapshot()["s1"]
	if h.Status != SourceHealthOK {
		t.Errorf("status = %q, want %q", h.Status, SourceHealthOK)
	}
	if h.ConsecutiveErrors != 0 {
		t.Errorf("恢复后 ConsecutiveErrors 应为 0，实际 %d", h.ConsecutiveErrors)
	}
	if h.LastError != "" {
		t.Errorf("恢复后 LastError 应清空，实际 %q", h.LastError)
	}
}

// TestHealthSnapshot_IsDeepCopy 快照必须是副本：调用方改动不影响内部状态。
func TestHealthSnapshot_IsDeepCopy(t *testing.T) {
	w, dev, src, mock := newHealthTestWorkers(t)
	mock.readFn = func(ctx context.Context, points []*platform.Point) (map[string]Value, error) {
		return map[string]Value{"p1": {DataType: "int32", Int64: 1}}, nil
	}
	w.pollOnce(context.Background(), dev, src, mock, 300*time.Millisecond)

	snap := w.SourceHealthSnapshot()
	snap["s1"] = SourceHealth{Status: SourceHealthError, ConsecutiveErrors: 999}

	if got := w.SourceHealthSnapshot()["s1"]; got.Status != SourceHealthOK {
		t.Errorf("改动快照污染了内部状态：status = %q", got.Status)
	}
}

// TestPruneRuntimeState 验证配置重载后，已删除 Source 的缓存/健康度被清理，
// 而仍存在的 Source 保留 —— 否则 /values 会返回幽灵数据。
func TestPruneRuntimeState(t *testing.T) {
	w, dev, src, mock := newHealthTestWorkers(t)
	mock.readFn = func(ctx context.Context, points []*platform.Point) (map[string]Value, error) {
		return map[string]Value{"p1": {DataType: "int32", Int64: 1}}, nil
	}
	w.pollOnce(context.Background(), dev, src, mock, 300*time.Millisecond)

	// 手工塞入一个配置里已不存在的 Source（模拟"删了设备但缓存没清"）
	w.pointCacheMutex.Lock()
	w.pointCache["ghost"] = map[string]CachedPointValue{"g1": {PointBizID: "g1"}}
	w.sourceHealth["ghost"] = &SourceHealth{SourceBizID: "ghost", Status: SourceHealthOK}
	w.pointCacheMutex.Unlock()

	cfg := &platform.Config{
		Version:  2,
		Channels: []*platform.Channel{{ChannelBizID: "c1", Role: "collect", Type: "mock"}},
		Devices: []*platform.Device{{
			DeviceBizID: "d1", Enabled: true,
			Sources: []*platform.Source{{SourceBizID: "s1", ChannelBizID: "c1", CollectEnabled: true}},
		}},
	}
	w.pruneRuntimeState(cfg)

	if _, ok := w.PointValueCache()["ghost"]; ok {
		t.Error("已删除 Source 的缓存应被清理")
	}
	if _, ok := w.SourceHealthSnapshot()["ghost"]; ok {
		t.Error("已删除 Source 的健康度应被清理")
	}
	if _, ok := w.PointValueCache()["s1"]; !ok {
		t.Error("仍存在的 Source 缓存应保留")
	}

	// 停用的 Device 其 Source 也应被清理（否则界面一直显示旧值）
	_ = dev
	cfg.Devices[0].Enabled = false
	w.pruneRuntimeState(cfg)
	if _, ok := w.PointValueCache()["s1"]; ok {
		t.Error("Device 停用后其 Source 缓存应被清理")
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
