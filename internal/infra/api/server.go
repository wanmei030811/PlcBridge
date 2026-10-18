// Package api 实现 plcbridge HTTP API（端口 :8082）。
//
// 路由：
//   GET  /health           健康检查
//   GET  /metrics          Prometheus 指标
//   POST /api/v1/write     单点位回写
//   POST /api/v1/write/batch 批量回写
//
// 鉴权：所有 /api/v1/* 路由需 Bearer Token。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/plcbridge/plcbridge/internal/app/write"
	"github.com/plcbridge/plcbridge/internal/infra/auth"
	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/metrics"
)

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
}

// forwardWorkersSnapshotter 是 forward.Workers 的 Snapshot() 方法的接口抽象。
// 避免 api 包依赖 forward 包（forward 又依赖 collect + protocol + target，循环依赖风险）。
type forwardWorkersSnapshotter interface {
	Snapshot() map[string]protocol.ChannelSnapshot
}

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
		Addr:    addr,
		Handler: s.routes(),
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

// Start 启动 HTTP server。
func (s *Server) Start() error {
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

	r.GET("/health", s.health)
	r.GET("/metrics", gin.WrapH(metrics.PromHandler()))
	// /runtime/channels 公开：仅返回连接状态/最近错误/最近活动（不含敏感数据），
	// 供 plcbridge-web 仪表盘代理使用，避免要求 service token。
	r.GET("/runtime/channels", s.channelsRuntime)

	api := r.Group("/api/v1")
	api.Use(s.auth.Middleware())
	{
		api.POST("/write", s.writeSingle)
		api.POST("/write/batch", s.writeBatch)
		api.GET("/channels/runtime", s.channelsRuntime)
	}
	return r
}

// === /health ===

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
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
	TimeoutMs     int             `json:"timeout_ms"`
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
	CompensationTriggered bool              `json:"compensation_triggered"`
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
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config not loaded"})
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "device disabled"})
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
		c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
		return
	}
	if channelBizID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no write-enabled source for device"})
		return
	}

	cli, err := GetOrCreateChannel(s.collectChannels, s.store, channelBizID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "channel unavailable: " + err.Error()})
		return
	}
	defer s.collectChannels.Release(channelBizID, cli)

	timeoutMs := req.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = 5000
	}

	// 2. 预先读取每条 write 之前的原值，存入 compensations
	compensations := make([]batchCompensationEntry, 0, len(req.Writes))
	// 需要从 cfg 查 address / dataType
	addrByPoint := map[string]struct {
		Address  string
		DataType string
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
							Address  string
							DataType string
						}{pt.Address, pt.DataType}
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "point not found: " + w.PointBizID})
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
			pointBizID: w.PointBizID,
			address:    addr.Address,
			dataType:   addr.DataType,
			origValue:  v,
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
	origValue  protocol.Value
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
		if err := cli.Write(ctx, comp.address, rollbackVal, comp.dataType); err != nil {
			logging.Error(logging.EventWriteFail).
				Str("trace_id", traceID).
				Str("point", comp.pointBizID).
				Str("address", comp.address).
				Err(err).
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

// jsonError 返回统一错误响应。
func jsonError(c *gin.Context, status int, err error) {
	c.JSON(status, gin.H{"error": err.Error()})
}

var _ = json.Marshal
