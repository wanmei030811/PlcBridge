// write_proxy.go：把回写（write）请求从 web 端代理到 plcbridge 主进程。
//
// 为什么需要代理：
//
//	回写能力（write worker / writeQueue / PLC.BatchWrite）只存在于
//	plcbridge 主进程（:8082）。web 进程只有 SQLite 配置，
//	没有 write worker —— 在 web 上直接实现回写是不存在的。
//
//	而前端 web/src/api/write.ts 调的是 `/write/batch`，
//	axios baseURL 是 '/api/v1' → 实际打到 web 自己的 :8080。
//	此前 web 没有注册任何 /write 路由 → gin 落到 NoRoute → SPA fallback
//	→ {"code":"not_found"}。
//
//	现场表现：Web 控制台的「批量写入」按钮一点就报"接口不存在"，
//	但用 Bearer Token 直接调 plcbridge :8082 却完全正常 ——
//	非常容易误判成"回写功能没实现"。
//
// 鉴权：跟随 web 后端的 auth group（已登录用户才能调）。
// 跨进程：web 用共享密钥现签一个短期 Bearer Token（见 infra/auth.ServiceToken），
//
//	plcbridge 用同一密钥校验。token 不落盘、不复用。
package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/infra/auth"
)

// plcbridgeWriteTimeout 是 web → plcbridge 回写调用的超时。
//
// 回写是同步等 PLC 应答的，正常的 S7 写是毫秒级；
// 给 30s 是为了容忍"PLC 忙 / 网络抖动导致的重试"，
// 同时不至于让前端 axios（15s）先超时导致"前端报失败但 PLC 其实写成功了"。
const plcbridgeWriteTimeout = 30 * time.Second

// writeProxyHandler 代理 POST /api/v1/write 与 /api/v1/write/batch 到 plcbridge :8082。
//
// 请求体原样透传（限流 BodyLimit 已经限制了大小），
// 响应体原样返回（保持主进程的状态码与业务 JSON 结构）。
func writeProxyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 读请求体：上限沿用主进程约定，避免无限读入。
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, plcbridgeMaxRespBytes))
		if err != nil {
			WriteError(c, ErrInvalidParam.With("读取请求体失败: "+err.Error()))
			return
		}

		targetURL := plcbridgeTargetRuntimeBase + c.FullPath()
		ctx, cancel := context.WithTimeout(c.Request.Context(), plcbridgeWriteTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
		if err != nil {
			WriteError(c, ErrInternal.With("构造 plcbridge 请求失败: "+err.Error()))
			return
		}
		// 现签短期服务令牌（不落盘、不复用）。
		tok, err := auth.ServiceToken(plcbridgeWriteTimeout + time.Minute)
		if err != nil {
			WriteError(c, ErrInternal.With("签发 plcbridge 服务令牌失败: "+err.Error()))
			return
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			WriteError(c, ErrPlcbridgeUnavailable.With("plcbridge 主进程暂不可达: "+err.Error()))
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, plcbridgeMaxRespBytes))
		if err != nil {
			WriteError(c, ErrPlcbridgeUnavailable.With("读取 plcbridge 响应失败: "+err.Error()))
			return
		}

		// 主进程统一返回 {code,msg} 或业务 JSON；这里原样透传，
		// 前端 axios 拦截器按 status 分别处理。
		c.Data(resp.StatusCode, "application/json; charset=utf-8", respBody)
	}
}
