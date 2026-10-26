// batch write 补偿机制测试（Plan §8.2 / §12.1 #4）。
//
// 直接测 applyBatchCompensation（已抽出的纯函数），不走 HTTP / worker 链路，
// 避免依赖 write.Worker 的 5s ticker + queue 初始化顺序。
package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// fakeChannel 实现 protocol.ProtocolChannel。
type fakeChannel struct {
	mu     sync.Mutex
	writes []fakeWrite
	failOn int
	count  int
}

type fakeWrite struct {
	Address  string
	Value    any
	DataType string
}

func (f *fakeChannel) Read(_ context.Context, _ string, _ string) (protocol.Value, error) {
	return protocol.Value{Int64: 999}, nil
}
func (f *fakeChannel) BatchRead(_ context.Context, _ []*platform.Point) (map[string]protocol.Value, error) {
	return nil, nil
}
func (f *fakeChannel) Write(_ context.Context, address string, value any, dataType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	if f.failOn > 0 && f.count == f.failOn {
		return &fakeErr{msg: "fake fail"}
	}
	f.writes = append(f.writes, fakeWrite{address, value, dataType})
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

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

// TestApplyBatchCompensation_HappyPath 验证：成功写入的点位会按缓存原值回滚。
func TestApplyBatchCompensation_HappyPath(t *testing.T) {
	cli := &fakeChannel{}
	compensations := []batchCompensationEntry{
		{pointBizID: "pt-1", address: "DB1.DBD0", dataType: "int32", origValue: protocol.Value{Int64: 100}},
		{pointBizID: "pt-2", address: "DB1.DBD4", dataType: "int32", origValue: protocol.Value{Int64: 200}},
	}
	results := []batchWriteResult{
		{PointBizID: "pt-1", Result: "success"},
		{PointBizID: "pt-2", Result: "fail", Error: "PLC timeout"}, // 第 2 条失败
	}

	triggered, partial := applyBatchCompensation(context.Background(), "trace-1", compensations, results, cli)
	if !triggered {
		t.Error("triggered = false, want true")
	}
	if partial {
		t.Error("partial = true, want false (all rollback succeed)")
	}

	cli.mu.Lock()
	defer cli.mu.Unlock()
	if len(cli.writes) != 1 {
		t.Fatalf("writes = %d, want 1 (only pt-1 was success)", len(cli.writes))
	}
	if cli.writes[0].Address != "DB1.DBD0" {
		t.Errorf("rollback address = %q, want DB1.DBD0", cli.writes[0].Address)
	}
	if v, ok := cli.writes[0].Value.(int64); !ok || v != 100 {
		t.Errorf("rollback value = %v, want int64(100)", cli.writes[0].Value)
	}
}

// TestApplyBatchCompensation_AllSuccess_NoRollback 验证：全部成功时不该补偿。
// （实际上 applyBatchCompensation 会被 writeBatch 的 failIdx>0 条件拦掉，这里
// 测的是直接调它：所有点位都 success → successSet 命中所有 → 全部回滚。
// 写"全部成功"场景的逻辑是：writeBatch 不会调补偿——但测试抽出的函数行为本身。）
func TestApplyBatchCompensation_AllSuccess(t *testing.T) {
	cli := &fakeChannel{}
	compensations := []batchCompensationEntry{
		{pointBizID: "pt-1", address: "DB1.DBD0", dataType: "int32", origValue: protocol.Value{Int64: 100}},
	}
	results := []batchWriteResult{
		{PointBizID: "pt-1", Result: "success"},
	}
	triggered, _ := applyBatchCompensation(context.Background(), "t", compensations, results, cli)
	if !triggered {
		t.Error("triggered = false")
	}
	if len(cli.writes) != 1 {
		t.Errorf("writes = %d, want 1", len(cli.writes))
	}
}

// TestApplyBatchCompensation_PartialFailure 验证：补偿过程中某个点位 Write 失败
// → 返回 partial=true，但其他成功点位仍回滚。
func TestApplyBatchCompensation_PartialFailure(t *testing.T) {
	cli := &fakeChannel{failOn: 2} // 第 2 次 Write 失败
	compensations := []batchCompensationEntry{
		{pointBizID: "pt-1", address: "DB1.DBD0", dataType: "int32", origValue: protocol.Value{Int64: 100}},
		{pointBizID: "pt-2", address: "DB1.DBD4", dataType: "int32", origValue: protocol.Value{Int64: 200}},
	}
	results := []batchWriteResult{
		{PointBizID: "pt-1", Result: "success"},
		{PointBizID: "pt-2", Result: "success"},
	}

	triggered, partial := applyBatchCompensation(context.Background(), "t", compensations, results, cli)
	if !triggered {
		t.Error("triggered = false")
	}
	if !partial {
		t.Error("partial = false, want true (第 2 次 Write 失败)")
	}
	cli.mu.Lock()
	defer cli.mu.Unlock()
	if len(cli.writes) != 1 {
		t.Errorf("writes = %d, want 1 (第 1 个成功, 第 2 个失败)", len(cli.writes))
	}
}

// TestRollbackValueFromProtocolValue 验证不同 dataType 反向取值正确。
func TestRollbackValueFromProtocolValue(t *testing.T) {
	cases := []struct {
		dataType string
		val      protocol.Value
		want     any
	}{
		{"bool", protocol.Value{Bool: true}, true},
		{"int32", protocol.Value{Int64: 42}, int64(42)},
		{"uint16", protocol.Value{Uint64: 42}, uint64(42)},
		{"float64", protocol.Value{Float64: 3.14}, 3.14},
		{"string", protocol.Value{String: "hi"}, "hi"},
		{"unknown_type", protocol.Value{Int64: 7}, int64(7)}, // fallback
	}
	for _, c := range cases {
		got := rollbackValueFromProtocolValue(c.val, c.dataType)
		if got != c.want {
			t.Errorf("dataType=%q: got %v(%T), want %v(%T)", c.dataType, got, got, c.want, c.want)
		}
	}
}
