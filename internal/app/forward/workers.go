// Package forward ?? plcbridge ????????
//
// ????
//
//	collectPool -> Target.Queue -> forwardPool -> Target.Send (MQTT/InfluxDB)
//
// ?????6.4.3???????? WAL????? + ERROR ?? + Prometheus ???
//
// ?? Target ?? forward goroutine?? forwardPool ???
package forward

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/metrics"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// Workers ?????? goroutine?
type Workers struct {
	store  *config.Store
	pools  *pool.Pools
	router *target.Router

	mu    sync.Mutex
	cache *clientCache
	gen   int64 // current cfg generation loaded in cache
}

// NewWorkers ???? workers?
func NewWorkers(store *config.Store, pools *pool.Pools, router *target.Router) *Workers {
	return &Workers{store: store, pools: pools, router: router}
}

// Start ???? Target ??? worker?
func (w *Workers) Start(ctx context.Context) {
	go w.runSupervisor(ctx)
	go w.runHealthLoop(ctx)
}

// targetHealthLoopInterval background target health check interval.
//
// Why this loop exists (2026-10-25 fix):
// Target status (connected / last_error) was only refreshed when something
// called HealthCheck. In practice that meant:
//   - The forward queue was empty (upstream PLC down) -> no Send() -> no error
//     was ever recorded -> InfluxDB "Disconnected" was never noticed.
//   - The only path that probed the target was the manual "diagnose" action.
//
// With this loop the target connection state is refreshed on its own, so the
// UI shows "abnormal" within seconds of the downstream service going down,
// and flips back to "online" as soon as the service comes back.
const targetHealthLoopInterval = 5 * time.Second

// runHealthLoop periodically probes every forward Target client.
//
// HealthCheck implementations are cheap (MQTT: check session; InfluxDB: HTTP
// /ping) and are throttled per client by the protocol layer, so a 5s tick is
// safe even with many targets.
//
// Failures are intentionally NOT logged per tick: a downed InfluxDB would
// otherwise produce a log line every 5s forever. The error is recorded on the
// client itself (LastError) and surfaces through the diagnostics API.
func (w *Workers) runHealthLoop(ctx context.Context) {
	ticker := time.NewTicker(targetHealthLoopInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.probeTargets(ctx)
		}
	}
}

// probeTargets runs HealthCheck against every cached Target client.
func (w *Workers) probeTargets(ctx context.Context) {
	w.mu.Lock()
	cache := w.cache
	w.mu.Unlock()
	if cache == nil {
		return
	}
	for bizID, cli := range cache.snapshot() {
		if cli == nil {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_ = cli.HealthCheck(probeCtx)
		cancel()
		_ = bizID
	}
}

func (w *Workers) runSupervisor(ctx context.Context) {
	var cancel context.CancelFunc = func() {}
	currentGen := -1

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			cancel()
			w.closeClients()
			return
		case <-ticker.C:
			cfg := w.store.Current()
			if cfg == nil {
				logging.Warn(logging.EventServiceStop).
					Msg("runSupervisor: store.Current() == nil")
				continue
			}
			logging.Debug(logging.EventConfigReload).
				Int("cfg_version", cfg.Version).
				Int("currentGen", currentGen).
				Msg("runSupervisor: tick")
			if cfg.Version != currentGen {
				logging.Info(logging.EventConfigReload).
					Int("old_gen", currentGen).
					Int("new_gen", cfg.Version).
					Msg("runSupervisor: cfg 版本变化，触发 spawnAll")
				cancel()
				w.closeClients()
				ctxChild, c := context.WithCancel(ctx)
				cancel = c
				currentGen = cfg.Version
				w.spawnAll(ctxChild, cfg)
			}
		}
	}
}

// closeClients 关闭所有已创建的 Target client（在 reload / shutdown 时调用）。
func (w *Workers) closeClients() {
	w.mu.Lock()
	cache := w.cache
	w.cache = nil
	w.mu.Unlock()
	if cache == nil {
		return
	}
	for bizID, cli := range cache.snapshot() {
		if closer, ok := cli.(target.TargetCloser); ok {
			if err := closer.Close(); err != nil {
				logging.Warn(logging.EventTargetConnectFail).
					Str("target", bizID).Err(err).Msg("关闭 Target client 失败")
			}
		}
	}
}

func (w *Workers) spawnAll(ctx context.Context, cfg *platform.Config) {
	// 1) 计算停用的 target：device.enabled=false 时，相关的 target 应停止转发
	//    （避免 forward worker 闲等 router queue 中缓存的历史数据）
	disabledDevices := make(map[string]bool, len(cfg.Devices))
	for _, dev := range cfg.Devices {
		if !dev.Enabled {
			disabledDevices[dev.DeviceBizID] = true
		}
	}
	disabledTargets := make(map[string]bool)
	for _, dev := range cfg.Devices {
		if !disabledDevices[dev.DeviceBizID] {
			continue
		}
		for _, tgt := range dev.Targets {
			if tgt != nil {
				disabledTargets[tgt.TargetBizID] = true
			}
		}
	}
	if len(disabledTargets) > 0 {
		logging.Info(logging.EventServiceStart).
			Int("disabled_count", len(disabledTargets)).
			Msg("spawnAll: 停用 device 对应的 target 跳过 spawn（closeClients 已在前置调用）")
	}

	queues := w.router.AllQueues()
	cache := newClientCache(cfg)

	w.mu.Lock()
	w.cache = cache
	w.mu.Unlock()

	logging.Info(logging.EventServiceStart).
		Int("queue_count", len(queues)).
		Int("cfg_version", cfg.Version).
		Msg("spawnAll: 被触发")

	for targetBizID, q := range queues {
		q := q
		tBizID := targetBizID
		// 停用 device 的 target：清空其 queue（不 spawn worker），让上游缓存数据
		// 不会持续触发空闲 forwarder
		if disabledTargets[tBizID] {
			logging.Info(logging.EventServiceStart).
				Str("target", tBizID).
				Msg("spawnAll: target 所属 device 已停用，清空 queue 跳过 spawn")
			_ = w.router.ClearQueue(tBizID)
			continue
		}
		logging.Info(logging.EventServiceStart).
			Str("target", tBizID).
			Msg("spawnAll: 发现 target queue")
		client, err := cache.getOrCreate(tBizID)
		if err != nil {
			logging.Error(logging.EventTargetConnectFail).
				Err(err).Str("target", tBizID).Msg("spawnAll: Target client 创建失败")
			continue
		}

		_ = w.pools.SubmitForwardBlocking(ctx, "forward-"+tBizID, func(ctx context.Context) {
			w.runForwarder(ctx, tBizID, q, client)
		}, 100*time.Millisecond)
	}
}

// runForwarder 转发 worker 主体。
func (w *Workers) runForwarder(ctx context.Context, targetBizID string, q *target.Queue, client target.TargetClient) {
	logging.Info(logging.EventServiceStart).Str("target", targetBizID).Msg("forwarder 启动")

	// 启动后立即主动探测连接（不等 Send 触发）。
	// 原因：若上游 PLC 离线，forward 队列永远为空，Send() 不会执行，
	// IsConnected/Stats 永远显示未连接——对运维视角不可见。
	probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := client.ProbeConnect(probeCtx); err != nil {
		logging.Warn(logging.EventTargetConnectFail).
			Str("target", targetBizID).
			Err(err).
			Msg("forwarder 启动时探测连接失败（不影响数据流，等 Send 重试）")
	} else {
		logging.Info(logging.EventTargetConnectOK).
			Str("target", targetBizID).
			Msg("forwarder 启动时探测连接成功")
	}
	probeCancel()

	const batchSize = 100
	const flushInterval = 200 * time.Millisecond

	batch := make([]target.PointValue, 0, batchSize)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := client.Send(sendCtx, batch); err != nil {
			logging.Warn(logging.EventForwardSendFail).
				Str("target", targetBizID).
				Int("batch_size", len(batch)).Err(err).Msg("Target 转发失败")
			metrics.TargetDroppedTotal.WithLabelValues(targetBizID, "send_error").Inc()
		} else {
			metrics.TargetSentTotal.WithLabelValues(targetBizID).Add(float64(len(batch)))
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case <-ticker.C:
			flush()
		default:
			// 上报队列长度（Prometheus）。
			//
			// 为什么在这里埋点：forwarder 是唯一持续消费 target 队列的组件，
			// 它的轮询循环天然是观察队列长度的最佳位置。
			// 队列接近打满（>80%）时 alert PlcbridgeTargetQueueNearFull 会触发。
			if qlen, _, _ := q.Stats(); qlen >= 0 {
				metrics.TargetQueueLength.WithLabelValues(targetBizID).Set(float64(qlen))
			}
			if v, ok := tryDequeue(q); ok {
				batch = append(batch, v)
				if len(batch) >= batchSize {
					flush()
				}
			} else {
				select {
				case <-ctx.Done():
					flush()
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		}
	}
}

func tryDequeue(q *target.Queue) (target.PointValue, bool) {
	select {
	case v, ok := <-q.Raw():
		return v, ok
	default:
		return target.PointValue{}, false
	}
}

func (w *Workers) Stop(timeout time.Duration) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	done := make(chan struct{})
	go func() {
		w.closeClients()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logging.Warn(logging.EventServiceStop).
			Msg("forward.Stop 超时")
	}
}

// Snapshot 返回所有 forward Target 的 runtime 状态快照（key = channel_biz_id）。
//
// 用途：web 仪表盘通道列表（与 collect 通道共用同一份 ChannelSnapshot 形状）。
//
// 聚合策略：同一 forward channel 可被多个 target 引用，IsConnected 走 OR（任一 target 连上即视为活）；
//
//	last_error / Stats 优先返回"出错"的那个 target（多 target 都错时取最后一个）。
//
// 调用方：api.Server.channelsRuntime 每 1s 调一次。
func (w *Workers) Snapshot() map[string]protocol.ChannelSnapshot {
	w.mu.Lock()
	cache := w.cache
	w.mu.Unlock()
	if cache == nil {
		return map[string]protocol.ChannelSnapshot{}
	}

	// 遍历 cfg.Devices[].Targets[]，按 ForwardChannelBizID 聚合。
	// 用一个临时 map 存"该 channel 下所有 target 的 client + 协议名"。
	agg := map[string]*forwardAggEntry{}
	for _, dev := range cache.cfg.Devices {
		for _, tgt := range dev.Targets {
			cli, ok := cache.get(tgt.TargetBizID)
			if !ok {
				continue
			}
			// 找 channel（取协议名）
			var chBizID, chType string
			for _, ch := range cache.cfg.Channels {
				if ch.ChannelBizID == tgt.ForwardChannelBizID {
					chBizID = ch.ChannelBizID
					chType = ch.Type
					break
				}
			}
			if chBizID == "" {
				continue
			}
			entry, ok := agg[chBizID]
			if !ok {
				entry = &forwardAggEntry{protocol: chType}
				agg[chBizID] = entry
			}
			entry.targets = append(entry.targets, tgt.TargetBizID)
			entry.clients = append(entry.clients, cli)
		}
	}

	out := make(map[string]protocol.ChannelSnapshot, len(agg))
	for chBizID, entry := range agg {
		connected := false
		var lastErr string
		var lastErrAt int64
		var firstStats map[string]any
		protocolName := entry.protocol
		for _, cli := range entry.clients {
			if cli.IsConnected() {
				connected = true
			}
			if e := cli.LastError(); e != "" {
				lastErr = e // 聚合：取最后一个非空
			}
			if firstStats == nil {
				firstStats = safeTargetStats(cli)
			}
		}
		// LastActiveMs: 暂用 0（forward 没有 Read/Write 持续活动，仅 Send 触发；可后续加 lastSendOkMs 聚合）
		out[chBizID] = protocol.ChannelSnapshot{
			Connected:    connected && lastErr == "",
			LastActiveMs: 0, // PR1 暂不填；PR2 聚合 lastSendOkMs
			LastError:    lastErr,
			Protocol:     protocolName,
			Stats: map[string]any{
				"target_count": len(entry.clients),
				"targets":      entry.targets,
				"protocol":     protocolName,
				"underlying":   firstStats, // 单 target 的真实 Stats
			},
		}
		_ = lastErrAt
	}
	return out
}

// TargetSnapshot 返回所有 Target 的诊断快照 JSON（Plan3 3.4）。
func (w *Workers) TargetSnapshot() []byte {
	w.mu.Lock()
	cache := w.cache
	w.mu.Unlock()
	type ts struct {
		TargetBizID string `json:"target_biz_id"`
		// Type 是转发协议类型（mqtt / influxdb），来自 Target 所属的 forward Channel。
		Type string `json:"type"`
		// Format 是数据序列化格式（json / line_protocol / raw），来自 Target 配置。
		Format        string         `json:"format"`
		Connected     bool           `json:"connected"`
		LastError     string         `json:"last_error"`
		Stats         map[string]any `json:"stats"`
		QueueLen      int            `json:"queue_len"`
		QueueCap      int            `json:"queue_cap"`
		DroppedTotal  int64          `json:"dropped_total"`
		EnqueuedTotal int64          `json:"enqueued_total"`
	}
	var out []ts
	if cache != nil {
		// channelType 查 forward channel 的协议类型。
		// 为什么需要：Target 只存 forward_channel_biz_id，协议类型声明在 Channel 上。
		channelType := func(channelBizID string) string {
			for _, ch := range cache.cfg.Channels {
				if ch.ChannelBizID == channelBizID {
					return ch.Type
				}
			}
			return ""
		}
		for _, dev := range cache.cfg.Devices {
			for _, tgt := range dev.Targets {
				cli, ok := cache.get(tgt.TargetBizID)
				if !ok {
					continue
				}
				stats := safeTargetStats(cli)
				q := w.router.Get(tgt.TargetBizID)
				queueLen, dropped, enqueued := 0, int64(0), int64(0)
				queueCap := 0
				if q != nil {
					l, d, e := q.Stats()
					queueLen, dropped, enqueued = l, int64(d), int64(e)
					queueCap = q.Cap()
				}
				out = append(out, ts{
					TargetBizID:   tgt.TargetBizID,
					Type:          channelType(tgt.ForwardChannelBizID),
					Format:        tgt.Format,
					Connected:     cli.IsConnected(),
					LastError:     cli.LastError(),
					Stats:         stats,
					QueueLen:      queueLen,
					QueueCap:      queueCap,
					DroppedTotal:  dropped,
					EnqueuedTotal: enqueued,
				})
			}
		}
	}
	data, _ := json.Marshal(map[string]any{"targets": out})
	return data
}

type forwardAggEntry struct {
	protocol string
	targets  []string
	clients  []target.TargetClient
}

// QueryHistory 通过指定 target 查询历史数据（Plan3 3.5）。
//
// 只对实现了 target.HistoryQueryer 的 Target 有效（InfluxDB 的 V1 与 V2 都实现了）。
// 返回 ErrHistoryNotSupported 表示该 target 不支持历史查询。
func (w *Workers) QueryHistory(ctx context.Context, targetBizID string, q target.HistoryQuery) (any, error) {
	w.mu.Lock()
	cache := w.cache
	w.mu.Unlock()
	if cache == nil {
		return nil, ErrHistoryNotSupported
	}
	cli, ok := cache.get(targetBizID)
	if !ok {
		return nil, ErrHistoryNotSupported
	}
	hq, ok := cli.(target.HistoryQueryer)
	if !ok {
		return nil, ErrHistoryNotSupported
	}
	return hq.QueryHistory(ctx, q)
}

// ErrHistoryNotSupported 表示该 target 不支持历史数据查询。
var ErrHistoryNotSupported = errors.New("target does not support history query")

// safeTargetStats 包装 cli.Stats() 防止 panic。
func safeTargetStats(cli target.TargetClient) map[string]any {
	if cli == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			logging.Warn(logging.EventTargetStatsPanic).
				Interface("recover", r).
				Msg("TargetClient.Stats panic")
		}
	}()
	return cli.Stats()
}

// === clientCache ===

// clientCache 缓存 Target client。
//
// 为什么需要锁（2026-10-25 修复）：
// 后台健康巡检（runHealthLoop → probeTargets）会**并发读** cache.clients，
// 而 spawnAll 里的 getOrCreate 会**并发写**同一张 map。
// Go 的 map 并发读写会直接 panic（fatal error: concurrent map read and map write），
// 现场表现为 plcbridge 进程在配置热重载 + 目标探活同时发生时随机崩溃。
type clientCache struct {
	cfg     *platform.Config
	mu      sync.RWMutex
	clients map[string]target.TargetClient
}

func newClientCache(cfg *platform.Config) *clientCache {
	return &clientCache{cfg: cfg, clients: map[string]target.TargetClient{}}
}

// snapshot 返回 client 列表的副本（并发安全）。
func (c *clientCache) snapshot() map[string]target.TargetClient {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]target.TargetClient, len(c.clients))
	for k, v := range c.clients {
		out[k] = v
	}
	return out
}

// get 返回指定 target 的 client（并发安全）。
func (c *clientCache) get(targetBizID string) (target.TargetClient, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cli, ok := c.clients[targetBizID]
	return cli, ok
}

func (c *clientCache) getOrCreate(targetBizID string) (target.TargetClient, error) {
	c.mu.RLock()
	cli, ok := c.clients[targetBizID]
	c.mu.RUnlock()
	if ok {
		return cli, nil
	}

	for _, dev := range c.cfg.Devices {
		for _, tgt := range dev.Targets {
			if tgt.TargetBizID != targetBizID {
				continue
			}
			var channel *platform.Channel
			for _, ch := range c.cfg.Channels {
				if ch.ChannelBizID == tgt.ForwardChannelBizID {
					channel = ch
					break
				}
			}
			if channel == nil {
				return nil, &NotFoundError{Entity: "forward channel", ID: tgt.ForwardChannelBizID}
			}
			plugin, err := target.Get(channel.Type)
			if err != nil {
				return nil, err
			}
			cli, err := plugin.CreateTarget(tgt, channel)
			if err != nil {
				logging.Error(logging.EventTargetConnectFail).
					Err(err).Str("target", targetBizID).
					Str("channel", channel.ChannelBizID).
					Str("type", channel.Type).
					Msg("getOrCreate: CreateTarget 失败")
				return nil, err
			}
			logging.Info(logging.EventTargetConnectOK).
				Str("target", targetBizID).
				Str("channel", channel.ChannelBizID).
				Str("type", channel.Type).
				Send()
			c.mu.Lock()
			c.clients[targetBizID] = cli
			c.mu.Unlock()
			return cli, nil
		}
	}
	return nil, &NotFoundError{Entity: "target", ID: targetBizID}
}

type NotFoundError struct {
	Entity string
	ID     string
}

func (e *NotFoundError) Error() string {
	return e.Entity + " not found: " + e.ID
}
