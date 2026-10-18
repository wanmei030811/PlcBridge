// 单账号 + HTTPOnly Cookie 鉴权（Plan §A.2：MVP 阶段不用 RBAC，用单账号即可）。
//
// 数据流：
//   - accounts 表存 username + bcrypt(password)
//   - 登录成功 → 生成 session token → 内存 SessionStore 绑定 user → 下 HTTPOnly Cookie
//   - 中间件 RequireSession 解析 Cookie → 查 SessionStore → 校验未过期
//
// 启动时若无 accounts 行，main.go 会生成初始 admin 密码并打印到日志。
package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/plcbridge/plcbridge/internal/infra/logging"
)

const (
	SessionCookieName = "plcbridge_session"
	SessionTTL        = 12 * time.Hour
	cookieHMACSecretLen = 32
)

// SessionStore 内存 session 表。
//
// 单进程内存存储（Plan §A.2 MVP）：重启 → 失效，符合单账号本地运维预期；
// 集群部署需要迁移到 Redis 共享 session（见 Plan §A.2 后续工作）。
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]sessionEntry // token → {username, expiresAt}
	hmacKey  []byte

	stopGC   chan struct{}
	stopOnce sync.Once
}

type sessionEntry struct {
	username  string
	expiresAt time.Time
}

// NewSessionStore 创建 session store（HMAC key 由调用方传入，保持重启会过期一致性）。
// 启动后台 GC 协程清理过期 entry，避免长跑内存泄漏。
func NewSessionStore(hmacKey []byte) *SessionStore {
	if len(hmacKey) < cookieHMACSecretLen {
		// 长度不够：panic（这是 key 生成/加载逻辑的 bug，不能静默续 0）
		panic(fmt.Sprintf("HMAC key too short: %d < %d", len(hmacKey), cookieHMACSecretLen))
	}
	s := &SessionStore{
		sessions: map[string]sessionEntry{},
		hmacKey:  hmacKey,
		stopGC:   make(chan struct{}),
	}
	go s.runGC()
	return s
}

// Stop 关闭后台 GC（测试 / 进程退出时调用）。用 sync.Once 兜底多次 Stop 调用导致 double-close panic。
func (s *SessionStore) Stop() {
	s.stopOnce.Do(func() { close(s.stopGC) })
}

// runGC 每分钟扫一次过期 entry 并清理。
func (s *SessionStore) runGC() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.stopGC:
			return
		case now := <-t.C:
			s.sweep(now)
		}
	}
}

func (s *SessionStore) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.sessions {
		if now.After(e.expiresAt) {
			delete(s.sessions, k)
		}
	}
}

// Len 当前 session 数（仅测试用）。
func (s *SessionStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// Issue 给 username 颁发一个 token，同时返回 cookie value（已经 HMAC 签名）。
func (s *SessionStore) Issue(username string) (cookie string, err error) {
	tok, err := randomToken(32)
	if err != nil {
		return "", err
	}
	exp := time.Now().Add(SessionTTL)
	s.mu.Lock()
	s.sessions[tok] = sessionEntry{username: username, expiresAt: exp}
	s.mu.Unlock()

	// cookie 值 = token + "." + sig(token)
	sig := s.sign(tok)
	return tok + "." + sig, nil
}

// Validate 解析 cookie value 并返回 username；失败返回 ErrUnauthorized。
var ErrUnauthorized = errors.New("unauthorized")

func (s *SessionStore) Validate(cookieValue string) (string, error) {
	idx := -1
	for i := len(cookieValue) - 1; i >= 0; i-- {
		if cookieValue[i] == '.' {
			idx = i
			break
		}
	}
	if idx <= 0 || idx >= len(cookieValue)-1 {
		return "", ErrUnauthorized
	}
	tok := cookieValue[:idx]
	gotSig := cookieValue[idx+1:]

	wantSig := s.sign(tok)
	gotBytes := []byte(gotSig)
	wantBytes := []byte(wantSig)
	// hmac.Equal 内部走 constant-time，且对长度不等也走常量时间分支。
	if !hmac.Equal(gotBytes, wantBytes) {
		return "", ErrUnauthorized
	}
	s.mu.RLock()
	entry, ok := s.sessions[tok]
	expired := ok && time.Now().After(entry.expiresAt)
	s.mu.RUnlock()
	if !ok || expired {
		// 顺手 GC：删除过期 entry，避免过期 session 持续占内存
		if expired {
			s.mu.Lock()
			if e, ok := s.sessions[tok]; ok && time.Now().After(e.expiresAt) {
				delete(s.sessions, tok)
			}
			s.mu.Unlock()
		}
		return "", ErrUnauthorized
	}
	return entry.username, nil
}

// Revoke 主动撤销 cookie（logout）。
func (s *SessionStore) Revoke(cookieValue string) {
	idx := -1
	for i := len(cookieValue) - 1; i >= 0; i-- {
		if cookieValue[i] == '.' {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return
	}
	tok := cookieValue[:idx]
	s.mu.Lock()
	delete(s.sessions, tok)
	s.mu.Unlock()
}

func (s *SessionStore) sign(tok string) string {
	mac := hmac.New(sha256.New, s.hmacKey)
	mac.Write([]byte(tok))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// RandomHMACKey 生成 HMAC key（用于 session 签名；启动时调用一次，存盘）。
func RandomHMACKey() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

// === HTTP layer ===

// loginHandler 单账号密码登录。
func loginHandler(db *sql.DB, sess *SessionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Username string `json:"username" binding:"required"`
			Password string `json:"password" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// 查账号
		var pwHash string
		err := db.QueryRowContext(c.Request.Context(),
			`SELECT password_hash FROM accounts WHERE id=1 AND username=?`, req.Username).Scan(&pwHash)
		if err != nil {
			// 计时攻击防护：无论用户不存在还是密码错，固定返回错
			_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$abcdefghijklmnopqrstuv"), []byte(req.Password))
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(pwHash), []byte(req.Password)); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		cookieVal, err := sess.Issue(req.Username)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// cookie: HttpOnly + SameSite=Lax（防止 CSRF + 防 XSS 读取）
		// secure=false（开发支持 HTTP），生产建议前置 TLS 反代后改 true
		c.SetSameSite(http.SameSiteLaxMode)
		c.SetCookie(SessionCookieName, cookieVal, int(SessionTTL.Seconds()), "/", "", false, true)
		c.JSON(http.StatusOK, gin.H{"username": req.Username})
	}
}

// logoutHandler 撤销 session。
func logoutHandler(sess *SessionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if v, err := c.Cookie(SessionCookieName); err == nil {
			sess.Revoke(v)
		}
		c.SetSameSite(http.SameSiteLaxMode)
		c.SetCookie(SessionCookieName, "", -1, "/", "", false, true)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// meHandler 返回当前用户。
func meHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		username := c.GetString("session_user")
		c.JSON(http.StatusOK, gin.H{"username": username})
	}
}

// changePasswordHandler 改密码（要求已登录）。
func changePasswordHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		username := c.GetString("session_user")
		var req struct {
			OldPassword string `json:"old_password" binding:"required"`
			NewPassword string `json:"new_password" binding:"required,min=8"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var pwHash string
		if err := db.QueryRowContext(c.Request.Context(),
			`SELECT password_hash FROM accounts WHERE id=1 AND username=?`, username).Scan(&pwHash); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(pwHash), []byte(req.OldPassword)); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "old password incorrect"})
			return
		}
		newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if _, err := db.ExecContext(c.Request.Context(),
			`UPDATE accounts SET password_hash=?, updated_at=? WHERE id=1`,
			string(newHash), time.Now().UnixMilli()); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// RequireSession 中间件：未登录 → 401。
func RequireSession(sess *SessionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		v, err := c.Cookie(SessionCookieName)
		if err != nil || v == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "login required"})
			return
		}
		username, err := sess.Validate(v)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
			return
		}
		c.Set("session_user", username)
		c.Next()
	}
}

// HashPassword bcrypt 封装。
func HashPassword(p string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// randomToken 16 字节随机（base64 URL safe）。
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// RandomPassword 生成 N 字节随机密码（hex），用于首次启动打印。
func RandomPassword(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// EnsureAccount 单账号初始化。
//
// 规则：
//   - accounts 表为空 → 插入 username + bcrypt(password)；password 为空则随机生成
//   - accounts 表已有 → 直接返回 ("", nil)；绝不返回旧密码或入参
//
// 返回值：仅当**本次新生成**了密码时返回该密码供调用方写 bootstrap；其他情况一律空串。
func EnsureAccount(db *sql.DB, username, password string) (string, error) {
	if username == "" {
		username = "admin"
	}
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE id=1`).Scan(&exists); err != nil {
		return "", err
	}
	if exists > 0 {
		// 账号已存在：绝不返回旧密码，避免 main 误判为新生成再次落盘明文
		return "", nil
	}
	generated := false
	if password == "" {
		password = RandomPassword(8)
		generated = true
	}
	hashed, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	if err := NewStore(db).UpsertAccount(context.Background(), username, hashed); err != nil {
		return "", err
	}
	if generated {
		logging.Info(logging.EventServiceStart).
			Str("username", username).
			Str("password", password).
			Str("note", "请登录后立即修改密码（系统设置 → 修改密码）").
			Msg("初始管理员账号已生成")
		return password, nil
	}
	// 显式指定了密码但账号不存在 → 创建后不返回明文（main 已知）
	return "", nil
}
