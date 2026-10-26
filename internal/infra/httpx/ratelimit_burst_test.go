package httpx

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestRateLimiter_BurstIsCapped 验证令牌桶突发上限真的会挡住超量请求。
//
// 现场意义：API 暴露在工控网段，突发请求（前端批量轮询 / 脚本误循环）
// 不能把进程压垮。
func TestRateLimiter_BurstIsCapped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rl := NewRateLimiter(50, 10)

	r := gin.New()
	r.Use(rl.Middleware())
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	var okCount, limited int64
	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.RemoteAddr = "10.0.0.1:1234"
			r.ServeHTTP(w, req)
			if w.Code == http.StatusTooManyRequests {
				atomic.AddInt64(&limited, 1)
			} else {
				atomic.AddInt64(&okCount, 1)
			}
		}()
	}
	wg.Wait()

	if limited == 0 {
		t.Fatalf("并发 %d 个请求全部通过（ok=%d），限流器未生效", n, okCount)
	}
	if okCount > 10 {
		t.Errorf("放行数 = %d, want <= 10（burst 上限）", okCount)
	}
	t.Logf("放行=%d, 限流=%d", okCount, limited)
}

// TestRateLimiter_PerIPIsolation 验证不同 IP 互不影响。
func TestRateLimiter_PerIPIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rl := NewRateLimiter(1, 1)

	// IP A 用掉唯一令牌
	if !rl.Allow("1.1.1.1") {
		t.Fatal("IP A 首次请求应放行")
	}
	if rl.Allow("1.1.1.1") {
		t.Error("IP A 第二次请求应被限流")
	}
	// IP B 不受影响
	if !rl.Allow("2.2.2.2") {
		t.Error("IP B 首次请求不应被 IP A 的消耗影响")
	}
}
