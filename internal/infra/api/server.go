// Package api 实现 plcbridge HTTP API（端口 :8082）。
//
// 路由：
//
//	GET  /health           健康检查
//	GET  /metrics          Prometheus 指标
//	POST /api/v1/write     单点位回写
//	POST /api/v1/write/batch 批量回写
//
// 鉴权：所有 /api/v1/* 路由需 Bearer Token。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/plcbridge/plcbridge/internal/app/collect"
	"github.com/plcbridge/plcbridge/internal/app/forward"
	"github.com/plcbridge/plcbridge/internal/app/write"
	"github.com/plcbridge/plcbridge/internal/infra/auth"
	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/httpx"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/infra/secconfig"
	"github.com/plcbridge/plcbridge/internal/infra/swagger"
	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/metrics"
)

// mustMarshal wraps json.Marshal with panic on error (used only in handlers that cannot fail).
func mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic("json.Marshal: " + err.Error())
	}
	return data
}

// Server 是 plcbridge 的 HTTP 服务。
type Server struct {
	store     *config.Store
	pools     *pool.Pools
	writer    *write.Workers
	auth      *auth.Authenticator
	startTime time.Time
	srv       *http.Server

	// collectChannels 注入 collect worker 持有的 ChannelRegistry，避免自己再开一份 cache
	// 引发 reload 时的双重连接 / 资源泄漏。
	collectChannels *protocol.ChannelRegistry

	// forwardWorkers 注入 forward.Workers，用于 /api/v1/channels/runtime 返回 forward 通道状态。
	// 可选注入：nil 时仅返回 collect 通道状态（向后兼容旧部署）。
	forwardWorkers forwardWorkersSnapshotter

	// collectWorkers 注入 collect.Workers，用于 GET /api/v1/sources/:biz_id/values 返回实时值缓存。
	collectWorkers *collect.Workers
	workersMu      sync.Mutex
}

// forwardWorkersSnapshotter 是 forward.Workers 的 Snapshot() 方法的接口抽象。
// 避免 api 包依赖 forward 包（forward 又依赖 collect + protocol + target，循环依赖风险）。
type forwardWorkersSnapshotter interface {
	Snapshot() map[string]protocol.ChannelSnapshot
	// TargetSnapshot 返回 Target 诊断信息（Plan3 3.4），值类型为 json.RawMessage 以避免循环依赖。
	TargetSnapshot() []byte
	// QueryHistory 查询 Target 历史数据（Plan3 3.5）。
	QueryHistory(ctx context.Context, targetBizID string, q target.HistoryQuery) (any, error)
}

// cfg 是本包的默认安全配置（可在 NewServer 外部覆盖）。
//
// 用包级变量而不是每次 NewServer 都读环境变量，是为了让测试可以
// 临时改写并在之后恢复（t.Cleanup）。
var cfg = secconfig.DefaultHTTPServerTimeouts()

// SetTimeouts 覆盖 HTTP 超时配置（测试或特殊部署用）。
func SetTimeouts(t secconfig.HTTPServerTimeouts) { cfg = t }

// NewServer 创建 API server。
func NewServer(store *config.Store, pools *pool.Pools, addr string) *Server {
	a := auth.New(store)
	s := &Server{
		store:     store,
		pools:     pools,
		auth:      a,
		startTime: time.Now(),
	}
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadTimeout:       time.Duration(cfg.ReadTimeout) * time.Second,
		WriteTimeout:      time.Duration(cfg.WriteTimeout) * time.Second,
		IdleTimeout:       time.Duration(cfg.IdleTimeout) * time.Second,
		ReadHeaderTimeout: time.Duration(cfg.ReadHeaderTimeout) * time.Second,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}
	return s
}

// SetWriter 注入回写 workers。
func (s *Server) SetWriter(w *write.Workers) {
	s.writer = w
}

// SetChannelRegistry 注入 collect workers 的 channel registry（用于 batch 回写补偿）。
func (s *Server) SetChannelRegistry(reg *protocol.ChannelRegistry) {
	s.collectChannels = reg
}

// SetForwardWorkers 注入 forward workers（用于 /api/v1/channels/runtime 合并 forward 状态）。
// 接受 interface{ Snapshot() ... } 避免 api 包依赖 forward 包（防循环依赖）。
func (s *Server) SetForwardWorkers(fw forwardWorkersSnapshotter) {
	s.forwardWorkers = fw
}

// SetCollectWorkers 注入 collect workers（用于 GET /api/v1/sources/:biz_id/values 返回实时值缓存）。
func (s *Server) SetCollectWorkers(w *collect.Workers) {
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	s.collectWorkers = w
}

func (s *Server) collect() *collect.Workers {
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	return s.collectWorkers
}

// Start 启动 HTTP server。
//
// 若配置了 TLS（PLCBRIDGE_TLS_CERT / PLCBRIDGE_TLS_KEY），走 HTTPS；
// 否则走明文 HTTP（局域网部署场景，由反代负责 TLS 终结）。
func (s *Server) Start() error {
	certFile, keyFile := secconfig.TLSFiles()
	if certFile != "" && keyFile != "" {
		logging.Info(logging.EventServiceStart).
			Str("addr", s.srv.Addr).
			Str("tls_cert", certFile).
			Msg("HTTP API 启动（HTTPS）")
		return s.srv.ListenAndServeTLS(certFile, keyFile)
	}
	logging.Info(logging.EventServiceStart).Str("addr", s.srv.Addr).Msg("HTTP API 启动")
	return s.srv.ListenAndServe()
}

// Shutdown 优雅停止。
func (s *Server) Shutdown(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

func (s *Server) routes() http.Handler {
	r := gin.New()
	r.Use(gin.Recovery())
	// 安全基线中间件（顺序：恢复 → 安全头 → 限流 → 体限 → 超时 → 业务）。
	//
	// - SecurityHeaders 必须最早，否则出错响应也带不上安全头。
	// - BodyLimit 必须在 handler 读 body 之前。
	// - RateLimiter 在最外层，先把明显超量的请求挡掉，省下游资源。
	r.Use(httpx.SecurityHeaders())
	if rl := httpx.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst); rl != nil {
		r.Use(rl.Middleware())
	}
	r.Use(httpx.BodyLimit(cfg.MaxBodyBytes))
	r.Use(httpx.Timeout(time.Duration(cfg.WriteTimeout) * time.Second))

	r.GET("/health", s.health)
	r.GET("/metrics", gin.WrapH(metrics.PromHandler()))
	// /runtime/channels 公开：仅返回连接状态/最近错误/最近活动（不含敏感数据），
	// 供 plcbridge-web 仪表盘代理使用，避免要求 service token。
	r.GET("/runtime/channels", s.channelsRuntime)

	// Plan3 3.9: Swagger UI（公开）
	r.GET("/swagger", func(c *gin.Context) { c.Redirect(http.StatusMovedPermanently, "/swagger/") })
	r.GET("/swagger/*any", gin.WrapH(swagger.Handler()))

	// Plan3 3.9: OpenAPI YAML spec（公开）
	r.GET("/api/openapi.yaml", gin.WrapH(swagger.OpenAPIHandler()))

	// Plan2 主题 A 3.3 + 3.4：插件元信息接口（公开）
	// 理由：仅返回 name / version / JSON Schema，无敏感数据；前端动态生成协议下拉框 +
	// 配置表单的"事实源唯一"。与 /health 同等公开性。
	r.GET("/api/v1/protocols", s.listProtocols)

	api := r.Group("/api/v1")
	api.Use(s.auth.Middleware())
	{
		api.POST("/write", s.writeSingle)
		api.POST("/write/batch", s.writeBatch)
		api.GET("/channels/runtime", s.channelsRuntime)
		// Plan3 3.2：实时点位值
		api.GET("/sources/:biz_id/values", s.sourceValues)
		// Plan3 3.2：设备级实时值聚合（一次拿全设备的，避免前端并发 N 次）
		api.GET("/devices/:biz_id/values", s.deviceValues)
		// Plan3 3.4：Target 诊断
		api.GET("/targets/:biz_id/diag", s.targetDiag)
		api.GET("/targets/diag", s.targetsDiag)
		// Plan3 3.5：InfluxDB 历史趋势
		api.GET("/targets/:biz_id/history", s.targetHistory)
	}
	return r
}

// === GET /api/v1/protocols ===
//
// 返回已注册 collect 协议 + forward Target 的元信息（Plan2 主题 A 3.3）。
//
// 响应形状：
//
//	{
//	  "collect": [
//	    {"name": "s7",         "version": "1.0.0", "role": "collect", "config_schema": {...}},
//	    {"name": "modbus_tcp", "version": "...",   "role": "collect", "config_schema": {...}},
//	    ...
//	  ],
//	  "forward": [
//	    {"name": "mqtt",     "version": "1.1.0", "role": "forward", "config_schema": {...}},
//	    {"name": "influxdb", "version": "...",   "role": "forward", "config_schema": {...}}
//	  ]
//	}
//
// 设计要点：
//   - 不缓存：每次请求读 registry。registry 在进程生命周期内只增不减（init() 阶段），
//     重复 map 遍历 O(N) 极小（< 10 项），不值得引入缓存一致性复杂度。
//   - role 字段冗余但必要：让前端用一份代码统一处理 collect + forward。
//   - config_schema 字段冗余但必要：让前端用一份代码生成配置表单（v0.1 仅展示，
//     Plan2 后续可在 web/src/components/SchemaForm.vue 实现表单生成器）。
func (s *Server) listProtocols(c *gin.Context) {
	c.Data(http.StatusOK, "application/json; charset=utf-8", mustMarshal(map[string]any{
		"collect": protocol.AllInfo(),
		"forward": target.AllInfo(),
	}))
}

// === GET /api/v1/sources/:biz_id/values ===
//
// Plan3 3.2：实时点位值预览。
// 从 collect.Workers.PointValueCache() 读取最新采集值 + SourceHealthSnapshot() 读取健康度。
//
// 响应形状：
//
//	{
//	  "source_biz_id": "src-1",
//	  "values": {"P1": {point_biz_id, address, value, quality, timestamp}, ...},
//	  "updated_at": 1720000000000,          // 本 source 最新点位值时间戳
//	  "health": {status, last_success_ms, ...} // 见 collect.SourceHealth
//	}
//
// stale 语义（前端标黄/标红就靠它）：
//   - health.status == "ok"    → 值新鲜
//   - health.status == "error" → 最近一次采集失败，values 里是**旧值**，
//     具体多旧看 last_success_ms（没有则看 updated_at）
//   - health.status == "never" → 从未成功采集过，values 为空
func (s *Server) sourceValues(c *gin.Context) {
	bizID := c.Param("biz_id")
	workers := s.collect()
	if workers == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "collector_not_running", "msg": "采集服务未运行"})
		return
	}
	cache := workers.PointValueCache()
	health := workers.SourceHealthSnapshot()
	resp := gin.H{
		"source_biz_id": bizID,
		"values":        map[string]any{},
		"updated_at":    0,
		"health":        healthOf(health, bizID),
	}
	srcValues, ok := cache[bizID]
	if !ok {
		// 没有该 source 的缓存（从未采集到 / 已按配置裁剪）。
		// 注意：仍然回 health，接口形状保持一致。
		c.JSON(http.StatusOK, resp)
		return
	}
	// 找最新时间戳
	var latest int64
	for _, v := range srcValues {
		if v.Timestamp > latest {
			latest = v.Timestamp
		}
	}
	resp["values"] = srcValues
	resp["updated_at"] = latest
	c.JSON(http.StatusOK, resp)
}

// === GET /api/v1/devices/:biz_id/values ===
//
// Plan3 3.2：设备级实时值聚合。
//
// 为什么需要：单 Source 接口存在（/sources/:biz_id/values），但设备详情页要"一屏看全设备"。
// 若前端并发请求 N 个 source，需要 N 次 HTTP + N 份响应解析；这里在服务端一次算完，
// 并顺带给出设备整体健康度（最差的那个 Source 决定整体状态）。
//
// 路由参数用 :biz_id（device_biz_id），不是 SQLite 行号 —— 与 /api/v1/protocols 等
// 面向主进程的接口保持同一寻址方式；web 端代理会做 biz_id → 主进程的直传。
//
// 响应形状：
//
//	{
//	  "device_biz_id": "dev-1",
//	  "sources": {
//	    "src-1": {
//	      "source_biz_id": "src-1",
//	      "values": {...},
//	      "updated_at": 1720000000000,
//	      "health": {...}
//	    }
//	  },
//	  "updated_at": 1720000000000,   // 全部 source 中最新的
//	  "health": {...},               // 设备整体：最差状态优先
//	  "source_count": 2,
//	  "error_count": 1
//	}
func (s *Server) deviceValues(c *gin.Context) {
	bizID := c.Param("biz_id")
	workers := s.collect()
	if workers == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "collector_not_running", "msg": "采集服务未运行"})
		return
	}

	// 按当前配置枚举该 Device 的 Source：只返回配置里真实存在的 Source，
	// 避免把缓存里的幽灵 Source（配置已删）也吐出来。
	srcIDs := s.sourcesOfDevice(bizID)
	if len(srcIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"device_biz_id": bizID,
			"sources":       map[string]any{},
			"updated_at":    0,
			"health":        healthOf(nil, ""),
			"source_count":  0,
			"error_count":   0,
			"exists":        false,
		})
		return
	}

	cache := workers.PointValueCache()
	health := workers.SourceHealthSnapshot()

	sources := make(map[string]any, len(srcIDs))
	var latest int64
	errorCount := 0
	var worst collect.SourceHealth
	haveWorst := false
	for _, sid := range srcIDs {
		h := healthOf(health, sid)
		item := gin.H{
			"source_biz_id": sid,
			"values":        map[string]any{},
			"updated_at":    int64(0),
			"health":        h,
		}
		if vs, ok := cache[sid]; ok {
			var srcLatest int64
			for _, v := range vs {
				if v.Timestamp > srcLatest {
					srcLatest = v.Timestamp
				}
			}
			item["values"] = vs
			item["updated_at"] = srcLatest
			if srcLatest > latest {
				latest = srcLatest
			}
		}
		if h.Status == collect.SourceHealthError {
			errorCount++
		}
		if !haveWorst || healthSeverity(h) > healthSeverity(worst) {
			worst = h
			haveWorst = true
		}
		sources[sid] = item
	}

	overall := collect.SourceHealth{
		SourceBizID:   bizID,
		Status:        collect.SourceHealthNever,
		LastSuccessMs: latest,
	}
	if haveWorst {
		overall = worst
		overall.SourceBizID = bizID // 设备级标识，非某个 source
	}

	c.JSON(http.StatusOK, gin.H{
		"device_biz_id": bizID,
		"sources":       sources,
		"updated_at":    latest,
		"health":        overall,
		"source_count":  len(srcIDs),
		"error_count":   errorCount,
		"exists":        true,
	})
}

// sourcesOfDevice 按当前配置枚举 device_biz_id 下的所有 Source（只含启用的 Device）。
func (s *Server) sourcesOfDevice(deviceBizID string) []string {
	cfg := s.store.Current()
	if cfg == nil {
		return nil
	}
	var out []string
	for _, dev := range cfg.Devices {
		if dev.DeviceBizID != deviceBizID || !dev.Enabled {
			continue
		}
		for _, src := range dev.Sources {
			out = append(out, src.SourceBizID)
		}
	}
	return out
}

// healthOf 取某 Source 的健康度；从未采集过返回 status="never" 的零值。
func healthOf(all map[string]collect.SourceHealth, sourceBizID string) collect.SourceHealth {
	if sourceBizID != "" {
		if h, ok := all[sourceBizID]; ok {
			return h
		}
	}
	return collect.SourceHealth{SourceBizID: sourceBizID, Status: collect.SourceHealthNever}
}

// healthSeverity 给健康状态排序，用于"设备整体取最差的那个 Source"。
func healthSeverity(h collect.SourceHealth) int {
	switch h.Status {
	case collect.SourceHealthNever:
		return 2
	case collect.SourceHealthError:
		return 1
	case collect.SourceHealthOK:
		return 0
	default:
		return 2
	}
}

// === GET /api/v1/targets/:biz_id/diag ===

// targetDiag 返回单个 Target 的诊断信息（Plan3 3.4）。
func (s *Server) targetDiag(c *gin.Context) {
	bizID := c.Param("biz_id")
	if s.forwardWorkers == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "forward_not_running", "msg": "forward 服务未运行"})
		return
	}
	raw := s.forwardWorkers.TargetSnapshot()
	// TargetSnapshot() 返回的是 {"targets":[...]} 对象（不是裸数组），
	// 这里必须按同样的结构解包，否则 json.Unmarshal 会报
	// "cannot unmarshal object into Go value of type []map[string]interface {}"（2026-10-21 修复）。
	var snapshot struct {
		Targets []map[string]any `json:"targets"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "msg": err.Error()})
		return
	}
	for _, t := range snapshot.Targets {
		if t["target_biz_id"] == bizID {
			c.JSON(http.StatusOK, t)
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"code": "target_not_found", "msg": "未找到 Target: " + bizID})
}

// === GET /api/v1/targets/diag ===

// targetsDiag 返回所有 Target 的诊断列表（Plan3 3.4）。
func (s *Server) targetsDiag(c *gin.Context) {
	if s.forwardWorkers == nil {
		c.JSON(http.StatusOK, gin.H{"targets": []any{}})
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", s.forwardWorkers.TargetSnapshot())
}

// === GET /api/v1/targets/:biz_id/history ===

// targetHistory 查询单个 Target 的历史数据（Plan3 3.5）。
//
// 查询参数：
//   - measurement : 必填，InfluxDB measurement 名（点位的 topic 前缀）
//   - field       : 点位字段名，默认 "_value"
//   - duration    : 时间范围，默认 "1h"，最大 "7d"
//   - limit       : 返回点数上限，默认 1000，最大 10000
//
// 响应：
//
//	{ "target_biz_id": "...", "measurement": "...", "points": [ {"t": 1234567890, "v": 1.0} ] }
//
// 实现说明（2026-10-21 修正）：
//
//	本层只做「HTTP 参数解析 + 安全收敛」，具体查询语句由 Target 客户端自行翻译
//	（V2 → Flux，V1 → InfluxQL）。历史不再在此层拼 Flux 串，
//	否则 V1 客户端（version=1.x）永远无法实现 HistoryQueryer。
func (s *Server) targetHistory(c *gin.Context) {
	bizID := c.Param("biz_id")
	if s.forwardWorkers == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "forward_not_running", "msg": "forward 服务未运行"})
		return
	}

	measurement := c.Query("measurement")
	if measurement == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_param", "msg": "measurement 必填"})
		return
	}
	field := c.Query("field")
	if field == "" {
		// field 缺省时，从该 Target 关联的点位里解析出唯一的 field key。
		//
		// 为什么要这样（2026-10-21 修正）：
		//   写入侧把 Point.PointBizID 当作 InfluxDB 的 field key
		//   （见 influxdb.buildInfluxPoint / buildV1LineProtocol），
		//   所以库里根本没有 "_value" 这个 field。历史代码默认 "_value"，
		//   导致查询恒定返回空——用户看到的就是「历史趋势图一直空」。
		//   多个点位时无法用单 field 表示，交给调用方显式指定。
		field = s.resolveTargetField(bizID)
		if field == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"code": "invalid_param",
				"msg":  "该 Target 关联多个点位，请显式指定 field（点位业务 ID）",
			})
			return
		}
	}
	duration := c.DefaultQuery("duration", "1h")
	if !historyDurationRe.MatchString(duration) {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": "invalid_param",
			"msg":  "duration 格式非法（应为 <数字><单位>，单位 s/m/h/d/w）",
		})
		return
	}
	limit := 1000
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	// 安全上限（Plan3 3.5 风险缓解）
	if limit > historyMaxLimit {
		limit = historyMaxLimit
	}
	if limit <= 0 {
		limit = 1000
	}

	rows, err := s.forwardWorkers.QueryHistory(c.Request.Context(), bizID, target.HistoryQuery{
		Measurement: measurement,
		Field:       field,
		Duration:    duration,
		Limit:       limit,
	})
	if err != nil {
		if errors.Is(err, forward.ErrHistoryNotSupported) {
			c.JSON(http.StatusBadRequest, gin.H{
				"code": "history_not_supported",
				"msg":  "该 Target 不支持历史查询（仅 InfluxDB 支持）",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "history_query_failed", "msg": err.Error()})
		return
	}

	// 归一化为 {t, v} 数组，方便前端直接喂给图表库
	type point struct {
		T int64 `json:"t"`
		V any   `json:"v"`
	}
	var points []point
	if list, ok := rows.([]map[string]any); ok {
		for _, r := range list {
			var ts int64
			switch tv := r["_time"].(type) {
			case time.Time:
				ts = tv.UnixMilli()
			case int64:
				ts = tv
			case int:
				ts = int64(tv)
			case float64:
				ts = int64(tv)
			}
			points = append(points, point{T: ts, V: r["_value"]})
		}
	}
	// 按时间升序排序（2026-10-21）。
	//
	// 为什么需要：InfluxDB 的 LIMIT 默认取「最早」的 N 行，要取最近的数据
	// 必须在查询里加 ORDER BY time DESC。查询层因此返回的是「最新在前」，
	// 但折线图要求 x 轴从左到右是时间递增，否则画出来是折返的锯齿。
	// 这里统一在 API 层排成升序，前端拿到即可直接用。
	sort.SliceStable(points, func(i, j int) bool {
		return points[i].T < points[j].T
	})

	c.JSON(http.StatusOK, gin.H{
		"target_biz_id": bizID,
		"measurement":   measurement,
		"field":         field,
		"duration":      duration,
		"count":         len(points),
		"points":        points,
	})
}

// resolveTargetField 从当前配置里解析 target 关联的唯一点位业务 ID。
//
// 返回值可直接用作 InfluxDB 的 field key（写入侧就是这个 key）。
// 找不到 target、target 关联 0 个点位、或关联多个点位时返回空串
// （多点位场景由调用方显式指定 field）。
func (s *Server) resolveTargetField(targetBizID string) string {
	cfg := s.store.Current()
	if cfg == nil {
		return ""
	}
	for _, dev := range cfg.Devices {
		if dev == nil {
			continue
		}
		for _, tgt := range dev.Targets {
			if tgt == nil || tgt.TargetBizID != targetBizID {
				continue
			}
			// PointBizIDs == ["*"] 表示所有点位 → 无法确定单一 field。
			if len(tgt.PointBizIDs) != 1 {
				return ""
			}
			return tgt.PointBizIDs[0]
		}
	}
	return ""
}

// historyDurationRe 匹配 "<数字><单位>" 形式的时间窗口。
//
// 在 API 层就拒绝非法 duration，避免把畸形参数透传到 Target 客户端。
var historyDurationRe = regexp.MustCompile(`^(\d+)([smhdw])$`)

// historyMaxLimit 历史查询返回点数硬上限。
const historyMaxLimit = 10000

func (s *Server) health(c *gin.Context) {
	cfg := s.store.Current()
	version := 0
	if cfg != nil {
		version = cfg.Version
	}
	c.JSON(http.StatusOK, gin.H{
		"status":         "ok",
		"uptime_sec":     int(time.Since(s.startTime).Seconds()),
		"config_version": version,
	})
}

// === GET /api/v1/channels/runtime ===
//
// 返回所有 collect + forward 通道的 runtime 状态快照，供 web 后台展示连接状态。
//
// 形状（PR1 扩展）：
//
//	{
//	  "ok":       true,
//	  "fetched":  "2026-10-08T20:30:00Z",
//	  "channels": {
//	    "s7_plc_main":      { "connected": true, "last_active_ms": ..., "last_error": "",
//	                          "protocol": "s7", "stats": {"connection_state": "Connected", "rack": 0, ...} },
//	    "mqtt_broker_main": { "connected": true, "last_active_ms": 0,   "last_error": "",
//	                          "protocol": "mqtt", "stats": {"connection_state": "Connected", "broker_url": "...", ...} }
//	  }
//	}
//
// 历史：PR1 之前只返回 collect 通道（forward 注释里写"暂未纳入"）。
// PR1 改动：注入 forward.Workers.Snapshot() 后合并；Protocol/Stats 新增。
// 兼容性：旧 web（不读 protocol/stats 字段）继续 work；新 web 可展示协议特定诊断数据。
func (s *Server) channelsRuntime(c *gin.Context) {
	// 1) collect 通道
	collectSnap := map[string]protocol.ChannelSnapshot{}
	if s.collectChannels != nil {
		collectSnap = s.collectChannels.Snapshot()
	}

	// 2) forward 通道（合并到同一 map，同 key 后写覆盖前写 — 实际不会重叠，channel_biz_id 唯一）
	if s.forwardWorkers != nil {
		forwardSnap := s.forwardWorkers.Snapshot()
		for k, v := range forwardSnap {
			collectSnap[k] = v
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":       true,
		"fetched":  time.Now().UTC().Format(time.RFC3339),
		"channels": collectSnap,
	})
}

// === POST /api/v1/write ===

type writeRequest struct {
	TraceID     string `json:"trace_id"`
	DeviceBizID string `json:"device_biz_id" binding:"required"`
	PointBizID  string `json:"point_biz_id"  binding:"required"`
	Value       any    `json:"value"        binding:"required"`
	ValueType   string `json:"value_type"   binding:"required"`
	TimeoutMs   int    `json:"timeout_ms"`
}

type writeResponse struct {
	TraceID    string `json:"trace_id"`
	Result     string `json:"result"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

func (s *Server) writeSingle(c *gin.Context) {
	var req writeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, codeInvalidParam, err.Error())
		return
	}

	if req.TraceID == "" {
		req.TraceID = uuid.NewString()
	}
	resultCh := make(chan write.Result, 1)
	cmd := write.Command{
		TraceID:     req.TraceID,
		DeviceBizID: req.DeviceBizID,
		PointBizID:  req.PointBizID,
		Value:       req.Value,
		DataType:    req.ValueType,
		TimeoutMs:   req.TimeoutMs,
		ResultChan:  resultCh,
		EnqueuedAt:  time.Now(),
	}

	if err := s.writer.Enqueue(cmd); err != nil {
		// 设备没有可用的 write worker（停用 / 未启用回写 / 不存在）——
		// 这是配置或参数问题，必须返回 4xx 而不是 503/504。
		//
		// 不区分的话，用户只能看到"网关超时"，完全猜不到是
		// "这个设备的回写开关没打开"。
		var notWritable *write.NotWritableError
		if errors.As(err, &notWritable) {
			c.JSON(http.StatusBadRequest, writeResponse{
				TraceID: req.TraceID,
				Result:  "fail",
				Error:   notWritable.Error(),
			})
			return
		}
		c.JSON(http.StatusServiceUnavailable, writeResponse{
			TraceID: req.TraceID,
			Result:  "fail",
			Error:   err.Error(),
		})
		return
	}

	timeoutMs := req.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = 500
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutMs)*time.Millisecond+100*time.Millisecond)
	defer cancel()

	select {
	case res := <-resultCh:
		c.JSON(http.StatusOK, writeResponse{
			TraceID:    req.TraceID,
			Result:     res.Result,
			Error:      res.Error,
			DurationMs: res.DurationMs,
		})
	case <-ctx.Done():
		c.JSON(http.StatusGatewayTimeout, writeResponse{
			TraceID: req.TraceID,
			Result:  "timeout",
		})
	}
}

// === POST /api/v1/write/batch ===

type batchWriteRequest struct {
	TraceID       string           `json:"trace_id"`
	DeviceBizID   string           `json:"device_biz_id" binding:"required"`
	Transactional bool             `json:"transactional"`
	TimeoutMs     int              `json:"timeout_ms"`
	Writes        []batchWriteItem `json:"writes" binding:"required"`
}

type batchWriteItem struct {
	PointBizID string `json:"point_biz_id" binding:"required"`
	Value      any    `json:"value"`
	ValueType  string `json:"value_type"`
}

type batchWriteResponse struct {
	TraceID               string             `json:"trace_id"`
	Result                string             `json:"result"`
	CompensationTriggered bool               `json:"compensation_triggered"`
	Writes                []batchWriteResult `json:"writes"`
	DurationMs            int64              `json:"duration_ms"`
}

type batchWriteResult struct {
	PointBizID string `json:"point_biz_id"`
	Result     string `json:"result"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) writeBatch(c *gin.Context) {
	var req batchWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, codeInvalidParam, err.Error())
		return
	}
	if req.TraceID == "" {
		req.TraceID = uuid.NewString()
	}

	start := time.Now()
	results := make([]batchWriteResult, 0, len(req.Writes))

	// === 补偿机制（Plan §8.2 / §12.1 #4）===
	//
	// 批量回写中途失败时，把已经写入的点位回滚到原值，保证 PLC 状态一致。
	// 实现：
	//   1. 拿到设备对应 channel 句柄
	//   2. 每条 write 之前先 Read 原值缓存（compensations slice）
	//   3. 顺序入队；任一失败 → 触发 compensation：按逆序写回原值
	//   4. compensation 自身失败 → 仅记日志，response 标明 compensation_partial
	//
	// 边界：
	//   - 第一个 write 就失败：没东西可补偿，跳过
	//   - 读原值失败：跳过该点位的补偿（已写成功的部分仍尝试回滚）
	//   - 非事务模式（Transactional=false）：失败不回滚，业务方自己处理
	if !req.Transactional {
		// 不需要补偿：跑原始流程
		hasFail := false
		for _, w := range req.Writes {
			resultCh := make(chan write.Result, 1)
			cmd := write.Command{
				TraceID:     req.TraceID,
				DeviceBizID: req.DeviceBizID,
				PointBizID:  w.PointBizID,
				Value:       w.Value,
				DataType:    w.ValueType,
				TimeoutMs:   req.TimeoutMs,
				ResultChan:  resultCh,
				EnqueuedAt:  time.Now(),
			}
			if err := s.writer.Enqueue(cmd); err != nil {
				results = append(results, batchWriteResult{PointBizID: w.PointBizID, Result: "fail", Error: err.Error()})
				hasFail = true
				continue
			}
			timeoutMs := req.TimeoutMs
			if timeoutMs == 0 {
				timeoutMs = 5000
			}
			ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutMs)*time.Millisecond)
			select {
			case res := <-resultCh:
				if res.Result != "success" {
					hasFail = true
				}
				results = append(results, batchWriteResult{PointBizID: w.PointBizID, Result: res.Result, Error: res.Error})
			case <-ctx.Done():
				hasFail = true
				results = append(results, batchWriteResult{PointBizID: w.PointBizID, Result: "timeout"})
			}
			cancel()
		}
		resp := batchWriteResponse{
			TraceID:               req.TraceID,
			Writes:                results,
			DurationMs:            time.Since(start).Milliseconds(),
			CompensationTriggered: false,
		}
		switch {
		case hasFail && len(results) == len(req.Writes):
			resp.Result = "fail"
		case hasFail:
			resp.Result = "partial"
		default:
			resp.Result = "success"
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	// === 事务模式：先读原值，再写，再补偿 ===

	// 1. 找到 device 的 channel
	cfg := s.store.Current()
	if cfg == nil {
		jsonError(c, http.StatusServiceUnavailable, codeConfigNotLoad, "config not loaded")
		return
	}
	var deviceBizID = req.DeviceBizID
	var channelBizID string
	var deviceFound bool
	for _, dev := range cfg.Devices {
		if dev.DeviceBizID != deviceBizID {
			continue
		}
		deviceFound = true
		if !dev.Enabled {
			jsonError(c, http.StatusBadRequest, codeDeviceDisabled, "device disabled")
			return
		}
		// 同一设备允许多 source，取第一个 enabled + write-enabled
		for _, src := range dev.Sources {
			if !src.WriteEnabled {
				continue
			}
			channelBizID = src.ChannelBizID
			break
		}
		break
	}
	if !deviceFound {
		jsonError(c, http.StatusNotFound, codeDeviceNotFound, "device not found")
		return
	}
	if channelBizID == "" {
		jsonError(c, http.StatusBadRequest, codeDeviceDisabled, "no write-enabled source for device")
		return
	}

	cli, err := GetOrCreateChannel(s.collectChannels, s.store, channelBizID)
	if err != nil {
		jsonError(c, http.StatusServiceUnavailable, codeChannelUnavail, "channel unavailable: "+err.Error())
		return
	}
	defer s.collectChannels.Release(channelBizID, cli)

	timeoutMs := req.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = 5000
	}

	// 2. 预先读取每条 write 之前的原值，存入 compensations
	compensations := make([]batchCompensationEntry, 0, len(req.Writes))
	// 需要从 cfg 查 address / dataType / string 长度
	addrByPoint := map[string]struct {
		Address      string
		DataType     string
		StringLength int
	}{}
	for _, dev := range cfg.Devices {
		if dev.DeviceBizID != deviceBizID {
			continue
		}
		for _, src := range dev.Sources {
			for _, pt := range src.Points {
				for _, w := range req.Writes {
					if pt.PointBizID == w.PointBizID {
						addrByPoint[pt.PointBizID] = struct {
							Address      string
							DataType     string
							StringLength int
						}{pt.Address, pt.DataType, pt.StringLength}
					}
				}
			}
		}
	}

	readCtx, readCancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutMs)*time.Millisecond)
	for _, w := range req.Writes {
		addr, ok := addrByPoint[w.PointBizID]
		if !ok {
			readCancel()
			jsonError(c, http.StatusBadRequest, codePointNotFound, "point not found: "+w.PointBizID)
			return
		}
		v, rerr := cli.Read(readCtx, addr.Address, addr.DataType)
		if rerr != nil {
			// 读不到原值：不预存，但继续尝试写入（写入仍会成功；补偿时跳过）
			logging.Warn(logging.EventPointReadFail).
				Str("trace_id", req.TraceID).
				Str("point", w.PointBizID).
				Err(rerr).
				Msg("batch 回写：读原值失败，跳过该点位补偿")
			continue
		}
		compensations = append(compensations, batchCompensationEntry{
			pointBizID:   w.PointBizID,
			address:      addr.Address,
			dataType:     addr.DataType,
			stringLength: addr.StringLength,
			origValue:    v,
		})
	}
	readCancel()

	// 3. 顺序入队；任一失败 → 触发补偿
	var firstErr string
	var compensationTriggered bool
	var compensationPartial bool
	failIdx := -1
	for i, w := range req.Writes {
		resultCh := make(chan write.Result, 1)
		cmd := write.Command{
			TraceID:     req.TraceID,
			DeviceBizID: req.DeviceBizID,
			PointBizID:  w.PointBizID,
			Value:       w.Value,
			DataType:    w.ValueType,
			TimeoutMs:   req.TimeoutMs,
			ResultChan:  resultCh,
			EnqueuedAt:  time.Now(),
		}
		if err := s.writer.Enqueue(cmd); err != nil {
			results = append(results, batchWriteResult{PointBizID: w.PointBizID, Result: "fail", Error: err.Error()})
			firstErr = err.Error()
			failIdx = i
			break
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutMs)*time.Millisecond)
		select {
		case res := <-resultCh:
			if res.Result != "success" {
				firstErr = res.Error
				failIdx = i
				results = append(results, batchWriteResult{PointBizID: w.PointBizID, Result: res.Result, Error: res.Error})
				cancel()
				break
			}
			results = append(results, batchWriteResult{PointBizID: w.PointBizID, Result: "success"})
		case <-ctx.Done():
			firstErr = "timeout"
			failIdx = i
			results = append(results, batchWriteResult{PointBizID: w.PointBizID, Result: "timeout"})
			cancel()
			break
		}
		cancel()
	}

	// 4. 补偿触发条件：事务模式 + 有失败 + 已成功点位有缓存原值
	if req.Transactional && firstErr != "" && failIdx > 0 && len(compensations) > 0 {
		compCtx, compCancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutMs)*time.Millisecond)
		compensationTriggered, compensationPartial = applyBatchCompensation(
			compCtx, req.TraceID, compensations, results, cli,
		)
		compCancel()
	}

	resp := batchWriteResponse{
		TraceID:               req.TraceID,
		Writes:                results,
		DurationMs:            time.Since(start).Milliseconds(),
		CompensationTriggered: compensationTriggered,
	}
	// compensation_partial 透出在 Result 字段
	switch {
	case compensationTriggered && compensationPartial:
		resp.Result = "compensation_partial"
	case compensationTriggered:
		resp.Result = "compensated"
	case firstErr != "" && len(results) == len(req.Writes):
		resp.Result = "fail"
	case firstErr != "":
		resp.Result = "partial"
	default:
		resp.Result = "success"
	}
	c.JSON(http.StatusOK, resp)
}

// rollbackValueFromProtocolValue 从 protocol.Value 反向提取可写回的值。
//
// 根据 dataType 选择正确的 Value 字段：
//   - bool       → orig.Bool
//   - int8/16/32/64 / uint*  → orig.Int64 / orig.Uint64
//   - float32/64 → orig.Float64
//   - string     → orig.String
//
// 边界：未知 dataType / 解析失败 → 返回 orig.Int64（最常见），由 PLC 端按字节解释。
func rollbackValueFromProtocolValue(v protocol.Value, dataType string) any {
	switch dataType {
	case "bool":
		return v.Bool
	case "int8", "int16", "int32", "int64":
		return v.Int64
	case "uint8", "uint16", "uint32", "uint64":
		return v.Uint64
	case "float32", "float64":
		return v.Float64
	case "string":
		return v.String
	default:
		return v.Int64
	}
}

// batchCompensationEntry 内部：补偿记录（pointBizID + address + 原值快照）。
type batchCompensationEntry struct {
	pointBizID string
	address    string
	dataType   string
	// stringLength 是点位声明的字符串长度（仅 string 类型有效）。
	// 补偿回滚时要传给协议层 —— S7 无头布局没有长度字段，
	// 不知道内容区多长就无法正确填充。
	stringLength int
	origValue    protocol.Value
}

// applyBatchCompensation 执行批量回写补偿（Plan §8.2 / §12.1 #4）。
//
// 遍历已写入成功的点位（results 中 Result=="success"），把它们写回缓存的原值。
//
// 返回：
//   - triggered：是否实际触发补偿
//   - partial ：补偿过程中是否有失败（多个点位部分回滚成功）
//
// 抽出来是为了方便单元测试（不依赖 write.Worker / 5s ticker / 全局 sync.Map）。
func applyBatchCompensation(
	ctx context.Context,
	traceID string,
	compensations []batchCompensationEntry,
	results []batchWriteResult,
	cli protocol.ProtocolChannel,
) (triggered, partial bool) {
	successSet := map[string]bool{}
	for _, r := range results {
		if r.Result == "success" {
			successSet[r.PointBizID] = true
		}
	}
	rolledBack := 0
	for _, comp := range compensations {
		if !successSet[comp.pointBizID] {
			continue
		}
		rollbackVal := rollbackValueFromProtocolValue(comp.origValue, comp.dataType)
		var werr error
		if sw, ok := cli.(protocol.StringLengthWriter); ok && comp.dataType == "string" {
			werr = sw.WriteStringLen(ctx, comp.address, rollbackVal, comp.dataType, comp.stringLength)
		} else {
			werr = cli.Write(ctx, comp.address, rollbackVal, comp.dataType)
		}
		if werr != nil {
			logging.Error(logging.EventWriteFail).
				Str("trace_id", traceID).
				Str("point", comp.pointBizID).
				Str("address", comp.address).
				Err(werr).
				Msg("补偿回滚失败")
			partial = true
			continue
		}
		rolledBack++
	}
	logging.Info(logging.EventWritePartial).
		Str("trace_id", traceID).
		Int("rolled_back", rolledBack).
		Int("total_cached", len(compensations)).
		Bool("partial", partial).
		Msg("批量回写补偿触发")
	return true, partial
}

// === 全局计数 ===

var activeConnections atomic.Int64

// ErrUnavailable 在 plcbridge 启动未完成时返回。
var ErrUnavailable = errors.New("plcbridge unavailable")

// GetOrCreateChannel 通过注入的 collect ChannelRegistry 拿/建 ProtocolChannel。
//
// 调用方必须在用完后 defer Release。
func GetOrCreateChannel(reg *protocol.ChannelRegistry, store *config.Store, channelBizID string) (protocol.ProtocolChannel, error) {
	if reg == nil {
		return nil, errors.New("channel registry 未注入")
	}
	cfg := store.Current()
	if cfg == nil {
		return nil, ErrUnavailable
	}
	for _, ch := range cfg.Channels {
		if ch.ChannelBizID == channelBizID {
			acquireCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return reg.Acquire(acquireCtx, ch)
		}
	}
	return nil, errors.New("channel not found: " + channelBizID)
}

// jsonError 返回统一错误响应（Plan2 4.1 一致：{code, msg}）。
//
// 注意：plcbridge :8082 的鉴权/错误格式历史上用 {error: msg}。
// 4.1 统一：所有错误响应都返回 {code, msg}。
// 前端通过 web 端代理间接调用，但直接调用 :8082 的工具链（curl/Postman）也能看到一致格式。
func jsonError(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{
		"code": code,
		"msg":  msg,
	})
}

// 标准 error code（plcbridge :8082 API）。
// 跟 web 端 APIError code 命名风格一致（snake_case），便于客户端统一处理。
const (
	codeInvalidParam   = "invalid_param"
	codeDeviceNotFound = "device_not_found"
	codeDeviceDisabled = "device_disabled"
	codePointNotFound  = "point_not_found"
	codeConfigNotLoad  = "config_not_loaded"
	codeChannelUnavail = "channel_unavailable"
)

var _ = json.Marshal
