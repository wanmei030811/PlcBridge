package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// newRouter 构造一个带指定中间件的测试引擎。
func newRouter(mw ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(mw...)
	r.POST("/echo", func(c *gin.Context) {
		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.GET("/slow", func(c *gin.Context) {
		time.Sleep(500 * time.Millisecond)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

// TestBodyLimitRejectsOversizedContentLength 验证：声明超大的 body 被直接拒绝。
func TestBodyLimitRejectsOversizedContentLength(t *testing.T) {
	r := newRouter(BodyLimit(1024))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(strings.Repeat("x", 2048)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), CodePayloadTooLarge) {
		t.Errorf("body should contain %q, got %s", CodePayloadTooLarge, w.Body.String())
	}
}

// TestBodyLimitAllowsNormalRequest 验证：正常大小的请求不受影响。
func TestBodyLimitAllowsNormalRequest(t *testing.T) {
	r := newRouter(BodyLimit(1024 * 1024))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(`{"a":1}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestBodyLimitDisabled 验证：maxBytes<=0 时不限流。
func TestBodyLimitDisabled(t *testing.T) {
	r := newRouter(BodyLimit(0))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(strings.Repeat("x", 100000)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	// handler 会因 JSON 解析失败返回 400（说明 body 被完整读取了），而不是 413。
	if w.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("body limit should be disabled, got 413")
	}
}

// TestSecurityHeaders 验证：安全响应头存在。
func TestSecurityHeaders(t *testing.T) {
	r := newRouter(SecurityHeaders())

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	r.ServeHTTP(w, req)

	checks := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, want := range checks {
		if got := w.Header().Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}

	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Error("Content-Security-Policy header missing")
	}
	if !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP should have default-src 'self', got %q", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP should have frame-ancestors 'none', got %q", csp)
	}
}

// TestCORSDeniesUnknownOrigin 验证：未列入白名单的 Origin 不获得 CORS 头。
//
// 这是核心安全修复：旧实现反射任意 Origin，导致任意站点可带着 cookie 访问。
func TestCORSDeniesUnknownOrigin(t *testing.T) {
	r := newRouter(CORS([]string{"http://allowed.example.com"}))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("Origin", "http://evil.example.com")
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want empty", got)
	}
}

// TestCORSDeniesAllWhenNoWhitelist 验证：白名单为空时不放行任何跨域。
func TestCORSDeniesAllWhenNoWhitelist(t *testing.T) {
	r := newRouter(CORS(nil))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("Origin", "http://any.example.com")
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

// TestCORSAllowsWhitelistedOrigin 验证：白名单内的 Origin 获得 CORS 头。
func TestCORSAllowsWhitelistedOrigin(t *testing.T) {
	r := newRouter(CORS([]string{"http://allowed.example.com"}))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("Origin", "http://allowed.example.com")
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://allowed.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the origin echoed", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want 'true'", got)
	}
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary = %q, should contain Origin", got)
	}
}

// TestCORSWildcardDoesNotAllowCredentials 验证：通配模式下不带凭证。
//
// 浏览器规范禁止 "*" 与 credentials 同用，带了会被浏览器拒绝。
func TestCORSWildcardDoesNotAllowCredentials(t *testing.T) {
	r := newRouter(CORS([]string{"*"}))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("Origin", "http://any.example.com")
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://any.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want origin echoed", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want empty in wildcard mode", got)
	}
}

// TestCORSPreflightReturns204 验证：OPTIONS 预检短路返回 204。
func TestCORSPreflightReturns204(t *testing.T) {
	r := newRouter(CORS([]string{"http://allowed.example.com"}))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/echo", nil)
	req.Header.Set("Origin", "http://allowed.example.com")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
}

// TestRateLimiterRejectsOverLimit 验证：超过速率上限的请求被 429 拒绝。
func TestRateLimiterRejectsOverLimit(t *testing.T) {
	rl := NewRateLimiter(1, 1) // 1 rps，突发 1
	if rl == nil {
		t.Fatal("NewRateLimiter returned nil for positive rps")
	}
	r := newRouter(rl.Middleware())

	// 第一次应该通过。
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.RemoteAddr = "10.1.2.3:12345"
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", w.Code)
	}

	// 第二次（同一 IP，突发额度已用完）应该被拒。
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req2.RemoteAddr = "10.1.2.3:12345"
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429; body=%s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), CodeRateLimited) {
		t.Errorf("body should contain %q, got %s", CodeRateLimited, w2.Body.String())
	}
	if w2.Header().Get("Retry-After") == "" {
		t.Error("Retry-After header missing")
	}
}

// TestRateLimiterIsolatesIPs 验证：不同 IP 独立计数。
func TestRateLimiterIsolatesIPs(t *testing.T) {
	rl := NewRateLimiter(1, 1)
	r := newRouter(rl.Middleware())

	for _, ip := range []string{"10.1.2.3:1", "10.1.2.4:1"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/ping", nil)
		req.RemoteAddr = ip
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("IP %s status = %d, want 200", ip, w.Code)
		}
	}
}

// TestRateLimiterNilWhenDisabled 验证：rps<=0 时返回 nil（不限流）。
func TestRateLimiterNilWhenDisabled(t *testing.T) {
	if NewRateLimiter(0, 100) != nil {
		t.Error("NewRateLimiter(0, ...) should return nil")
	}
	if NewRateLimiter(-1, 100) != nil {
		t.Error("NewRateLimiter(-1, ...) should return nil")
	}
}

// TestRateLimiterRefillsOverTime 验证：令牌随时间补充。
func TestRateLimiterRefillsOverTime(t *testing.T) {
	rl := NewRateLimiter(10, 1) // 10 rps，突发 1

	if !rl.Allow("ip") {
		t.Fatal("first request should be allowed")
	}
	if rl.Allow("ip") {
		t.Fatal("second immediate request should be rejected")
	}

	// 等 200ms（10 rps → 补充 2 个令牌）。
	time.Sleep(200 * time.Millisecond)

	if !rl.Allow("ip") {
		t.Error("request should be allowed after tokens refill")
	}
}

// TestTimeoutReturns504 验证：超时请求返回 504。
func TestTimeoutReturns504(t *testing.T) {
	r := newRouter(Timeout(100 * time.Millisecond))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/slow", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), CodeTimeout) {
		t.Errorf("body should contain %q, got %s", CodeTimeout, w.Body.String())
	}
}

// TestTimeoutDoesNotAffectFastRequests 验证：快速请求不受超时中间件影响。
func TestTimeoutDoesNotAffectFastRequests(t *testing.T) {
	r := newRouter(Timeout(2 * time.Second))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// TestTimeoutDisabled 验证：d<=0 时不限时。
func TestTimeoutDisabled(t *testing.T) {
	r := newRouter(Timeout(0))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}
