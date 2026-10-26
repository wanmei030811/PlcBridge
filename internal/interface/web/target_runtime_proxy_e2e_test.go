// web → plcbridge 代理链路的端到端测试（用假的 core 服务端）。
//
// 验证：
//  1. web 端代理会带上可被 core 接受的 Bearer Token
//  2. query string（measurement / field / duration / limit）原样透传
//  3. core 的响应体原样透传给前端
//  4. core 返回非 2xx 时，状态码与错误体原样透传
package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeCoreServer 模拟 plcbridge :8082 的目标运行时接口。
func fakeCoreServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// TestProxyForwardsAuthAndQuery 验证代理带上 Token 且透传 query。
func TestProxyForwardsAuthAndQuery(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	srv := fakeCoreServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"target_biz_id":"influxdb","measurement":"sss","count":2,
			"points":[{"t":1750000000000,"v":1},{"t":1750000060000,"v":2}]}`))
	})

	orig := plcbridgeTargetRuntimeBase
	plcbridgeTargetRuntimeBase = srv.URL
	defer func() { plcbridgeTargetRuntimeBase = orig }()

	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet,
		"/api/v1/targets/influxdb/history?measurement=sss&field=M100&duration=1h&limit=2000",
		cookie, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}
	if gotAuth == "" {
		t.Error("代理必须带上 Authorization 头")
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("Authorization 格式错: %q", gotAuth)
	}
	if gotPath != "/api/v1/targets/influxdb/history" {
		t.Errorf("路径 = %q", gotPath)
	}
	for _, want := range []string{"measurement=sss", "field=M100", "duration=1h", "limit=2000"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query 未透传 %q, got: %s", want, gotQuery)
		}
	}

	// 响应体原样透传
	var resp struct {
		TargetBizID string `json:"target_biz_id"`
		Count       int    `json:"count"`
		Points      []struct {
			T int64   `json:"t"`
			V float64 `json:"v"`
		} `json:"points"`
	}
	decodeJSON(t, w, &resp)
	if resp.TargetBizID != "influxdb" {
		t.Errorf("target_biz_id = %q", resp.TargetBizID)
	}
	if resp.Count != 2 || len(resp.Points) != 2 {
		t.Errorf("count=%d points=%d, want 2/2", resp.Count, len(resp.Points))
	}
	if resp.Points[0].T != 1750000000000 {
		t.Errorf("t = %d", resp.Points[0].T)
	}
}

// TestProxyPassesThroughCoreError 验证 core 的错误原样透传。
func TestProxyPassesThroughCoreError(t *testing.T) {
	srv := fakeCoreServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"history_not_supported","msg":"该 Target 不支持历史查询"}`))
	})

	orig := plcbridgeTargetRuntimeBase
	plcbridgeTargetRuntimeBase = srv.URL
	defer func() { plcbridgeTargetRuntimeBase = orig }()

	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet,
		"/api/v1/targets/influxdb/history?measurement=sss&field=M100", cookie, nil))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != "history_not_supported" {
		t.Errorf("code = %q, want history_not_supported", body.Code)
	}
	if body.Msg == "" {
		t.Error("msg 不应为空")
	}
}

// TestProxyDiagPassthrough 验证 diag 代理同样透传。
func TestProxyDiagPassthrough(t *testing.T) {
	srv := fakeCoreServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/targets/influxdb/diag" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"target_biz_id":"influxdb","connected":false}`))
	})

	orig := plcbridgeTargetRuntimeBase
	plcbridgeTargetRuntimeBase = srv.URL
	defer func() { plcbridgeTargetRuntimeBase = orig }()

	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet, "/api/v1/targets/influxdb/diag", cookie, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	decodeJSON(t, w, &body)
	if body["target_biz_id"] != "influxdb" {
		t.Errorf("target_biz_id = %v", body["target_biz_id"])
	}
}
