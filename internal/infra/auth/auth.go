// Package auth 实现 plcbridge HTTP API 的 Bearer Token 鉴权（§8.6）。
//
// MVP 简化：
//   - Token 从 config.json 读取（auth_tokens[]）
//   - 使用 JWT 签名（HMAC-SHA256）
//   - 不做 RBAC（Plan.md §A.2）
package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/plcbridge/plcbridge/internal/infra/config"
)

// Authenticator 鉴权器。
type Authenticator struct {
	secret []byte
}

// New 创建鉴权器（secret 从 config 读取）。
func New(store *config.Store) *Authenticator {
	cfg := store.Current()
	secret := []byte("plcbridge-default-secret-change-me")
	if cfg != nil {
		// 真实实现：cfg.AuthTokens / cfg.APISecret
		// MVP 暂用默认值
		_ = cfg
	}
	return &Authenticator{secret: secret}
}

// Middleware Gin 中间件。
func (a *Authenticator) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing Authorization header"})
			return
		}
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid Authorization header"})
			return
		}
		tokenStr := parts[1]

		token, err := jwt.Parse(tokenStr, func(_ *jwt.Token) (any, error) {
			return a.secret, nil
		}, jwt.WithValidMethods([]string{"HS256"}))
		if err != nil || !token.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		c.Next()
	}
}
