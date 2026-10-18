// Package collect 实现 plcbridge 的采集应用服务。
//
// 数据流（§5.2）：
//   1. 每个 Source 一个轮询 goroutine（在 collectPool 内）
//   2. 按 CollectIntervalMs 周期触发采集
//   3. 通过 ProtocolChannel.BatchRead 批量读
//   4. 失败点位丢弃（§6.4.2），不重试不补值
//   5. 成功点位 → 按 Target.source_biz_ids+point_biz_ids 过滤 → 入对应 Target 队列
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

// Workers 持有所有采集 goroutine。
type Workers struct {
	store *config.Store
	pools *pool.Pools
	// channels 单例缓存：同 channel_biz_id 复用同一连接
	channels *protocol.ChannelRegistry
	// targetRouters 按 target_biz_id 索引 target.Queue（forwardPool 消费）
	targetRouters *target.Router
}

// NewWorkers 创建采集 workers。
func NewWorkers(store *config.Store, pools *pool.Pools) *Workers {
	return &Workers{
		store:         store,
		pools:         pools,
		channels:      protocol.NewChannelRegistry(),
		targetRouters: target.NewRouter(store),
	}
}

// Channels 暴露给 write / heartbeat 模块复用同一连接。
func (w *Workers) Channels() *protocol.ChannelRegistry { return w.channels }

// TargetRouter 暴露给 forward 模块使用。
func (w *Workers) TargetRouter() *target.Router {
	return w.targetRouters
}

// Start 启动所有采集 worker（监听配置变更，动态启停）。
func (w *Workers) Start(ctx context.Context) {
	go w.runSupervisor(ctx)
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
			}
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
// valueToAny 把协议层的 Value 按其 DataType 映射到 JSON 友好的 Go 原生类型。
//
// 原因：下游 mqtt/influxdb 序列化时不能直接把 Value struct 塞进 map（会被展开成 7 个字段）。
// 必须从 Value 各字段里挑一个语义对应的：bool→bool，int*→int64，uint*→uint64，float*→float64，string→string。
// 协议层还应当为 bool/int/uint/float 同步填 Float64（便于其它只用 Float64 的旧调用方），但这层只用作"正确类型"的来源。
func valueToAny(v protocol.Value) any {
	switch v.DataType {
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
		// 未知类型（理论上协议层已 normalize），退回 Float64 而不是 0，避免静默丢值。
		return v.Float64
	}
}

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
				Value:       valueToAny(v),
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
