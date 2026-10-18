// Package protocol —— ProtocolChannel 单例缓存层。
//
// 目的：避免每个 Source / Heartbeat Worker 都各自对一个 Channel 建立独立 TCP 连接，
//       并发挤爆 S7 PLC（§6.2.1 协议层硬约束：S7-200 SMART 1~2 / S7-1200 4~6 / S7-1500 8~16）。
//
// 设计：
//   - ChannelRegistry 按 channel_biz_id 缓存 *cachedChannel
//   - 引用计数：每次 Acquire +1，Release -1，归零时延迟 5s 断开（避免抖动）
//   - 配置变更时上层（collect.Workers）调 Invalidate 清掉旧 channel
//   - Connect 失败时返回错误，让上层走 breaker（不阻塞其它 channel）
package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// ErrChannelClosed 表示 Channel 已被 Invalidate，调用方需重新 Acquire。
var ErrChannelClosed = errors.New("protocol channel closed (config invalidated)")

// ChannelRegistry 单例缓存：同一 channel_biz_id 复用同一 ProtocolChannel。
type ChannelRegistry struct {
	mu      sync.Mutex
	entries map[string]*cachedChannel
	// failed 记录 Connect 失败的 channel：保留在 snapshot 中以便 UI 展示失败原因。
	// 独立存放而非放在 entries，避免和"已 connect / 已 disconnect"语义混淆。
	failed map[string]*failedChannel
	// reconnectAt 记录每个 channel 最近一次 Reconnect 尝试的时间戳。
	//
	// 为什么需要：原 ReconnectThrottle 用 lastErrAt（错误时间）做节流，但 collect worker
	// 1Hz read 失败时会持续刷 lastErrAt，导致节流永远不生效 → 每次 snapshot 都触发 reconnect。
	// gos7 ConnectContext 实际耗时 100~500ms（成功）或 2s（connect timeout 失败），持续触发
	// 既浪费 CPU 又互相阻塞 → 用户感知 ON→OFF 高达 ~2s（snapshot 阻塞在 reconnect）。
	//
	// 改为用 reconnectAt 节流：仅在快照路径控制频率，read 错误流不影响。
	reconnectAt map[string]time.Time
	// reconnectLocks 防止同一 channel 在前一次 reconnect 还在跑时又被新 snapshot 启动一次。
	// 简单 sync.Mutex 即可：snapshot 是 1Hz 串行调用，争用极低；handler 死亡场景下
	// "上次未结束就 skip" 反而是想要的（避免 1Hz 起 2s 阻塞的 reconnect goroutine 堆积）。
	reconnectLocks map[string]*sync.Mutex
}

type cachedChannel struct {
	bizID    string
	protocol string // 协议名（"s7" / "modbus_tcp" / "opcua"），由 Acquire 时记录，Snapshot 用于填 ChannelSnapshot.Protocol
	cli      ProtocolChannel
	refs     int
	closed   bool

	// 最后一次 disconnect 计划时间；refs==0 后延迟 disconnectGrace 再真断
	disconnectAt time.Time
}

// failedChannel 记录 connect 失败但仍希望被 Snapshot 看见的 channel。
// 独立存放而非放在 entries 里，因为 closed 走的是另一条释放路径。
// cli 保留 ProtocolChannel 引用以便重试 Connect（协议层 Connect 幂等）。
type failedChannel struct {
	bizID       string
	protocol    string // 同 cachedChannel.protocol
	cli         ProtocolChannel
	connErr     error
	lastRetryAt time.Time
}

// FailedRetryInterval failed 通道两次自动重试 Connect 之间的最小间隔。
// 100ms 防抖：同 ReconnectThrottle 思路（见下方）。原 3s 让初次 Connect 失败后等 3s 才
// 再尝试，对用户感知 OFF→ON 太长；100ms + 1Hz 前端轮询 → 最坏 ~1.1s。
const FailedRetryInterval = 100 * time.Millisecond

const disconnectGrace = 5 * time.Second

// NewChannelRegistry 创建空 Registry。
func NewChannelRegistry() *ChannelRegistry {
	return &ChannelRegistry{
		entries:        map[string]*cachedChannel{},
		failed:         map[string]*failedChannel{},
		reconnectAt:    map[string]time.Time{},
		reconnectLocks: map[string]*sync.Mutex{},
	}
}

// Acquire 取出/创建 channel_biz_id 对应的 ProtocolChannel（引用计数 +1）。
//
// 同一 channel_biz_id 多次 Acquire 返回同一个 ProtocolChannel。
// 调用方在不再使用时必须调 Release。
func (r *ChannelRegistry) Acquire(ctx context.Context, ch *platform.Channel) (ProtocolChannel, error) {
	if ch == nil {
		return nil, fmt.Errorf("channel 为 nil")
	}

	r.mu.Lock()
	if cached, ok := r.entries[ch.ChannelBizID]; ok {
		if cached.closed {
			r.mu.Unlock()
			return nil, ErrChannelClosed
		}
		cached.refs++
		cli := cached.cli
		r.mu.Unlock()
		return cli, nil
	}

	// 创建新通道
	plugin, err := Get(ch.Type)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	cli, err := plugin.CreateChannel(ch)
	if err != nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("CreateChannel %s 失败: %w", ch.ChannelBizID, err)
	}
	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	connectErr := cli.Connect(connectCtx)
	cancel()
	if connectErr != nil {
		// 释放底层 socket 避免泄漏
		_ = cli.Disconnect()
		// 把失败记到 failed 表，snapshot 仍能展示给用户；调用方拿 error 退出（契约不变）。
		r.failed[ch.ChannelBizID] = &failedChannel{
			bizID:       ch.ChannelBizID,
			protocol:    ch.Type,
			cli:         cli,
			connErr:     connectErr,
			lastRetryAt: time.Now(),
		}
		r.mu.Unlock()
		logging.Warn(logging.EventChannelConnectFail).
			Str("channel", ch.ChannelBizID).
			Str("type", ch.Type).
			Str("address", ch.Address).
			Err(connectErr).
			Msg("channel Connect 失败，记录到失败表")
		return nil, fmt.Errorf("Connect %s 失败: %w", ch.ChannelBizID, connectErr)
	}

	r.entries[ch.ChannelBizID] = &cachedChannel{
		bizID:    ch.ChannelBizID,
		protocol: ch.Type,
		cli:      cli,
		refs:     1,
	}
	r.mu.Unlock()

	logging.Info(logging.EventChannelEstablished).
		Str("channel", ch.ChannelBizID).
		Str("type", ch.Type).
		Str("address", ch.Address).
		Msg("channel 已建立")
	return cli, nil
}

// Release 释放引用。refs 归零后延迟 disconnectGrace 真断开，给突发并发一个缓冲窗口。
//
// cli 必传且必须与当前 Acquire 拿到的实例一致，用于防御"配置热重载竞态"：
// 老 worker 持的是旧 cli 句柄，CloseAll 后新 worker 替换了 entry.cli，
// 老 worker 的 defer Release 不能误减新 entry 的引用计数。
func (r *ChannelRegistry) Release(bizID string, cli ProtocolChannel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cached, ok := r.entries[bizID]
	if !ok {
		return
	}
	// cli 不匹配：要么 entry 已被 CloseAll 删除/重建（老 worker 拿到的是孤儿 cli），
	// 要么 cli 被替换为新的（CloseAll 后 Acquire）。两者都不能减 refs。
	if cli != nil && cached.cli != cli {
		return
	}
	if cached.refs <= 0 {
		return
	}
	cached.refs--
	if cached.refs == 0 {
		cached.disconnectAt = time.Now().Add(disconnectGrace)
		go r.maybeClose(bizID, cached.disconnectAt)
	}
}

func (r *ChannelRegistry) maybeClose(bizID string, plannedAt time.Time) {
	time.Sleep(disconnectGrace)
	r.mu.Lock()
	defer r.mu.Unlock()
	cached, ok := r.entries[bizID]
	if !ok || cached.refs > 0 || !cached.disconnectAt.Equal(plannedAt) {
		return
	}
	if cached.closed {
		// 已被 Invalidate 显式断开过，直接清 entry
		delete(r.entries, bizID)
		delete(r.reconnectAt, bizID)
		delete(r.reconnectLocks, bizID)
		return
	}
	if err := cached.cli.Disconnect(); err != nil {
		logging.Warn(logging.EventChannelClosed).
			Str("channel", bizID).
			Err(err).
			Msg("channel Disconnect 出错")
	}
	delete(r.entries, bizID)
	delete(r.reconnectAt, bizID)
	delete(r.reconnectLocks, bizID)
	logging.Info(logging.EventChannelClosed).
		Str("channel", bizID).
		Msg("channel 已断开（引用归零 + 宽限到期）")
}

// Invalidate 强制关闭 channel。下次 Acquire 会重建。
// 用于：配置热更新 / 心跳探测失败强制重连。
//
// 设计要点：保留 entries 引用以确保已发放的 cli 句柄不会再被并发 disconnect；
// 标记 closed 后，新的 Acquire 立即拒绝；旧持有方调 HealthCheck 可发现失效。
//
// 同时清掉 failed 表：下次 Acquire 若重新 Connect 成功，failed 不应继续展示。
func (r *ChannelRegistry) Invalidate(bizID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cached, ok := r.entries[bizID]
	if ok && !cached.closed {
		cached.closed = true
		if err := cached.cli.Disconnect(); err != nil {
			logging.Warn(logging.EventChannelClosed).
				Str("channel", bizID).
				Err(err).
				Msg("Invalidate Disconnect 出错")
		}
		if cached.refs <= 0 {
			delete(r.entries, bizID)
			delete(r.reconnectAt, bizID)
			delete(r.reconnectLocks, bizID)
		}
	}
	// 清掉 failed 表里相同 bizID 的记录
	delete(r.failed, bizID)
	logging.Info(logging.EventChannelInvalidate).
		Str("channel", bizID).
		Msg("channel 已 Invalidate")
}

// CloseAll 进程退出时关闭所有 channel。
func (r *ChannelRegistry) CloseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for bizID, cached := range r.entries {
		if err := cached.cli.Disconnect(); err != nil {
			logging.Warn(logging.EventChannelClosed).
				Str("channel", bizID).
				Err(err).
				Msg("CloseAll Disconnect 出错")
		}
		delete(r.entries, bizID)
		delete(r.reconnectAt, bizID)
		delete(r.reconnectLocks, bizID)
	}
	r.failed = map[string]*failedChannel{}
}

// ChannelSnapshot 是单个 channel 的 runtime 状态快照（只读）。
//
// 通用字段（所有协议都填）：Connected / LastActiveMs / LastError。
// 协议特定数据：Protocol（"s7" / "modbus_tcp" / "opcua" / "mqtt" / "influxdb"）
//               + Stats（map[string]any，由各 driver 的 Stats() 填充）。
//
// Protocol + Stats 让前端可以按 type 渲染不同诊断卡片（s7 看 connection_state + rack/slot，
// opcua 看 session_id，mqtt 看 broker_url + queue_depth 等）。扩展新协议时无需改本结构。
type ChannelSnapshot struct {
	Connected    bool           `json:"connected"`
	LastActiveMs int64          `json:"last_active_ms"` // 最近成功操作的 unix ms；0 表示从未使用
	LastError    string         `json:"last_error"`     // 最近一次错误；空字符串表示无
	Protocol     string         `json:"protocol,omitempty"` // 协议类型（s7 / modbus_tcp / opcua / mqtt / influxdb）
	Stats        map[string]any `json:"stats,omitempty"`     // 协议特定运行时数据（见 ProtocolChannel.Stats 注释）
}

// ProbeIdleThreshold 多久没活动就触发一次 ProbeActive。
// 默认 10s 配合 1Hz 采集（最后一次成功之后 10s 内会被新活动覆盖）。
// 超过这个阈值表示 collect 也没在用了，必须主动发包探活。
const ProbeIdleThreshold = 10 * time.Second

// ReconnectThrottle 错误状态下两次 Reconnect 尝试之间的最小间隔（普通错误路径）。
//
// 历史：
//   v1: 3s（最早，节流 + snapshot 周期对齐）→ 用户感知 ON→OFF ~6-8s
//   v2: 100ms（用 lastErrAt 节流）→ 1Hz read 持续刷 lastErrAt，节流失效
//   v3 (current): 500ms（用 registry.reconnectAt 节流，不受 read 错误流影响）
//
// 设 500ms 理由：
//   - gos7 ConnectContext 实际耗时 100~500ms（成功）/ 2s（connect timeout 失败）
//   - 500ms 保证同一 reconnect goroutine 跑完后再起下一次，避免互相阻塞
//   - 1Hz 前端轮询下 OFF→ON 反映延迟 ≈ 1.5s (worst: read 失败 + snapshot 触发 reconnect
//     + 1Hz 轮询命中)；ON→OFF 反映延迟 ≈ 1s (collect read 失败后第一个 snapshot 触发)
//
// handler 死亡特例（"is null" / "未连接"）走 isHandlerDeadErr 旁路，用更短的
// ReconnectThrottleFast = 200ms，因为 handler 死了 connect 必然快速失败（~100ms），
// 可以更频繁重试。
const ReconnectThrottle = 500 * time.Millisecond
const ReconnectThrottleFast = 200 * time.Millisecond

// isHandlerDeadErr 判断 lastErr 是否表明 protocol handler 已被物理销毁。
// 用于：在 registry snapshot 路径中，区分"网络/PLC 错误（可等 10s 节流后重试）"和
// "handler 死亡（必须立即 Reconnect，不能再 Probe，否则永远卡在 is null）"。
//
// 常见 "handler dead" 错误（来自 gos7 / modbus / opcua 库）：
//   - "Connection to address ... is null"（gos7 TCPClientHandler.Send 看到 mb.conn==nil）
//   - "io: read/write on closed pipe"（底层 net.Conn 已 Close）
//   - "use of closed network connection"（同上，net 包 fmt）
func isHandlerDeadErr(s string) bool {
	if s == "" {
		return false
	}
	low := strings.ToLower(s)
	deadSubstrings := []string{
		"is null",                   // gos7 "Connection to address ... is null"
		"closed pipe",               // io.ErrClosedPipe
		"closed network connection", // net.ErrClosed
		"connection reset",          // RST from peer
		"未连接",                    // plcbridge 内部："S7 channel X 未连接"（client==nil）
	}
	for _, sub := range deadSubstrings {
		if strings.Contains(low, sub) {
			return true
		}
	}
	return false
}

// Snapshot 返回所有缓存 channel 的 runtime 状态快照（带 lazy 探活）。
//
// 包含两类：
//   - entries 里已 connect 的 channel：snapshot + lazy 探活
//   - failed 表里 Connect 失败的 channel：展示 connected=false + last_error，并按节流重试 Connect
//
// 策略：
//   - 已 connect 的 channel 若 last_active > now - ProbeIdleThreshold：直接返回 cached view
//   - 否则：调一次 ProbeActive，再取状态
//   - 失败的 channel 若距上次重试 > FailedRetryInterval：尝试重新 Connect；成功就移到 entries
//
// 调用方：web 仪表盘 / 通道列表状态展示（3s 轮询）。
func (r *ChannelRegistry) Snapshot() map[string]ChannelSnapshot {
	r.mu.Lock()
	ids := make([]string, 0, len(r.entries))
	clis := make(map[string]ProtocolChannel, len(r.entries))
	// protocols 记录每个 channel 的协议名（从 cachedChannel.protocol 复制，避免持锁外访问 entries）
	protocols := make(map[string]string, len(r.entries))
	for id, c := range r.entries {
		if c.closed {
			continue
		}
		ids = append(ids, id)
		clis[id] = c.cli
		protocols[id] = c.protocol
	}
	// failed 表里也复制 cli 引用（与 entries 不重叠）；cli 在 failedChannel 自身
	failedCopy := make(map[string]*failedChannel, len(r.failed))
	for k, v := range r.failed {
		failedCopy[k] = v
	}
	r.mu.Unlock()

	now := time.Now()
	out := make(map[string]ChannelSnapshot, len(ids)+len(failedCopy))

	// 1) 已 connect 的 channel
	for id, cli := range clis {
		last := cli.LastActiveAt()
		var lastMs int64
		if !last.IsZero() {
			lastMs = last.UnixMilli()
		}
		probeErr := error(nil)
		hasErr := cli.LastError() != ""
		// 探活条件：
		//   (a) 长时间无活动 (last.IsZero || now - last > 10s) — 防止 lastUsed 卡在旧值
		//   (b) 持续有错误 — 必须主动发包确认 link 真死。否则 breaker open 时 collect worker
		//       跳过 read，lastErr 也不会被刷新，snapshot 看不到 OFF（实测 3.21s 反映）。
		//
		// (b) 关键：breaker open 10s 期间 collect worker 跳过 read，但底层 socket 可能已死
		// 也可能已恢复。必须主动 AGReadMB 一字节确认。gos7 handler 死时立即返回 "is null"，
		// 成功则清 lastErr 让 connected 反映 ON。
		needProbe := last.IsZero() || now.Sub(last) > ProbeIdleThreshold
		if hasErr {
			needProbe = true
		}
		// needReconnect 触发条件（任一）：
		//   (a) 普通错误（read 失败/连接 refused 等）持续非空 → 调一次 Reconnect 强制重建
		//   (b) handler 死亡（"is null" / "未连接"）→ 立即 Reconnect，否则 read 永远走死路
		//
		// 节流策略：基于 registry 自己的 reconnectAt，而非 lastErrAt。
		// 原因：collect worker 1Hz read 失败会持续刷 lastErrAt，用 lastErrAt 节流则
		// 每次 snapshot 都满足节流条件，每次都启动新 reconnect goroutine，gos7 100~500ms
		// (成功) / 2s (失败) 阻塞互相干扰，UI 看到的状态卡在旧值。
		needReconnect := false
		if hasErr {
			errAt := cli.LastErrorAt()
			if !errAt.IsZero() {
				// (a) 持续错误：节流 ReconnectThrottle（500ms）
				// (b) handler 死亡：走 isHandlerDeadErr 旁路，更短节流 (200ms)
				r.mu.Lock()
				lastRec := r.reconnectAt[id]
				throttle := ReconnectThrottle
				if isHandlerDeadErr(cli.LastError()) {
					throttle = ReconnectThrottleFast
				}
				if now.Sub(lastRec) > throttle {
					needReconnect = true
					// 预占时间戳：先标记"正在 reconnect"，避免下一 tick 又起一次
					r.reconnectAt[id] = now
				}
				r.mu.Unlock()
			}
		}
		if needProbe {
			pctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			probeErr = cli.ProbeActive(pctx)
			cancel()
			if probeErr != nil {
				logging.Info(logging.EventSnapshotProbeFail).
					Str("channel", id).
					Err(probeErr).
					Msg("snapshot probe failed")
			}
			last = cli.LastActiveAt()
			if !last.IsZero() {
				lastMs = last.UnixMilli()
			}
		}
		if needReconnect {
			// 拿 reconnect lock 防并发：避免上一次 reconnect 还在跑（gos7 connect 阻塞 2s）
			// 时新 snapshot 又起一个 goroutine，gos7 内部 socket 状态机并发不安全。
			r.mu.Lock()
			lk, ok := r.reconnectLocks[id]
			if !ok {
				lk = &sync.Mutex{}
				r.reconnectLocks[id] = lk
			}
			r.mu.Unlock()
			gotLock := lk.TryLock()
			if gotLock {
				// 重连条件：lastErr 非空且距上次 reconnect > ReconnectThrottle（500ms）。
				// handler 死亡错误走 isHandlerDeadErr 旁路，更短节流 (200ms)。
				// 用一个独立的较长 ctx（3s），避免 read 短 ctx 拖垮 S7 握手。
			rctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			logging.Info(logging.EventSnapshotReconnect).
				Str("channel", id).
				Msg("snapshot reconnect attempt")
			// 强制重建：底层 socket 可能已被 PLC 关死，Connect 幂等不会重连。
			rerr := cli.Reconnect(rctx)
			cancel()
			if rerr == nil {
				logging.Info(logging.EventSnapshotReconnectOK).
					Str("channel", id).
					Msg("snapshot reconnect 成功")
			} else {
				logging.Info(logging.EventSnapshotReconnectFail).
					Str("channel", id).
					Err(rerr).
					Msg("snapshot reconnect 仍失败")
			}
				lk.Unlock()
				// 重新读最新状态
				last = cli.LastActiveAt()
				if !last.IsZero() {
					lastMs = last.UnixMilli()
				}
			}
			// 拿不到锁的分支：上一次 reconnect 还在跑，本轮跳过 — 但 out[id] 仍会
			// 用当前 cli 状态（connected, lastError）填充，不影响 UI。
		}
		connected := cli.IsConnected() && probeErr == nil && cli.LastError() == ""
		// Stats() 失败也不能让整个 snapshot 崩（driver 实现 bug 防御性）。
		stats := safeStats(cli)
		out[id] = ChannelSnapshot{
			Connected:    connected,
			LastActiveMs: lastMs,
			LastError:    cli.LastError(),
			Protocol:     protocols[id],
			Stats:        stats,
		}
	}

	// 2) Connect 失败的 channel —— 节流重试 Connect
	for id, f := range failedCopy {
		// 若同时已 connect（罕见：先失败后成功），跳过失败记录
		if _, ok := out[id]; ok {
			continue
		}
		// cli 必须有才能重试（Acquire 失败时本应存进 failedChannel；当前实现漏了，下面修）
		cli := f.cli
		if cli != nil && now.Sub(f.lastRetryAt) > FailedRetryInterval {
			logging.Info(logging.EventChannelConnectFail).
				Str("channel", id).
				Dur("since_last_retry", now.Sub(f.lastRetryAt)).
				Msg("snapshot retry Connect")
			retryCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			retryErr := cli.Connect(retryCtx)
			cancel()
			r.mu.Lock()
			if retryErr == nil {
				// 成功：挪到 entries，删 failed
				r.entries[id] = &cachedChannel{
					bizID:    id,
					protocol: f.protocol,
					cli:      cli,
					refs:     0,
				}
				delete(r.failed, id)
				r.mu.Unlock()
				logging.Info(logging.EventChannelEstablished).
					Str("channel", id).
					Msg("channel 重试 Connect 成功")
				out[id] = ChannelSnapshot{
					Connected:    true,
					LastActiveMs: now.UnixMilli(),
					LastError:    "",
					Protocol:     f.protocol,
					Stats:        safeStats(cli),
				}
				continue
			}
			// 仍失败：刷新 lastRetryAt + connErr
			f.connErr = retryErr
			f.lastRetryAt = now
			r.failed[id] = f
			r.mu.Unlock()
			logging.Info(logging.EventChannelConnectFail).
				Str("channel", id).
				Err(retryErr).
				Msg("channel 重试 Connect 仍失败")
		}
		// failed 通道的 Stats 来自 cli（即使 connect 失败，driver 通常还能返回 cli 内部状态）
		var failedStats map[string]any
		if cli != nil {
			failedStats = safeStats(cli)
		}
		out[id] = ChannelSnapshot{
			Connected:    false,
			LastActiveMs: 0,
			LastError:    f.connErr.Error(),
			Protocol:     f.protocol,
			Stats:        failedStats,
		}
	}
	return out
}

// safeStats 安全调用 cli.Stats()，捕获 panic 防止单个 driver bug 把整个 snapshot 拖崩。
func safeStats(cli ProtocolChannel) map[string]any {
	if cli == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			logging.Warn(logging.EventStatsPanic).
				Interface("recover", r).
				Msg("ProtocolChannel.Stats panic")
		}
	}()
	return cli.Stats()
}
