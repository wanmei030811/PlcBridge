// Package httpx 提供 plcbridge 两个 HTTP 服务共用的防护中间件。
//
// 为什么单独成包：
//
//	core 进程（:8082）和 web 进程（:8080）都需要同一套防护
//	（超时 / 请求体上限 / 限流 / 安全响应头）。
//	放在任一进程里都会造成另一进程漏配 —— 这正是历史上出现过的
//	"web 端做了校验、core 端没做"那类 bug 的成因。
//
// 设计原则：
//  1. 中间件全部可组合，且顺序无关（各自独立生效）。
//  2. 限流用标准库实现，不引入第三方依赖（避免供应链面扩大）。
//  3. 所有错误响应走统一的 {code, msg} 格式（与 web 端保持一致）。
package httpx

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// 错误响应用的错误码（与 web 端 errors.go 的命名风格一致）。
const (
	// CodeRateLimited 表示触发限流。
	CodeRateLimited = "rate_limited"
	// CodePayloadTooLarge 表示请求体超过上限。
	CodePayloadTooLarge = "payload_too_large"
	// CodeTimeout 表示请求处理超时。
	CodeTimeout = "request_timeout"
)

// writeError 写统一格式的错误响应。
func writeError(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{
		"code": code,
		"msg":  msg,
	})
}

// BodyLimit 限制请求体大小。
//
// 必须在 ShouldBindJSON / ShouldBind 之前使用 —— gin 的绑定是从
// c.Request.Body 读的，如果先读再限制就没意义了。
//
// maxBytes <= 0 时不限制（保留调用方显式关闭的能力）。
func BodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBytes <= 0 {
			c.Next()
			return
		}
		// 已声明 Content-Length 的请求：直接拒绝，避免读 body。
		if c.Request.ContentLength > maxBytes {
			writeError(c, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
				"请求体超过限制 ("+strconv.FormatInt(maxBytes, 10)+" 字节)")
			return
		}
		// chunked 传输（无 Content-Length）：包一层限制读取器。
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

// Timeout 给请求处理加超时。
//
// 实现方式：派生一个带超时的 ctx 塞回 Request，handler 侧通过
// c.Request.Context() 感知超时并可主动取消。
//
// 关键约束：**不在独立 goroutine 里写响应**。
// gin 的 Context 不是并发安全的，从看门狗 goroutine 调 writeError 会与
// handler 的写入竞争（曾导致 "close of closed channel" panic 与响应串扰）。
//
// 正确做法：用 timeoutWriter 包住 c.Writer，在 handler **尝试写响应时**
// 检查是否已超时 —— 超时则把状态码替换成 504。
// 这样:
//   - 响应永远由 handler 所在的 goroutine 写出，无并发写竞争。
//   - 超时的 handler 即使继续跑，其响应内容也会被丢弃（只回 504）。
//   - 真正的连接级保护仍由 http.Server 的 WriteTimeout / ReadHeaderTimeout 承担。
//
// 推荐 handler 主动检查 c.Request.Context().Done() 并提前返回，可让客户端更快拿到响应。
func Timeout(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d <= 0 {
			c.Next()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)

		tw := &timeoutWriter{ResponseWriter: c.Writer, ctx: ctx}
		c.Writer = tw

		c.Next()
	}
}

// timeoutWriter 包装 gin.ResponseWriter，在写响应时检查是否已超时。
//
// 超时则把状态码替换为 504 并清空 body，让客户端拿到明确错误而不是半个成功响应。
type timeoutWriter struct {
	gin.ResponseWriter
	ctx      context.Context
	timedOut bool
}

// WriteHeader 写状态码：超时则替换为 504。
func (w *timeoutWriter) WriteHeader(code int) {
	if w.ctx.Err() == context.DeadlineExceeded {
		w.timedOut = true
		code = http.StatusGatewayTimeout
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write 写 body：超时则丢弃原内容，改为写统一的超时错误体。
func (w *timeoutWriter) Write(b []byte) (int, error) {
	if w.ctx.Err() == context.DeadlineExceeded {
		w.timedOut = true
		if !w.ResponseWriter.Written() {
			w.ResponseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.ResponseWriter.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.ResponseWriter.Write(timeoutBody())
		}
		// 丢弃原 body（返回写入长度避免上层误判为错误）。
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// WriteString 写字符串 body：语义同 Write。
func (w *timeoutWriter) WriteString(s string) (int, error) {
	if w.ctx.Err() == context.DeadlineExceeded {
		w.timedOut = true
		if !w.ResponseWriter.Written() {
			w.ResponseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.ResponseWriter.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.ResponseWriter.Write(timeoutBody())
		}
		return len(s), nil
	}
	return w.ResponseWriter.WriteString(s)
}

// timeoutBody 返回超时时统一写出的错误响应体。
func timeoutBody() []byte {
	return []byte(`{"code":"` + CodeTimeout + `","msg":"请求处理超时"}`)
}

// Status 返回实际写出的状态码（超时后为 504）。
func (w *timeoutWriter) Status() int {
	if w.timedOut {
		return http.StatusGatewayTimeout
	}
	return w.ResponseWriter.Status()
}

// SecurityHeaders 添加安全响应头。
//
// CSP 设为 restrictive：默认拒绝一切外部资源加载。
// 前端是 Vue SPA + 内嵌静态资源，不需要任何外部 CDN。
// 但 Element Plus 的图标和 ECharts 有时会用 inline style / data: URI，
// 所以 style-src 放宽到 'unsafe-inline'（不能省，否则样式全挂）。
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		// CSP：默认拒绝；script 允许 'unsafe-inline'（Vue 运行时注入的内联脚本）
		// 与 'unsafe-eval'（部分构建工具的 eval），style 允许 inline 与 data:。
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline' 'unsafe-eval'; "+
				"style-src 'self' 'unsafe-inline' data:; "+
				"img-src 'self' data: blob:; "+
				"font-src 'self' data:; "+
				"connect-src 'self'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'")
		c.Next()
	}
}

// RateLimiter 是按客户端 IP 的令牌桶限流器。
//
// 为什么自己实现而不是用 golang.org/x/time/rate：
//  1. 避免引入新依赖（供应链面 + go.mod 变动）。
//  2. 需要"按 IP 分桶 + 定期清理过期桶"，标准库没有现成的。
//
// 并发安全：mu 保护 map，桶内用原子/互斥保护令牌数。
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rps     float64
	burst   int
	// lastSweep 记录上次清理空闲桶的时间。
	lastSweep time.Time
}

// bucket 是单个 IP 的令牌桶。
type bucket struct {
	tokens   float64
	lastFill time.Time
	lastSeen time.Time
}

// NewRateLimiter 创建限流器。
//
// rps <= 0 表示不限流（返回 nil，调用方跳过中间件）。
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	if rps <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = int(rps * 2)
		if burst < 1 {
			burst = 1
		}
	}
	return &RateLimiter{
		buckets: make(map[string]*bucket),
		rps:     rps,
		burst:   burst,
	}
}

// Allow 判断某 IP 的请求是否放行。
func (rl *RateLimiter) Allow(ip string) bool {
	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	// 定期清理：5 分钟没活动的桶直接删（防内存无限增长）。
	if now.Sub(rl.lastSweep) > 5*time.Minute {
		for k, b := range rl.buckets {
			if now.Sub(b.lastSeen) > 5*time.Minute {
				delete(rl.buckets, k)
			}
		}
		rl.lastSweep = now
	}

	b, ok := rl.buckets[ip]
	if !ok {
		b = &bucket{tokens: float64(rl.burst), lastFill: now, lastSeen: now}
		rl.buckets[ip] = b
	}

	// 按经过时间补充令牌（上限 burst）。
	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens += elapsed * rl.rps
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}
	b.lastFill = now
	b.lastSeen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Middleware 返回限流中间件。
//
// rl 为 nil 时返回 nil（调用方需判断）。
func (rl *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !rl.Allow(clientIP(c)) {
			c.Header("Retry-After", "1")
			writeError(c, http.StatusTooManyRequests, CodeRateLimited,
				"请求过于频繁，请稍后重试")
			return
		}
		c.Next()
	}
}

// clientIP 提取客户端 IP。
//
// 注意：这里**不信任** X-Forwarded-For —— 除非前面有可信反代，
// 否则攻击者可以随意伪造该头绕过限流。
// 需要反代场景时，应改为只信任来自已知反代 IP 的 XFF。
func clientIP(c *gin.Context) string {
	if ip, _, err := net.SplitHostPort(c.Request.RemoteAddr); err == nil && ip != "" {
		return ip
	}
	return c.Request.RemoteAddr
}

// CORS 白名单中间件。
//
// 与旧实现的关键差异：
//   - 旧：反射任意 Origin + Allow-Credentials: true → 任意站点可带着用户
//     cookie 读写 PLC 配置（CSRF + 数据窃取）。
//   - 新：默认不放行任何跨域；只有显式列入白名单的 Origin 才回 CORS 头。
//
// allowed 为空 → 不写任何 CORS 响应头（同源部署的默认行为）。
// allowed 含 "*" → 允许任意 Origin，但**不**回 Allow-Credentials
// （浏览器规范禁止 "*" 与 credentials 同用）。
func CORS(allowed []string) gin.HandlerFunc {
	allowAll := false
	allowSet := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		o = strings.TrimSpace(o)
		if o == "*" {
			allowAll = true
			continue
		}
		if o != "" {
			allowSet[strings.ToLower(o)] = true
		}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" && (allowAll || allowSet[strings.ToLower(origin)]) {
			h := c.Writer.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PATCH")
			h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
			h.Set("Access-Control-Max-Age", "86400")
			// 只有非通配模式才允许携带 cookie。
			if !allowAll {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
		}
		// 预检请求：无论是否放行都要短路返回，否则会走到业务 handler。
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
