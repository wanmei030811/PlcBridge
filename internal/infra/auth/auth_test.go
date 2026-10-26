package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// testSecret 是测试专用的强密钥（长度 >= 32，符合 secconfig 的强度要求）。
const testSecret = "test-only-secret-0123456789abcdef-32bytes"

// setupEnv 设置测试环境变量，并在测试结束后恢复。
func setupEnv(t *testing.T, key string) {
	t.Helper()
	orig, had := os.LookupEnv(SecretEnv)
	if err := os.Setenv(SecretEnv, key); err != nil {
		t.Fatalf("setenv failed: %v", err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(SecretEnv, orig)
		} else {
			_ = os.Unsetenv(SecretEnv)
		}
	})
}

// newTestRouter 构造一个带鉴权中间件和受保护路由的测试引擎。
func newTestRouter(a *Authenticator) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/protected", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	api := r.Group("/api/v1")
	api.Use(a.Middleware())
	api.GET("/thing", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

// TestServiceSecretPrefersEnv 验证：环境变量优先于 DefaultSecret。
func TestServiceSecretPrefersEnv(t *testing.T) {
	setupEnv(t, testSecret)
	got := string(ServiceSecret())
	if got != testSecret {
		t.Errorf("ServiceSecret() = %q, want %q", got, testSecret)
	}
}

// TestServiceSecretFallsBackToDefault 验证：未设置环境变量时回退到 DefaultSecret。
//
// 注意：这是**遗留行为**，生产路径已由 secconfig.EnsureAPISecret 拦截。
// 此测试锁住现状，防止有人无意中改变回退语义。
func TestServiceSecretFallsBackToDefault(t *testing.T) {
	orig, had := os.LookupEnv(SecretEnv)
	_ = os.Unsetenv(SecretEnv)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(SecretEnv, orig)
		}
	})

	if got := string(ServiceSecret()); got != DefaultSecret {
		t.Errorf("ServiceSecret() = %q, want %q", got, DefaultSecret)
	}
}

// TestServiceTokenAndMiddlewareAccept 验证：签发的 Token 能通过中间件校验。
func TestServiceTokenAndMiddlewareAccept(t *testing.T) {
	setupEnv(t, testSecret)

	tok, err := ServiceToken(time.Minute)
	if err != nil {
		t.Fatalf("ServiceToken failed: %v", err)
	}

	r := newTestRouter(New(nil))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestServiceTokenExpires 验证：过期 Token 被拒绝。
func TestServiceTokenExpires(t *testing.T) {
	setupEnv(t, testSecret)

	// 签发一个已经过期的 token（-1 分钟）。
	claims := jwt.MapClaims{
		"sub": "plcbridge-web",
		"iat": time.Now().Add(-2 * time.Minute).Unix(),
		"exp": time.Now().Add(-time.Minute).Unix(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign failed: %v", err)
	}

	r := newTestRouter(New(nil))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// TestMiddlewareRejectsMissingHeader 验证：缺少 Authorization 头 → 401。
func TestMiddlewareRejectsMissingHeader(t *testing.T) {
	setupEnv(t, testSecret)

	r := newTestRouter(New(nil))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unauthorized") {
		t.Errorf("body should contain 'unauthorized', got %s", w.Body.String())
	}
}

// TestMiddlewareRejectsMalformedHeader 验证：非 "Bearer <token>" 格式 → 401。
func TestMiddlewareRejectsMalformedHeader(t *testing.T) {
	setupEnv(t, testSecret)

	cases := []string{
		"",                   // 空
		"Token abc",          // 错误的 scheme
		"bearer abc",         // scheme 大小写不符（规范要求 Bearer）
		"Bearer",             // 缺少 token
		"Bearer a b",         // 空格过多
		"Basic dXNlcjpwYXNz", // 错误的 scheme
	}

	for _, h := range cases {
		r := newTestRouter(New(nil))
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("header %q: status = %d, want 401", h, w.Code)
		}
	}
}

// TestMiddlewareRejectsWrongSecret 验证：用错误密钥签发的 Token → 401。
//
// 这是最关键的越权场景：攻击者拿自己的密钥签 token，不能访问 API。
func TestMiddlewareRejectsWrongSecret(t *testing.T) {
	setupEnv(t, testSecret)

	attackerSecret := "attacker-secret-0123456789abcdef-32byte"
	claims := jwt.MapClaims{
		"sub": "attacker",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(attackerSecret))
	if err != nil {
		t.Fatalf("sign failed: %v", err)
	}

	r := newTestRouter(New(nil))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// TestMiddlewareRejectsAlgNone 验证：alg=none 的 token 被拒绝。
//
// JWT 最经典的攻击：把 alg 设成 none，去掉签名，服务器若不校验算法就放行。
func TestMiddlewareRejectsAlgNone(t *testing.T) {
	setupEnv(t, testSecret)

	// 手工构造 alg=none 的 token（头部 {"alg":"none","typ":"JWT"}）。
	tok := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." +
		"eyJzdWIiOiJhdHRhY2tlciIsImV4cCI6OTk5OTk5OTk5OX0."

	r := newTestRouter(New(nil))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// TestMiddlewareRejectsHS512 验证：非 HS256 算法的 token 被拒绝。
func TestMiddlewareRejectsHS512(t *testing.T) {
	setupEnv(t, testSecret)

	claims := jwt.MapClaims{
		"sub": "attacker",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign failed: %v", err)
	}

	r := newTestRouter(New(nil))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// TestMiddlewareProtectsOnlyGroupedRoutes 验证：未加中间件的公开路由仍可访问。
func TestMiddlewareProtectsOnlyGroupedRoutes(t *testing.T) {
	setupEnv(t, testSecret)

	r := newTestRouter(New(nil))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// TestServiceTokenDefaultTTL 验证：ttl<=0 时使用默认 5 分钟。
func TestServiceTokenDefaultTTL(t *testing.T) {
	setupEnv(t, testSecret)

	tok, err := ServiceToken(0)
	if err != nil {
		t.Fatalf("ServiceToken failed: %v", err)
	}

	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(tok, claims, func(_ *jwt.Token) (any, error) {
		return []byte(testSecret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	exp, ok := claims["exp"].(float64)
	if !ok {
		t.Fatalf("exp missing or wrong type: %#v", claims["exp"])
	}
	// 默认 TTL 是 5 分钟，留 1 分钟余量做时钟偏差容忍。
	lifetime := time.Until(time.Unix(int64(exp), 0))
	if lifetime > 5*time.Minute || lifetime < 3*time.Minute {
		t.Errorf("token lifetime = %v, want ~5m", lifetime)
	}
	_ = parsed
}
