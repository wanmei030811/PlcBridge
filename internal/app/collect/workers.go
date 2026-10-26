// Package collect 实现 plcbridge 的采集应用服务。
//
// 数据流（§5.2）：
//  1. 每个 Source 一个轮询 goroutine（在 collectPool 内）
//  2. 按 CollectIntervalMs 周期触发采集
//  3. 通过 ProtocolChannel.BatchRead 批量读
//  4. 失败点位丢弃（§6.4.2），不重试不补值
//  5. 成功点位 → 按 Target.source_biz_ids+point_biz_ids 过滤 → 入对应 Target 队列
//
// 关键设计：
//   - 历史：曾有熔断器锁 10s（2026-10-12 删除，拖累 ON→OFF 反映延迟 ~10s）
//   - 批量读：协议层（modbus_tcp.BatchRead 按 fc+连续地址打包，opcua.BatchRead 多节点 ReadRequest）已实现；
//     应用层目前按 Source 维度调用协议 BatchRead，由协议层自行决定如何打包。
//     地址连续性打包（应用层预排序后整批下传）在第 2 个月迭代。
package collect

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/metrics"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// CachedPointValue 是点位的最新采集值（Plan3 3.2 实时预览用）。
//
// JSON tag 是**必须的**：本结构直接被 API 层序列化返回给前端，
// 不加 tag 会序列化成 Go 字段名（PointBizID / Value / Timestamp），
// 而前端按 snake_case（point_biz_id / value / timestamp）取 → 全部 undefined。
type CachedPointValue struct {
	PointBizID string `json:"point_biz_id"`
	Address    string `json:"address"`
	Value      any    `json:"value"`
	Quality    string `json:"quality"`
	Timestamp  int64  `json:"timestamp"` // Unix ms
}

// SourceHealthStatus 是 Source 采集健康状态的取值（Plan3 3.2 实时预览用）。
const (
	// SourceHealthOK 表示最近一次批量读成功。
	SourceHealthOK = "ok"
	// SourceHealthError 表示最近一次批量读失败。
	// 注意：此时 Values 里可能仍有历史值（失败不覆盖成功值），必须结合 Stale 判断。
	SourceHealthError = "error"
	// SourceHealthNever 表示从未成功采集过（缓存为空）。
	SourceHealthNever = "never"
)

// SourceHealth 描述单个 Source 的采集健康度。
//
// 为什么需要它：pollOnce 读失败时直接 return，不更新缓存，
// 于是缓存里留着「上一次成功时的旧值」。若只把 values 返回给前端，
// 一个断了 10 分钟的 PLC 会显示成一个 10 分钟前的「正常」数字，用户无从分辨。
// 本结构补上「这个值有多新 / 最近一次采集成功了吗」的语义。
type SourceHealth struct {
	SourceBizID string `json:"source_biz_id"`
	Status      string `json:"status"`
	// LastSuccessMs 最近一次批量读成功的时间（Unix ms），从未成功为 0。
	LastSuccessMs int64 `json:"last_success_ms"`
	// LastErrorMs 最近一次批量读失败的时间（Unix ms），从未失败为 0。
	LastErrorMs int64 `json:"last_error_ms"`
	// LastError 最近一次错误的描述（截断后），无错误为空。
	LastError string `json:"last_error,omitempty"`
	// ConsecutiveErrors 连续失败次数；一次成功即归零。
	ConsecutiveErrors int `json:"consecutive_errors"`
	// IntervalMs 该 Source 的采集间隔，用于推导 stale 阈值。
	IntervalMs int `json:"interval_ms"`
}

// PointValueCache 是 Source 维度点位值的内存缓存。
//
// 设计：
//   - key = sourceBizID
//   - 每个 Source 维护自己的 map[pointBizID]CachedPointValue
//   - 每次 BatchRead 成功后更新缓存（失败不更新 → 旧值保留）
//   - 用于 GET /api/v1/sources/:biz_id/values 实时预览（Plan3 3.2）
type PointValueCache map[string]map[string]CachedPointValue

// Workers 持有所有采集 goroutine。
type Workers struct {
	store *config.Store
	pools *pool.Pools
	// channels 单例缓存：同 channel_biz_id 复用同一连接
	channels *protocol.ChannelRegistry
	// targetRouters 按 target_biz_id 索引 target.Queue（forwardPool 消费）
	targetRouters *target.Router
	// pointCache 点位值缓存（Plan3 3.2 实时预览）
	pointCache      PointValueCache
	pointCacheMutex sync.RWMutex

	// sourceHealth 每个 Source 的采集健康度（Plan3 3.2）。
	// 与 pointCache 共用同一把锁：两者总是一起读写（一次 poll 结果同时更新），
	// 拆成两把锁反而要让调用方自己配对加解锁。
	sourceHealth map[string]*SourceHealth
}

// NewWorkers 创建采集 workers。
func NewWorkers(store *config.Store, pools *pool.Pools) *Workers {
	return &Workers{
		store:         store,
		pools:         pools,
		channels:      protocol.NewChannelRegistry(),
		targetRouters: target.NewRouter(store),
		pointCache:    make(PointValueCache),
		sourceHealth:  make(map[string]*SourceHealth),
	}
}

// Channels 暴露给 write / heartbeat 模块复用同一连接。
func (w *Workers) Channels() *protocol.ChannelRegistry { return w.channels }

// PointValueCache 返回点位值缓存快照（Plan3 3.2 实时预览）。
func (w *Workers) PointValueCache() PointValueCache {
	w.pointCacheMutex.RLock()
	defer w.pointCacheMutex.RUnlock()
	// 深拷贝避免读锁期间被写
	out := make(PointValueCache, len(w.pointCache))
	for srcID, pts := range w.pointCache {
		out[srcID] = make(map[string]CachedPointValue, len(pts))
		for k, v := range pts {
			out[srcID][k] = v
		}
	}
	return out
}

// SourceHealthSnapshot 返回采集健康度快照（Plan3 3.2 实时预览）。
//
// 深拷贝（SourceHealth 内含 string 字段，值拷贝即可），
// 避免调用方持有内部指针后被采集 goroutine 改写。
func (w *Workers) SourceHealthSnapshot() map[string]SourceHealth {
	w.pointCacheMutex.RLock()
	defer w.pointCacheMutex.RUnlock()
	out := make(map[string]SourceHealth, len(w.sourceHealth))
	for k, v := range w.sourceHealth {
		if v != nil {
			out[k] = *v
		}
	}
	return out
}

// markPollSuccess 记录一次成功采集（pollOnce 成功后调用）。
//
// 与 updatePointCache 共用同一把锁，但分两次加解锁：
// 中间不会有其它 goroutine 读出"值已更新但健康度还是旧"的组合状态——
// 即使有，也只会多看到一次旧健康度，不会看到越界的值。
func (w *Workers) markPollSuccess(sourceBizID string, intervalMs int, now int64) {
	w.pointCacheMutex.Lock()
	defer w.pointCacheMutex.Unlock()
	h := w.healthLocked(sourceBizID)
	h.SourceBizID = sourceBizID
	h.Status = SourceHealthOK
	h.LastSuccessMs = now
	h.LastError = ""
	h.ConsecutiveErrors = 0
	h.IntervalMs = intervalMs
}

// markPollFailure 记录一次采集失败（pollOnce 读失败后调用）。
//
// 关键：只更新健康度，不动 pointCache —— 缓存里保留最后一次成功值，
// 由 SourceHealth.Status / ConsecutiveErrors / LastSuccessMs 告诉前端"这是旧值"。
func (w *Workers) markPollFailure(sourceBizID string, intervalMs int, now int64, err error) {
	w.pointCacheMutex.Lock()
	defer w.pointCacheMutex.Unlock()
	h := w.healthLocked(sourceBizID)
	h.SourceBizID = sourceBizID
	h.Status = SourceHealthError
	h.LastErrorMs = now
	h.ConsecutiveErrors++
	h.IntervalMs = intervalMs
	if err != nil {
		// 截断，避免超长错误撑爆 /values 响应。
		msg := err.Error()
		const maxErrLen = 200
		if len(msg) > maxErrLen {
			msg = msg[:maxErrLen] + "…"
		}
		h.LastError = msg
	}
}

// healthLocked 取（或懒创建）某 Source 的健康度记录。
// 调用方必须已持有 pointCacheMutex 写锁。
func (w *Workers) healthLocked(sourceBizID string) *SourceHealth {
	if w.sourceHealth == nil {
		w.sourceHealth = make(map[string]*SourceHealth)
	}
	h, ok := w.sourceHealth[sourceBizID]
	if !ok {
		// 从未成功采集过 → never（pollOnce 首轮就失败时会被 markPollFailure 覆盖为 error）。
		h = &SourceHealth{SourceBizID: sourceBizID, Status: SourceHealthNever}
		w.sourceHealth[sourceBizID] = h
	}
	return h
}

// updatePointCache 更新单个 Source 的点位值缓存。
func (w *Workers) updatePointCache(sourceBizID string, values map[string]protocol.Value, points []*platform.Point) {
	w.pointCacheMutex.Lock()
	defer w.pointCacheMutex.Unlock()
	if _, ok := w.pointCache[sourceBizID]; !ok {
		w.pointCache[sourceBizID] = make(map[string]CachedPointValue, len(points))
	}
	now := time.Now().UnixMilli()
	for _, p := range points {
		v, ok := values[p.PointBizID]
		if !ok {
			continue
		}
		w.pointCache[sourceBizID][p.PointBizID] = CachedPointValue{
			PointBizID: p.PointBizID,
			Address:    p.Address,
			Value:      v.AsGoNative(),
			Quality:    v.Quality,
			Timestamp:  now,
		}
	}
}

// TargetRouter 暴露给 forward 模块使用。
func (w *Workers) TargetRouter() *target.Router {
	return w.targetRouters
}

// Start 启动所有采集 worker（监听配置变更，动态启停）。
func (w *Workers) Start(ctx context.Context) {
	go w.runSupervisor(ctx)
	go w.runHealthLoop(ctx)
}

// healthLoopInterval 后台健康巡检间隔。
//
// 为什么需要这个后台循环（2026-10-25 修复）：
// registry.Snapshot() 里才有 ProbeActive + Reconnect 逻辑，而 Snapshot() 之前
// **只被 HTTP 请求触发**（仪表盘 / 通道列表轮询）。后果：
//   - 没人打开页面时，S7 断线后永远不会重连 → 数据采集永久停摆
//   - 页面打开时，重连时机取决于用户是否正好在看某个页面（"只有触发诊断后
//     才能显示异常"的直接原因）
//
// 1s 间隔与前端轮询节奏一致，且 registry 内部已有 ReconnectThrottle（500ms）
// 与 reconnectLocks 兜底，不会造成重连风暴。
const healthLoopInterval = 1 * time.Second

// runHealthLoop 后台周期性拉取通道健康快照。
//
// 它的作用不是"展示"，而是**驱动自愈**：
//   - 通道有持续错误 → registry 内部触发 Reconnect（重建底层 socket）
//   - 通道长时间无活动 → ProbeActive 探活
//
// 有了它，PLC 停机再启动后不需要任何人为操作即可自动恢复。
func (w *Workers) runHealthLoop(ctx context.Context) {
	ticker := time.NewTicker(healthLoopInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Snapshot 内部已做 panic 防护与节流；这里只负责触发。
			_ = w.channels.Snapshot()
		}
	}
}

func (w *Workers) runSupervisor(ctx context.Context) {
	var cancel context.CancelFunc = func() {}
	currentGen := 0

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			cancel()
			return
		case <-ticker.C:
			cfg := w.store.Current()
			if cfg == nil {
				continue
			}
			if cfg.Version != currentGen {
				logging.Info(logging.EventConfigReload).
					Int("version", cfg.Version).
					Msg("配置变更，重建采集 worker")
				cancel()
				// 配置变更 → 旧 channel 失效，强制下次重新建连（避免 rack/slot/地址变更后仍走旧 conn）
				w.channels.CloseAll()
				ctxChild, c := context.WithCancel(ctx)
				cancel = c
				currentGen = cfg.Version
				w.spawnAll(ctxChild, cfg)
				w.pruneRuntimeState(cfg)
			}
		}
	}
}

// pruneRuntimeState 丢弃配置里已不存在的 Source 的缓存与健康度。
//
// 为什么需要：pointCache / sourceHealth 是按 source_biz_id 累积的 map，
// 配置变更（删除 Source、克隆设备后改 ID）后旧 key 不会被清理，
// 于是 /values 仍会返回已删除 Source 的幽灵数据，且健康度永远停在旧值。
// 每次配置重载后按新配置里的 Source 全集裁剪一次。
func (w *Workers) pruneRuntimeState(cfg *platform.Config) {
	live := make(map[string]struct{})
	for _, dev := range cfg.Devices {
		if !dev.Enabled {
			// 停用的 Device 不再采集，其 Source 视为已下线（否则界面会一直显示旧值）。
			continue
		}
		for _, src := range dev.Sources {
			live[src.SourceBizID] = struct{}{}
		}
	}
	w.pointCacheMutex.Lock()
	defer w.pointCacheMutex.Unlock()
	for id := range w.pointCache {
		if _, ok := live[id]; !ok {
			delete(w.pointCache, id)
		}
	}
	for id := range w.sourceHealth {
		if _, ok := live[id]; !ok {
			delete(w.sourceHealth, id)
		}
	}
}

// spawnAll 启动所有 Source 的采集 goroutine。
func (w *Workers) spawnAll(ctx context.Context, cfg *platform.Config) {
	for _, dev := range cfg.Devices {
		if !dev.Enabled {
			continue
		}
		for _, src := range dev.Sources {
			src := src
			dev := dev
			_ = w.pools.SubmitCollectBlocking(ctx, "collect-"+src.SourceBizID, func(ctx context.Context) {
				w.runSourceCollector(ctx, dev, src)
			})
		}
	}
}

// runSourceCollector 单个 Source 的采集主循环。
func (w *Workers) runSourceCollector(ctx context.Context, dev *platform.Device, src *platform.Source) {
	interval := time.Duration(src.CollectIntervalMs) * time.Millisecond
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	timeout := time.Duration(src.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 300 * time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Acquire 失败时（PLC 上电延迟 / 网络未就绪 / 初次启动 PLC 未上线），不要直接 return。
	// 历史：worker 退出后，只有 supervisor 检测到 config version 变化才会重启，期间
	// Hsl 上线后 Snapshot.failed→entries 转换不会唤醒 worker → 状态卡 ON。
	// 现在：失败时 sleep 1s 重试，直到成功或 ctx 取消。
	//
	// 边界：把 retry 放在 lookupCollectPlugin 内部，Acquire 成功（cli 连上）后才进入
	// 采集循环；Acquire 持续失败则死循环，CPU 占用可控（sleep 1s）。
	var plugin protocol.ProtocolChannel
	for {
		var err error
		plugin, err = lookupCollectPlugin(ctx, w.channels, w.store, src.ChannelBizID)
		if err == nil {
			break
		}
		logging.Warn(logging.EventDeviceConnectFail).
			Str("source", src.SourceBizID).
			Err(err).
			Msg("采集 worker 启动时 Acquire 失败，将重试")
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
	defer w.channels.Release(src.ChannelBizID, plugin)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollOnce(ctx, dev, src, plugin, timeout)
		}
	}
}

// pollOnce 一次采集周期：批量读 + 过滤 + 入 Target 队列。
func (w *Workers) pollOnce(ctx context.Context, dev *platform.Device, src *platform.Source,
	plugin protocol.ProtocolChannel, timeout time.Duration) {

	batchID := fmt.Sprintf("%s_%s", src.SourceBizID, time.Now().UTC().Format(time.RFC3339))
	traceID := uuid.NewString()

	// 过滤要采的 point
	points := make([]*platform.Point, 0, len(src.Points))
	for _, p := range src.Points {
		if p.CollectEnabled {
			points = append(points, p)
		}
	}
	if len(points) == 0 {
		return
	}

	// 构建本 device 内 point_biz_id → source_biz_id 的反向索引,用于 target 路由判定。
	pointOwnerByBizID := make(map[string]string, len(points))
	for _, p := range points {
		pointOwnerByBizID[p.PointBizID] = src.SourceBizID
	}

	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	values, err := plugin.BatchRead(readCtx, points)
	dur := time.Since(start)
	metrics.CollectDurationSeconds.WithLabelValues(dev.DeviceBizID, src.SourceBizID).Observe(dur.Seconds())
	if err != nil {
		metrics.CollectTotal.WithLabelValues(dev.DeviceBizID, "fail").Inc()
		// 标记健康度为 error（Plan3 3.2）。缓存不动 —— 保留最后一次成功值，
		// 由 SourceHealth 告诉前端"这个值有多旧"，避免 PLC 掉线后界面还显示正常数字。
		w.markPollFailure(src.SourceBizID, src.CollectIntervalMs, time.Now().UnixMilli(), err)
		logging.Warn(logging.EventPointReadFail).
			Str("source", src.SourceBizID).
			Str("trace_id", traceID).
			Str("batch_id", batchID).
			Dur("duration", dur).
			Err(err).
			Msg("批量读失败")
		return
	}
	metrics.CollectTotal.WithLabelValues(dev.DeviceBizID, "success").Inc()

	// 更新点位值缓存（Plan3 3.2）
	w.updatePointCache(src.SourceBizID, values, points)
	w.markPollSuccess(src.SourceBizID, src.CollectIntervalMs, time.Now().UnixMilli())

	// 投递给匹配 Target
	for _, tgt := range dev.Targets {
		if !targetMatchesSource(tgt, src.SourceBizID, pointOwnerByBizID) {
			continue
		}
		queue := w.targetRouters.Get(tgt.TargetBizID)
		if queue == nil {
			logging.Warn(logging.EventTargetQueueFull).
				Str("target", tgt.TargetBizID).
				Str("source", src.SourceBizID).
				Msg("enqueue: target queue nil")
			continue
		}

		matched := matchedPointBizIDs(tgt, points)
		if len(matched) == 0 {
			continue
		}
		logging.Info(logging.EventForwardSendOK).
			Str("target", tgt.TargetBizID).
			Str("source", src.SourceBizID).
			Int("matched", len(matched)).
			Msg("enqueue to target queue")
		for _, pointBizID := range matched {
			v, ok := values[pointBizID]
			if !ok {
				continue
			}
			queue.Enqueue(target.PointValue{
				TraceID:     traceID,
				DeviceBizID: dev.DeviceBizID,
				SourceBizID: src.SourceBizID,
				PointBizID:  pointBizID,
				Timestamp:   time.Now(),
				Value:       v.AsGoNative(), // Plan2 5.1：协议层提供方法替代应用层 switch
				DataType:    v.DataType,
			})
		}
	}

	logging.Info(logging.EventPollBatchEnd).
		Str("source", src.SourceBizID).
		Str("trace_id", traceID).
		Str("batch_id", batchID).
		Int("point_count", len(values)).
		Msg("采集批次完成")
}

// targetMatchesSource 判断 target 是否接收来自 sourceBizID 的数据。
// 仅基于 point 集合推导:target.PointBizIDs = ["*"] 时视为全部 source 接受;
// 否则 target 实际涉及的 source 集合 = 其所选 point 的所属 source。
// pointOwnerByBizID 由调用方预先在 dev 范围内构建,key=point_biz_id, value=source_biz_id。
func targetMatchesSource(t *platform.Target, sourceBizID string, pointOwnerByBizID map[string]string) bool {
	if len(t.PointBizIDs) == 0 {
		return false
	}
	if hasWildcard(*t) {
		return true
	}
	for _, pb := range t.PointBizIDs {
		if pointOwnerByBizID[pb] == sourceBizID {
			return true
		}
	}
	return false
}

func hasWildcard(t platform.Target) bool {
	for _, id := range t.PointBizIDs {
		if id == "*" {
			return true
		}
	}
	return false
}

func matchedPointBizIDs(t *platform.Target, points []*platform.Point) []string {
	all := false
	for _, id := range t.PointBizIDs {
		if id == "*" {
			all = true
			break
		}
	}
	if all {
		ids := make([]string, 0, len(points))
		for _, p := range points {
			ids = append(ids, p.PointBizID)
		}
		return ids
	}
	idSet := map[string]bool{}
	for _, id := range t.PointBizIDs {
		idSet[id] = true
	}
	result := make([]string, 0)
	for _, p := range points {
		if idSet[p.PointBizID] {
			result = append(result, p.PointBizID)
		}
	}
	return result
}

func lookupCollectPlugin(ctx context.Context, registry *protocol.ChannelRegistry, store *config.Store, channelBizID string) (protocol.ProtocolChannel, error) {
	cfg := store.Current()
	if cfg == nil {
		return nil, fmt.Errorf("config 未加载")
	}
	for _, ch := range cfg.Channels {
		if ch.ChannelBizID == channelBizID {
			if ch.Role != "collect" {
				return nil, fmt.Errorf("Channel %s 不是 collect 角色", channelBizID)
			}
			return registry.Acquire(ctx, ch)
		}
	}
	return nil, fmt.Errorf("Channel %s 不存在", channelBizID)
}

// Stop 优雅停止：关闭所有 channel 连接，强制释放底层 socket。
//
// 设计：collect.Workers 的 goroutine 全部由 ctx.Done() 触发退出，
// 本方法额外确保 channel 连接立即断开，避免进程退出时还有未关的 TCP/OPC UA 会话。
func (w *Workers) Stop(timeout time.Duration) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	done := make(chan struct{})
	go func() {
		w.channels.CloseAll()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logging.Warn(logging.EventServiceStop).
			Msg("collect.Stop 超时，channel 强退")
	}
}

// === 熔断器 (已删除) ===
//
// 历史：2026-10-12 删除。原因 breaker 失败后锁 10s，期间 collect worker 完全跳过 read，
// 导致 ON→OFF 反映延迟 ~10s（实测 8s，因为有窗口滑动）。registry 本身已有
//   - isHandlerDeadErr（识别 handler 死亡） + ReconnectThrottleFast=200ms
//   - snapshot 每 3s 主动 ProbeActive
//   - cli.LastError 跟踪
// 三道保险足够，breaker 过度防御反而拖累用户体验。
//
// 如未来需要重新引入，必须改 half-open 模式：每 1s 试探一次 read，不能整段锁死。
