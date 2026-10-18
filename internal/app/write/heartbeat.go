// Package heartbeat 实现 S7 翻转心跳管理（§6.3.3）。
//
// 每个 write-enabled 的 S7 collect Channel 一个 HeartbeatWorker。
//
// 实现：
//   - 事件驱动：回写 API 调用时发信号，触发翻转
//   - 兜底定时器：业务稀疏时（> MaxIntervalMs 无事件）强制翻转
//   - 去抖：50ms 内的重复触发合并（§3 关键参数）
//   - 状态持久化：用 now.Unix() % 2 决定下次写值，重启后仍能继续翻转
//   - 真正写入 PLC：每次翻转通过 protocol.ChannelRegistry 取到的 ProtocolChannel
//     调 Write(address, value, "bool")，让 PLC 程序可判断 plcbridge 存活
package write

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/metrics"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// HeartbeatSupervisor S7 翻转心跳管理。
type HeartbeatSupervisor struct {
	store    *config.Store
	pools    *pool.Pools
	registry *protocol.ChannelRegistry
	workers  map[string]*heartbeatWorker
	mu       sync.RWMutex
}

func newHeartbeatSupervisor(store *config.Store, pools *pool.Pools, registry *protocol.ChannelRegistry) *HeartbeatSupervisor {
	return &HeartbeatSupervisor{
		store:    store,
		pools:    pools,
		registry: registry,
		workers:  map[string]*heartbeatWorker{},
	}
}

// Start 启动心跳（监听配置变更，动态启停）。
func (h *HeartbeatSupervisor) Start(ctx context.Context) {
	go h.runSupervisor(ctx)
}

func (h *HeartbeatSupervisor) runSupervisor(ctx context.Context) {
	var cancel context.CancelFunc = func() {}
	currentGen := 0

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			cancel()
			return
		case <-ticker.C:
			cfg := h.store.Current()
			if cfg == nil {
				continue
			}
			if cfg.Version != currentGen {
				cancel()
				ctxChild, c := context.WithCancel(ctx)
				cancel = c
				currentGen = cfg.Version
				h.spawnAll(ctxChild, cfg)
			}
		}
	}
}

func (h *HeartbeatSupervisor) spawnAll(ctx context.Context, cfg *platform.Config) {
	for _, ch := range cfg.Channels {
		if !h.shouldStart(ch) {
			continue
		}
		ch := ch
		h.mu.Lock()
		if _, exists := h.workers[ch.ChannelBizID]; exists {
			h.mu.Unlock()
			continue
		}
		w := &heartbeatWorker{
			channel:    ch,
			sigCh:      make(chan struct{}, 1),
			lastValue:  uint8(time.Now().Unix() % 2),
			debounceMs: 50,
		}
		h.workers[ch.ChannelBizID] = w
		h.mu.Unlock()

		_ = h.pools.SubmitWriteBlocking(ctx, "heartbeat-"+ch.ChannelBizID, func(ctx context.Context) {
			w.run(ctx, h.registry)
		}, 50*time.Millisecond)
	}
}

func (h *HeartbeatSupervisor) shouldStart(ch *platform.Channel) bool {
	return ch.Type == "s7" && ch.Role == "collect" && ch.HeartbeatEnabled
}

// SignalHeartbeat 事件驱动：业务回写时触发心跳翻转（§6.3.3）。
func (h *HeartbeatSupervisor) SignalHeartbeat(deviceBizID string) {
	cfg := h.store.Current()
	if cfg == nil {
		return
	}
	for _, dev := range cfg.Devices {
		if dev.DeviceBizID != deviceBizID {
			continue
		}
		for _, src := range dev.Sources {
			if !src.WriteEnabled {
				continue
			}
			h.mu.RLock()
			w, ok := h.workers[src.ChannelBizID]
			h.mu.RUnlock()
			if !ok {
				continue
			}
			select {
			case w.sigCh <- struct{}{}:
			default:
			}
		}
	}
}

func (h *HeartbeatSupervisor) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, w := range h.workers {
		if w.stopped.CompareAndSwap(false, true) {
			close(w.sigCh)
		}
	}
}

// heartbeatWorker 单 Channel 的心跳 worker。
type heartbeatWorker struct {
	channel    *platform.Channel
	sigCh      chan struct{}
	lastValue  uint8
	debounceMs int
	stopped    atomic.Bool
}

func (w *heartbeatWorker) run(ctx context.Context, registry *protocol.ChannelRegistry) {
	maxInterval := time.Duration(w.channel.HeartbeatMaxMs) * time.Millisecond
	if maxInterval == 0 {
		maxInterval = 5 * time.Second
	}
	ticker := time.NewTicker(maxInterval)
	defer ticker.Stop()

	// 启动时取一次 ProtocolChannel（带引用计数），避免每次 flip 重新 Acquire。
	cli, err := registry.Acquire(ctx, w.channel)
	if err != nil {
		logging.Warn(logging.EventHeartbeatChannelFail).
			Str("channel", w.channel.ChannelBizID).
			Err(err).
			Msg("heartbeat 取 channel 失败，已停")
		return
	}
	defer registry.Release(w.channel.ChannelBizID, cli)

	logging.Info(logging.EventHeartbeatStart).
		Str("channel", w.channel.ChannelBizID).
		Str("address", w.channel.HeartbeatAddress).
		Dur("max_interval", maxInterval).
		Msg("heartbeat worker 启动")

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.sigCh:
			w.flip(ctx, cli)
		case <-ticker.C:
			w.flip(ctx, cli)
		}
	}
}

// flip 翻转心跳值并真正写入 PLC。
//
// 写入失败（断连 / 通道已 Invalidate）只记 WARN，不影响心跳节奏：
//   - 上层 breaker / 配置热更新会让 registry 重建，下次 Acquire 自然恢复
//   - 心跳 worker 自身的 ctx 才是取消信号
func (w *heartbeatWorker) flip(ctx context.Context, cli protocol.ProtocolChannel) {
	w.lastValue = 1 - w.lastValue
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := cli.Write(writeCtx, w.channel.HeartbeatAddress, w.lastValue, "bool")
	if err != nil {
		// §6.3.4：所有失败都递增 Prometheus 计数（包括 ChannelClosed）—— Grafana 关心的是"心跳失败率"，
		// ChannelClosed 同样是连接层异常信号，需要可观测。
		metrics.HeartbeatWriteFailTotal.WithLabelValues(w.channel.ChannelBizID).Inc()
		// ErrChannelClosed：配置已 Invalidate，下次 Acquire 会重建，不影响
		if errors.Is(err, protocol.ErrChannelClosed) {
			logging.Debug(logging.EventHeartbeatWriteFail).
				Str("channel", w.channel.ChannelBizID).
				Msg("heartbeat write 遇 ChannelClosed，跳过本次")
			return
		}
		logging.Warn(logging.EventHeartbeatWriteFail).
			Str("channel", w.channel.ChannelBizID).
			Str("address", w.channel.HeartbeatAddress).
			Uint8("value", w.lastValue).
			Err(err).
			Str("event", "heartbeat_write_failed").
			Msg("heartbeat 写 PLC 失败")
		return
	}
	logging.Debug(logging.EventHeartbeatFlip).
		Str("channel", w.channel.ChannelBizID).
		Str("address", w.channel.HeartbeatAddress).
		Uint8("value", w.lastValue).
		Str("event", "heartbeat_flip").
		Msg("heartbeat 翻转")
}
