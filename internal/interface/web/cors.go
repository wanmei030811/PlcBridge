package web

import (
	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/infra/httpx"
	"github.com/plcbridge/plcbridge/internal/infra/secconfig"
)

// corsMiddleware CORS 白名单中间件。
//
// 与旧实现的关键差异（安全修复）：
//
//	旧：反射任意 Origin + Access-Control-Allow-Credentials: true
//	    → 任意第三方网站都能带着用户的 session cookie 读写 PLC 配置
//	    （读取点位数据 + 修改回写值），属于典型的 CSRF + 数据窃取。
//
//	新：默认不放行任何跨域（同源部署的最安全默认）；
//	    只有 PLCBRIDGE_CORS_ORIGINS 白名单里的 Origin 才回 CORS 头。
//
// 常用环境变量：
//
//	PLCBRIDGE_CORS_ORIGINS=http://10.0.0.5:8080,https://ops.example.com
//	PLCBRIDGE_CORS_ORIGINS=*    # 允许任意源，但不带凭证（浏览器规范要求）
func corsMiddleware() gin.HandlerFunc {
	return httpx.CORS(secconfig.CORSOrigins())
}
