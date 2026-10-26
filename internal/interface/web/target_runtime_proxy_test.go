// Target 运行态代理（→ plcbridge :8082）测试。
//
// 覆盖 Plan3 3.4（诊断）/ 3.5（历史趋势）在 web :8080 上的路由与代理行为。
//
// 背景：这两个路由此前只注册在 plcbridge :8082 上，web :8080 没有 handler，
// 前端调 /api/v1/targets/{bizId}/history 直接 404（SPA fallback 的 not_found）。
package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTargetRuntimeRoutesRegistered 验证路由已注册（不再 404 "api route not found"）。
//
// 这是回归测试：修复前 GET /api/v1/targets/influxdb/history 返回
// 404 + {"code":"not_found","msg":"api route not found"}。
func TestTargetRuntimeRoutesRegistered(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	cases := []struct {
		name string
		path string
	}{
		{"history", "/api/v1/targets/influxdb/history?measurement=sss"},
		{"diag", "/api/v1/targets/influxdb/diag"},
		{"targets diag", "/api/v1/targets/diag"},
		// Plan3 3.2 实时值代理。注意这两条的 :id 装的是业务 ID（source/device_biz_id），
		// 与同组的 /sources/:id、/devices/:id（行号）不冲突，但都必须是已注册路由。
		{"source values", "/api/v1/sources/src-1/values"},
		{"device values", "/api/v1/devices/dev-1/values"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, authedReq(http.MethodGet, c.path, cookie, nil))
		// 未登录应 401；已登录则不该是 404 路由不存在。
		if w.Code == http.StatusNotFound && strings.Contains(w.Body.String(), "api route not found") {
			t.Errorf("%s: 路由未注册，返回 %d body=%s", c.name, w.Code, w.Body.String())
		}
	}
}

// TestTargetHistoryRequiresMeasurement 验证 measurement 必填（省一次跨进程调用）。
func TestTargetHistoryRequiresMeasurement(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet, "/api/v1/targets/influxdb/history", cookie, nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	decodeJSON(t, w, &body)
	if body.Code != "invalid_param" {
		t.Errorf("code = %q, want invalid_param", body.Code)
	}
}

// TestTargetRuntimeRequiresAuth 验证这些路由需要登录。
func TestTargetRuntimeRequiresAuth(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	for _, path := range []string{
		"/api/v1/targets/influxdb/history?measurement=sss",
		"/api/v1/targets/influxdb/diag",
		"/api/v1/targets/diag",
		"/api/v1/sources/src-1/values",
		"/api/v1/devices/dev-1/values",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, w.Code)
		}
	}
}

// TestProxyToCoreUnreachable 验证主进程不可达时返回 503 + 统一错误格式。
func TestProxyToCoreUnreachable(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	// 用一个肯定连不上的地址覆盖基址（测试内改包级变量）。
	orig := plcbridgeTargetRuntimeBase
	plcbridgeTargetRuntimeBase = "http://127.0.0.1:1"
	defer func() { plcbridgeTargetRuntimeBase = orig }()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedReq(http.MethodGet, "/api/v1/targets/influxdb/history?measurement=sss", cookie, nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503. body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != "plcbridge_unavailable" {
		t.Errorf("code = %q, want plcbridge_unavailable", body.Code)
	}
}
