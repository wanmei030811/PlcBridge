// web → plcbridge 回写代理的端到端测试（用假的 core 服务端）。
//
// 背景：回写能力只存在于 plcbridge 主进程（:8082），web 进程（:8080）没有
// write worker。此前 web 端没注册 /write 路由，前端「批量写入」按钮
// 一点就报"接口不存在"，但直接调 :8082 完全正常 —— 极易误判为"功能没实现"。
//
// 验证：
//  1. POST /api/v1/write 与 /api/v1/write/batch 都能代理到 core
//  2. 请求体原样透传（关键：value / value_type 不能丢）
//  3. 带上 core 能接受的 Bearer Token
//  4. core 的状态码与响应体原样透传（含 400 / 504 等错误）
package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rawAuthedReq 构造带 session cookie、且 body 为**原始字节**的请求。
//
// 与 authedReq 的区别：authedReq 会把 body 当成对象做 JSON 编码，
// 传字符串会被编码成 JSON 字符串（"..."）而不是对象 ——
// 代理测试需要验证"请求体原样透传"，所以必须能传原始 JSON 文本。
func rawAuthedReq(method, path, cookie, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", SessionCookieName+"="+cookie)
	return req
}

// TestWriteProxy_ForwardsBodyAndAuth 验证单点回写代理：body 透传 + 带 Token。
func TestWriteProxy_ForwardsBodyAndAuth(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	var gotBody []byte
	srv := fakeCoreServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"trace_id":"t1","result":"success","duration_ms":3}`))
	})

	orig := plcbridgeTargetRuntimeBase
	plcbridgeTargetRuntimeBase = srv.URL
	defer func() { plcbridgeTargetRuntimeBase = orig }()

	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	payload := `{"device_biz_id":"OP01","point_biz_id":"M100","value":42,"value_type":"int8"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, rawAuthedReq(http.MethodPost, "/api/v1/write", cookie, payload))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("core 收到 method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/write" {
		t.Errorf("core 收到 path = %q, want /api/v1/write", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("代理必须带 Bearer Token，实际 = %q", gotAuth)
	}
	if strings.TrimSpace(string(gotBody)) != payload {
		t.Errorf("请求体未原样透传:\n got = %s\nwant = %s", gotBody, payload)
	}
	if !strings.Contains(w.Body.String(), `"result":"success"`) {
		t.Errorf("响应体未原样透传: %s", w.Body.String())
	}
}

// TestWriteProxy_BatchRoute 验证批量回写路由也代理到 core 的 /api/v1/write/batch。
func TestWriteProxy_BatchRoute(t *testing.T) {
	var gotPath string
	srv := fakeCoreServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"trace_id":"t2","result":"success","writes":[]}`))
	})

	orig := plcbridgeTargetRuntimeBase
	plcbridgeTargetRuntimeBase = srv.URL
	defer func() { plcbridgeTargetRuntimeBase = orig }()

	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	payload := `{"device_biz_id":"OP01","transactional":true,"writes":[{"point_biz_id":"M100","value":1,"value_type":"int8"}]}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, rawAuthedReq(http.MethodPost, "/api/v1/write/batch", cookie, payload))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/api/v1/write/batch" {
		t.Errorf("core 收到 path = %q, want /api/v1/write/batch", gotPath)
	}
}

// TestWriteProxy_PropagatesErrorStatus 验证 core 的错误状态码原样透传。
//
// 关键：主进程对"设备不可回写"返回 400，对"回写超时"返回 504。
// web 端必须原样透传，不能一律变 200 或 500 —— 否则前端无法区分
// "配置问题"（改配置即可）和"超时"（要重试）。
func TestWriteProxy_PropagatesErrorStatus(t *testing.T) {
	srv := fakeCoreServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"result":"fail","error":"设备不可回写: 设备已停用"}`))
	})

	orig := plcbridgeTargetRuntimeBase
	plcbridgeTargetRuntimeBase = srv.URL
	defer func() { plcbridgeTargetRuntimeBase = orig }()

	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, rawAuthedReq(http.MethodPost, "/api/v1/write", cookie,
		`{"device_biz_id":"x","point_biz_id":"y","value":1,"value_type":"int8"}`))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（原样透传 core 的错误码）", w.Code)
	}
	if !strings.Contains(w.Body.String(), "设备不可回写") {
		t.Errorf("错误体未透传: %s", w.Body.String())
	}
}

// TestWriteProxy_CoreUnreachable 验证 core 不可达时返回 503（而不是 500/404）。
func TestWriteProxy_CoreUnreachable(t *testing.T) {
	orig := plcbridgeTargetRuntimeBase
	plcbridgeTargetRuntimeBase = "http://127.0.0.1:1" // 必然连不上
	defer func() { plcbridgeTargetRuntimeBase = orig }()

	r, cleanup := newTestRouter(t)
	defer cleanup()
	cookie := loginAndCookie(t, r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, rawAuthedReq(http.MethodPost, "/api/v1/write", cookie,
		`{"device_biz_id":"x","point_biz_id":"y","value":1,"value_type":"int8"}`))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503. body=%s", w.Code, w.Body.String())
	}
}

// TestWriteProxy_RequiresLogin 验证未登录时不能回写。
//
// 关键：回写是**改变 PLC 状态**的操作，绝不能匿名可调。
func TestWriteProxy_RequiresLogin(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/write",
		strings.NewReader(`{"device_biz_id":"x","point_biz_id":"y","value":1,"value_type":"int8"}`)))

	if w.Code != http.StatusUnauthorized && w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 401/302（未登录不得回写）", w.Code)
	}
}
