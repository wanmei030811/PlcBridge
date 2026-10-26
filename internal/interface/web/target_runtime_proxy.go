// target_runtime_proxy.go：plcbridge 主进程（:8082）上的 Target 运行时接口在 web 端代理。
//
// 用途：前端「设备详情 → Target 诊断 / 历史趋势」需要访问主进程独有的能力：
//   - GET /api/v1/targets/:biz_id/diag    Target 诊断（连接状态、收发计数、最后错误）
//   - GET /api/v1/targets/diag            全部 Target 诊断
//   - GET /api/v1/targets/:biz_id/history 历史趋势（仅 InfluxDB）
//
// 这些数据只存在于 plcbridge 主进程内存（forward.Workers 的 client cache），
// web 进程只有 SQLite 配置，没有运行态，所以必须跨进程代理。
//
// 为什么之前会 404：
//
//	前端 web/src/api/history.ts 调 `/targets/${bizId}/history`，
//	而该路由此前只注册在 plcbridge :8082 上，web :8080 根本没有这个 handler
//	→ gin 落到 NoRoute → SPA fallback → {"code":"not_found"}。
//
// 鉴权：跟随 web 后端的 auth group（已登录用户才能调）。
// 跨进程：web 用共享密钥现签一个短期 Bearer Token（见 infra/auth.ServiceToken），
//
//	plcbridge 用同一密钥校验。token 不落盘、不复用。
package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/plcbridge/plcbridge/internal/infra/auth"
)

// plcbridgeTargetRuntimeBase 是 plcbridge 主进程的 API 基址。
//
// 为什么不走 config：主进程地址是编译期约定的（cmd/plcbridge/main.go 里 ":8082"），
// 与 md/architecture.md 的进程拓扑一致。
//
// 声明为 var 而非 const：测试需要临时指向一个不可达地址来验证 503 分支。
var plcbridgeTargetRuntimeBase = "http://127.0.0.1:8082"

// plcbridgeCallTimeout 是 web → plcbridge 单次调用的超时。
//
// 历史查询可能慢（InfluxDB 扫描），给 10s；诊断类给 3s。
const (
	plcbridgeCallTimeout  = 10 * time.Second
	plcbridgeDiagTimeout  = 3 * time.Second
	plcbridgeMaxRespBytes = 16 * 1024 * 1024 // 16MB 上限（历史数据可能较大）
)

// sourceValuesProxyHandler GET /api/v1/sources/:id/values
//
// 代理 plcbridge :8082 的实时点位值接口。
//
// 实时值只存在于主进程内存（collect.Workers 的 pointCache），web 进程只有 SQLite，
// 必须跨进程代理 —— 与 targetRuntime_proxy 同理。
//
// 路径参数装的是 source_biz_id（如 "src_s7_1"），不是 SQLite 行号：
// 实时值存在主进程内存里，主进程只认业务 ID。名字必须叫 :id 而不是 :biz_id ——
// 同组已有 `/sources/:id`，gin 1.9 不允许同一段位置出现不同名通配符（会 panic）。
func sourceValuesProxyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		bizID := c.Param("id")
		if bizID == "" {
			WriteError(c, ErrInvalidParam.With("source_biz_id 必填"))
			return
		}
		targetURL := fmt.Sprintf("%s/api/v1/sources/%s/values", plcbridgeTargetRuntimeBase, url.PathEscape(bizID))
		proxyToCore(c, targetURL, "", plcbridgeDiagTimeout)
	}
}

// deviceValuesProxyHandler GET /api/v1/devices/:id/values
//
// 代理 plcbridge :8082 的设备级实时值聚合接口。
//
// 同上，:id 装的是 device_biz_id 而非行号（主进程只认业务 ID）；
// 名字复用 :id 是为了与同组的 `/devices/:id` 共存。
func deviceValuesProxyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		bizID := c.Param("id")
		if bizID == "" {
			WriteError(c, ErrInvalidParam.With("device_biz_id 必填"))
			return
		}
		targetURL := fmt.Sprintf("%s/api/v1/devices/%s/values", plcbridgeTargetRuntimeBase, url.PathEscape(bizID))
		proxyToCore(c, targetURL, "", plcbridgeDiagTimeout)
	}
}

// targetHistoryProxyHandler GET /api/v1/targets/:id/history
//
// 代理 plcbridge :8082 的同名接口，透传 query string
// （measurement / field / duration / limit）。
//
// 注意：这里的路径参数是 :id，但它装的是 target_biz_id（如 "influxdb"），
// 不是 SQLite 行号——因为前端 history.ts 传的就是业务 ID。
// 之所以复用 :id 这个名字，是因为 gin 不允许同一路径段出现不同名的通配符
// （如 /targets/:id 与 /targets/:biz_id 会 panic）。
// 同一个路由组里 getTarget 用 :id 取行号，本 handler 用 :id 取业务 ID，
// 二者语义不同但不会冲突（各自路由各自取自己的 Param）。
//
// 响应：直接透传主进程的 JSON（{target_biz_id, measurement, count, points:[{t,v}]}）。
// 错误：主进程返回非 2xx 时，把其 {code,msg} 原样透传，保持错误语义一致。
func targetHistoryProxyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		bizID := c.Param("id")
		if bizID == "" {
			WriteError(c, ErrInvalidParam.With("target_biz_id 必填"))
			return
		}
		// measurement 必填（与主进程一致），这里先挡一层，省一次跨进程调用。
		if c.Query("measurement") == "" {
			WriteError(c, ErrInvalidParam.With("measurement 必填"))
			return
		}
		targetURL := fmt.Sprintf("%s/api/v1/targets/%s/history", plcbridgeTargetRuntimeBase, url.PathEscape(bizID))
		proxyToCore(c, targetURL, c.Request.URL.RawQuery, plcbridgeCallTimeout)
	}
}

// targetDiagProxyHandler GET /api/v1/targets/:id/diag
//
// 代理 plcbridge :8082 的 Target 诊断接口。
func targetDiagProxyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		bizID := c.Param("id")
		if bizID == "" {
			WriteError(c, ErrInvalidParam.With("target_biz_id 必填"))
			return
		}
		targetURL := fmt.Sprintf("%s/api/v1/targets/%s/diag", plcbridgeTargetRuntimeBase, url.PathEscape(bizID))
		proxyToCore(c, targetURL, "", plcbridgeDiagTimeout)
	}
}

// targetsDiagProxyHandler GET /api/v1/targets/diag
//
// 代理 plcbridge :8082 的全量 Target 诊断列表。
func targetsDiagProxyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		targetURL := plcbridgeTargetRuntimeBase + "/api/v1/targets/diag"
		proxyToCore(c, targetURL, "", plcbridgeDiagTimeout)
	}
}

// proxyToCore 是通用的「web → plcbridge :8082」代理。
//
//   - 用共享密钥现签 Bearer Token（主进程用同一密钥校验）
//   - 透传 query string
//   - 2xx：原样透传 body + Content-Type
//   - 非 2xx：原样透传 body + 状态码（主进程已是 {code,msg} 格式）
//   - 网络错误 / 超时：503 {code:"plcbridge_unavailable"}
func proxyToCore(c *gin.Context, targetURL, rawQuery string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	fullURL := targetURL
	if rawQuery != "" {
		fullURL = targetURL + "?" + rawQuery
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		WriteError(c, ErrInternal.With("构造 plcbridge 请求失败: "+err.Error()))
		return
	}
	// 现签短期服务令牌（不落盘、不复用）。
	tok, err := auth.ServiceToken(timeout + time.Minute)
	if err != nil {
		WriteError(c, ErrInternal.With("签发 plcbridge 服务令牌失败: "+err.Error()))
		return
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		WriteError(c, ErrPlcbridgeUnavailable.With("plcbridge 主进程暂不可达: "+err.Error()))
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, plcbridgeMaxRespBytes))
	if err != nil {
		WriteError(c, ErrPlcbridgeUnavailable.With("读取 plcbridge 响应失败: "+err.Error()))
		return
	}

	// 主进程统一返回 {code,msg} 或业务 JSON；这里原样透传，
	// 前端 axios 拦截器按 status 分别处理 401 / 其它错误。
	c.Data(resp.StatusCode, "application/json; charset=utf-8", body)
}
