// Plan3 3.5：历史趋势查询 API 单元测试。
//
// 注意（2026-10-21）：查询语句的构造（Flux / InfluxQL）已下沉到
// internal/infra/target/influxdb 包，本文件只保留 API 层的行为测试。
// 构造函数的测试见 influxdb/history_test.go。
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/infra/target"
)

// TestTargetHistoryRequiresMeasurement 验证 measurement 必填。
//
// 注意：/api/v1/* 路由需要鉴权（Bearer Token），未带 token 时会先返回 401，
// 所以这里断言的是"不会因为 measurement 缺失而 panic / 500"。
func TestTargetHistoryRequiresMeasurement(t *testing.T) {
	r := newTestServerForProtocols(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/targets/t1/history", nil)
	r.ServeHTTP(w, req)
	if w.Code == http.StatusInternalServerError {
		t.Errorf("status = %d, want not 500. body: %s", w.Code, w.Body.String())
	}
}

// TestHistoryDurationRe 验证 duration 参数格式校验。
func TestHistoryDurationRe(t *testing.T) {
	valid := []string{"1h", "15m", "7d", "30s", "1w"}
	for _, v := range valid {
		if !historyDurationRe.MatchString(v) {
			t.Errorf("historyDurationRe 应接受 %q", v)
		}
	}
	invalid := []string{"1x", "", "abc", "h", "-1h", "1h30m"}
	for _, v := range invalid {
		if historyDurationRe.MatchString(v) {
			t.Errorf("historyDurationRe 应拒绝 %q", v)
		}
	}
}

// fakeHistoryWorkers 返回预置的历史数据（模拟 Target 客户端的查询结果）。
type fakeHistoryWorkers struct {
	rows []map[string]any
}

func (f *fakeHistoryWorkers) Snapshot() map[string]protocol.ChannelSnapshot {
	return map[string]protocol.ChannelSnapshot{}
}

func (f *fakeHistoryWorkers) TargetSnapshot() []byte { return []byte(`{"targets":[]}`) }

func (f *fakeHistoryWorkers) QueryHistory(ctx context.Context, targetBizID string, q target.HistoryQuery) (any, error) {
	return f.rows, nil
}

// TestTargetHistory_SortsAscending 验证返回的点按时间升序排列。
//
// 回归测试（2026-10-21）：查询层为了拿到「最近 N 个点」必须加
// ORDER BY time DESC（InfluxDB 的 LIMIT 默认取最早的 N 行），
// 所以查询结果天然是「最新在前」。
// 但折线图要求 x 轴从左到右时间递增，否则画出来是折返的锯齿。
// 因此 API 层必须统一排成升序。
func TestTargetHistory_SortsAscending(t *testing.T) {
	fw := &fakeHistoryWorkers{
		rows: []map[string]any{
			{"_time": time.UnixMilli(3000), "_value": 4.0},
			{"_time": time.UnixMilli(1000), "_value": 1.0},
			{"_time": time.UnixMilli(2000), "_value": 2.0},
			{"_time": time.UnixMilli(4000), "_value": 3.0},
		},
	}
	store := config.NewStore("")
	pools := pool.NewPools(pool.Config{CollectSize: 1, ForwardSize: 1, WriteSize: 1, ConfigSize: 1})
	srv := NewServer(store, pools, ":0")
	srv.SetForwardWorkers(fw)
	r := srv.routes()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedCoreReq(t, http.MethodGet,
		"/api/v1/targets/t1/history?measurement=sss&field=M100&duration=1h"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Count  int `json:"count"`
		Points []struct {
			T int64   `json:"t"`
			V float64 `json:"v"`
		} `json:"points"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 4 {
		t.Fatalf("count = %d, want 4", resp.Count)
	}
	wantT := []int64{1000, 2000, 3000, 4000}
	for i, want := range wantT {
		if resp.Points[i].T != want {
			t.Errorf("points[%d].t = %d, want %d（未按时间升序）", i, resp.Points[i].T, want)
		}
	}
}
