// Plan3 3.4：GET /api/v1/targets/:biz_id/diag 单元测试。
//
// 覆盖 2026-10-21 修复的一个 bug：
//
//	forward.Workers.TargetSnapshot() 返回的是 {"targets":[...]} 对象，
//	而 targetDiag 之前按裸数组 []map[string]any 解包，
//	导致每次都 500 "cannot unmarshal object into Go value of type []map[string]interface {}"。
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/auth"
	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/infra/target"
)

// fakeForwardWorkers 是 forwardWorkersSnapshotter 的测试替身。
type fakeForwardWorkers struct {
	snapshot []byte
}

func (f *fakeForwardWorkers) Snapshot() map[string]protocol.ChannelSnapshot {
	return map[string]protocol.ChannelSnapshot{}
}

func (f *fakeForwardWorkers) TargetSnapshot() []byte { return f.snapshot }

func (f *fakeForwardWorkers) QueryHistory(ctx context.Context, targetBizID string, q target.HistoryQuery) (any, error) {
	return nil, nil
}

// newTestServerWithForward 构造带 forward workers 的 Server。
func newTestServerWithForward(t *testing.T, fw forwardWorkersSnapshotter) http.Handler {
	t.Helper()
	store := config.NewStore("")
	pools := pool.NewPools(pool.Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})
	srv := NewServer(store, pools, ":0")
	srv.SetForwardWorkers(fw)
	return srv.routes()
}

// authedCoreReq 构造带 Bearer Token 的请求（core 的 /api/v1/* 需要鉴权）。
func authedCoreReq(t *testing.T, method, path string) *http.Request {
	t.Helper()
	tok, err := auth.ServiceToken(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	return req
}

// TestTargetDiag_UnwrapsTargetsObject 验证 diag 能正确解包 {"targets":[...]}。
//
// 修复前：500 "cannot unmarshal object into Go value of type []map[string]interface {}"
func TestTargetDiag_UnwrapsTargetsObject(t *testing.T) {
	snapshot := []byte(`{"targets":[{"target_biz_id":"influxdb","type":"json","connected":false,
		"stats":{"protocol":"influxdb"},"queue_len":0,"queue_cap":10000}]}`)
	r := newTestServerWithForward(t, &fakeForwardWorkers{snapshot: snapshot})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedCoreReq(t, http.MethodGet, "/api/v1/targets/influxdb/diag"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["target_biz_id"] != "influxdb" {
		t.Errorf("target_biz_id = %v, want influxdb", got["target_biz_id"])
	}
}

// TestTargetDiag_NotFound 验证找不到时返回 404（不是 500）。
func TestTargetDiag_NotFound(t *testing.T) {
	snapshot := []byte(`{"targets":[{"target_biz_id":"influxdb"}]}`)
	r := newTestServerWithForward(t, &fakeForwardWorkers{snapshot: snapshot})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedCoreReq(t, http.MethodGet, "/api/v1/targets/nope/diag"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body: %s", w.Code, w.Body.String())
	}
}

// TestTargetsDiag_Passthrough 验证全量 diag 原样透传。
func TestTargetsDiag_Passthrough(t *testing.T) {
	snapshot := []byte(`{"targets":[{"target_biz_id":"a"},{"target_biz_id":"b"}]}`)
	r := newTestServerWithForward(t, &fakeForwardWorkers{snapshot: snapshot})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedCoreReq(t, http.MethodGet, "/api/v1/targets/diag"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", w.Code, w.Body.String())
	}
	var got struct {
		Targets []map[string]any `json:"targets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Targets) != 2 {
		t.Errorf("targets 数量 = %d, want 2", len(got.Targets))
	}
}
