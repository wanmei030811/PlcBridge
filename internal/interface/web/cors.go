package web

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// corsMiddleware 极简 CORS：
//   - 默认同源部署：reflect 任意 Origin 并允许 credentials
//   - 自动处理 OPTIONS 预检返回 204
//   - 不限制 method / header（局域网运维工具场景）
//
// 注意：生产环境若把 API 暴露公网，应在反代层（Nginx/Caddy）限制 Origin。
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PATCH")
			c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
			c.Header("Access-Control-Max-Age", "86400")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}