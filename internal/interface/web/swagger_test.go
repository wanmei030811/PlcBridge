// Plan3 3.9：web 端 Swagger UI + OpenAPI spec 挂载测试。
//
// 背景（2026-10-24）：前端导航栏的「API 文档」指向 /swagger/，
// 但此前只有核心进程（:8082）挂了 Swagger，web 端（:8080）没挂 ——
// 用户点导航栏会落到 SPA fallback，打开的是前端首页而不是 API 文档。
//
// 同时验证 spec 是内嵌的（不依赖 cwd / exe 同级目录有 api/openapi.yaml）。
package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSwaggerMounted_ServesUI 验证 /swagger/ 返回 Swagger UI 页面。
func TestSwaggerMounted_ServesUI(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/swagger/", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("GET /swagger/ status = %d, want 200. body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "swagger-ui") {
		t.Errorf("GET /swagger/ 应返回 Swagger UI 页面，body 前 300 字符: %s", body[:min(300, len(body))])
	}
}

// TestSwaggerUI_NoStandaloneLayout 验证 Swagger UI 页面不再引用 StandaloneLayout。
//
// 回归测试（2026-10-24）：swagger-ui-dist@5.x 的 swagger-ui-bundle.js 不包含
// StandalonePreset（在单独的 swagger-ui-standalone-preset.js 里）。
// 引用 SwaggerUIBundle.SwaggerUIStandalonePreset 得到 undefined，
// Swagger UI 抛 "No layout defined for \"StandaloneLayout\""，页面只剩一行报错。
func TestSwaggerUI_NoStandaloneLayout(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/swagger/", nil))
	body := w.Body.String()

	// 去掉注释再检查 —— 注释里会解释"为什么不能用 StandaloneLayout"，
	// 直接对原文 grep 会误报。
	//
	// 注意要同时去掉两种注释：
	//   - HTML 注释 <!-- ... -->
	//   - JS 行注释 // ...（本文件里的说明就写在 <script> 内的 // 注释里）
	code := stripLineComments(stripHTMLComments(body))

	if strings.Contains(code, "SwaggerUIStandalonePreset") {
		t.Error("Swagger UI 页面不应引用 SwaggerUIBundle.SwaggerUIStandalonePreset（bundle 里没有它）")
	}
	if strings.Contains(code, "StandaloneLayout") {
		t.Error("Swagger UI 页面不应声明 layout: 'StandaloneLayout'（会抛 No layout defined）")
	}
	if !strings.Contains(code, "SwaggerUIBundle.presets.apis") {
		t.Error("Swagger UI 页面应使用 SwaggerUIBundle.presets.apis")
	}
}

// stripHTMLComments 去掉 HTML 注释（<!-- ... -->）。
func stripHTMLComments(s string) string {
	for {
		i := strings.Index(s, "<!--")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "-->")
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+3:]
	}
}

// stripLineComments 去掉整行的 JS 行注释（去掉行首空白后以 // 开头）。
//
// 只处理"整行都是注释"的情况：这样不会误伤 URL 里的 https://，
// 也不会误伤行尾注释（本文件没有行尾注释）。
func stripLineComments(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// TestSwaggerMounted_RedirectsNoSlash 验证 /swagger（无斜杠）重定向到 /swagger/。
func TestSwaggerMounted_RedirectsNoSlash(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/swagger", nil))

	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /swagger status = %d, want 301", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/swagger/" {
		t.Errorf("Location = %q, want /swagger/", loc)
	}
}

// TestOpenAPIYAMLServed 验证 /api/openapi.yaml 返回内嵌的 spec。
//
// 关键点：spec 必须**非空**。此前实现是运行时读文件，
// 找不到文件时 openAPISpec 是 nil，接口返回 200 + 空 body，Swagger UI 白屏且无报错。
func TestOpenAPIYAMLServed(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/openapi.yaml", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/openapi.yaml status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if len(body) == 0 {
		t.Fatal("GET /api/openapi.yaml 返回空 body（spec 未内嵌）")
	}
	if !strings.Contains(body, "openapi:") {
		t.Errorf("响应不含 OpenAPI 文档，body 前 200 字符: %s", body[:min(200, len(body))])
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Errorf("Content-Type = %q, want 含 yaml", ct)
	}
}

// TestOpenAPISpec_CoversPlan3Endpoints 验证 spec 里含 Plan3 新增的关键端点。
//
// 回归测试（2026-10-24）：这 4 个端点已实现但漏在 spec 外。
func TestOpenAPISpec_CoversPlan3Endpoints(t *testing.T) {
	r, cleanup := newTestRouter(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/openapi.yaml", nil))
	body := w.Body.String()

	want := []string{
		"/api/v1/points/import:",
		"/api/v1/points/export:",
		"/api/v1/devices/{id}/clone:",
		"/api/v1/channels/runtime:",
	}
	for _, p := range want {
		if !strings.Contains(body, p) {
			t.Errorf("OpenAPI spec 缺端点 %s", p)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
