// Package auth 实现 plcbridge HTTP API 的 Bearer Token 鉴权（§8.6）。
//
// MVP 简化：
//   - Token 从 config.json 读取（auth_tokens[]）
//   - 使用 JWT 签名（HMAC-SHA256）
//   - 不做 RBAC（Plan.md §A.2）
package auth

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/plcbridge/plcbridge/internal/infra/config"
)

// DefaultSecret 是历史遗留的默认 HMAC 签名密钥。
//
// ⚠️ 这个值公开在本仓库源码里，**任何人都知道**。它绝不能用于生产。
//
// 现状（安全修复后）：
//   - 生产启动路径（cmd/plcbridge）已改为强制要求环境变量 PLCBRIDGE_API_SECRET，
//     密钥缺失或过弱直接拒启动（见 secconfig.EnsureAPISecret）。
//   - supervisor.ps1 会在首次启动时自动生成 32 字节随机密钥写入
//     <ConfigDir>/api.secret，并注入两个子进程。
//   - 本常量仅保留给「单测 / 本地开发」路径使用；用到它的地方必须有注释说明
//     这是非生产场景。
//
// 保留而不删除的原因：删除会破坏大量现有测试与本地开发脚本的编译。
// 正确的长期做法是让测试统一注入一个明显标注为测试用的密钥。
const DefaultSecret = "plcbridge-default-secret-change-me"

// SecretEnv 是自定义签名密钥的环境变量名。
//
// plcbridge-web 需要用它签发「服务令牌」去调 plcbridge :8082，
// 两边必须用同一个密钥，否则 401。
const SecretEnv = "PLCBRIDGE_API_SECRET"

// ServiceSecret 返回当前进程使用的 HMAC 签名密钥。
//
// 优先级：环境变量 PLCBRIDGE_API_SECRET > DefaultSecret。
//
// 说明：plcbridge 与 plcbridge-web 是同机部署的两个进程，通过 localhost 通信，
// 共享密钥是合理的（同信任域）。若两者部署在不同机器，应改用 mTLS 或
// 独立的内部鉴权机制。
func ServiceSecret() []byte {
	if s := os.Getenv(SecretEnv); s != "" {
		return []byte(s)
	}
	return []byte(DefaultSecret)
}

// ServiceToken 签发一个用于 web → plcbridge 内部调用的 Bearer Token。
//
// 有效期短（默认 5 分钟），因为是每次请求现签，不做持久化。
// 签名算法与 auth.Middleware 校验时一致（HS256）。
func ServiceToken(ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	claims := jwt.MapClaims{
		"sub": "plcbridge-web",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(ttl).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(ServiceSecret())
}

// Authenticator 鉴权器。
type Authenticator struct {
	secret []byte
}

// New 创建鉴权器（secret 从环境变量 / 默认值读取）。
func New(store *config.Store) *Authenticator {
	secret := ServiceSecret()
	if store != nil {
		// 未来可从 config 读（cfg.APISecret），当前用环境变量 + 默认值。
		_ = store
	}
	return &Authenticator{secret: secret}
}

// Middleware Gin 中间件。
//
// 错误响应格式（Plan2 4.1 一致）：返回 {code, msg}，code=unauthorized / unauthorized_invalid_token。
func (a *Authenticator) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.Abort()
			c.JSON(http.StatusUnauthorized, gin.H{
				"code": "unauthorized",
				"msg":  "missing Authorization header",
			})
			return
		}
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			c.Abort()
			c.JSON(http.StatusUnauthorized, gin.H{
				"code": "unauthorized",
				"msg":  "invalid Authorization header",
			})
			return
		}
		tokenStr := parts[1]

		token, err := jwt.Parse(tokenStr, func(_ *jwt.Token) (any, error) {
			return a.secret, nil
		}, jwt.WithValidMethods([]string{"HS256"}))
		if err != nil || !token.Valid {
			c.Abort()
			c.JSON(http.StatusUnauthorized, gin.H{
				"code": "unauthorized_invalid_token",
				"msg":  "invalid token",
			})
			return
		}
		c.Next()
	}
}
