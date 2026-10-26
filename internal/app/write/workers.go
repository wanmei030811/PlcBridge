// Package write ?? plcbridge ????????
//
// ????5.2??
//
//	HTTP /api/v1/write -> writeQueue (? device_biz_id ??) -> writePool -> PLC.BatchWrite
//
// ?????
//   - ? PLC ?? FIFO ???device_biz_id ???6.1?
//   - ??? 1024 ????? 503?6.1?
//   - ??????????writePool?
//   - ???S7 0/1 ?? + ???? + ??????6.3.3?
//   - MVP ????6.4.3????? 503??? WAL
package write

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/plcbridge/plcbridge/internal/infra/config"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/infra/pool"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/metrics"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// Command ????????
type Command struct {
	TraceID     string
	DeviceBizID string
	PointBizID  string
	Value       any
	DataType    string
	TimeoutMs   int
	ResultChan  chan Result
	EnqueuedAt  time.Time
}

// Result ??????
type Result struct {
	Result     string
	Error      string
	DurationMs int64
}

// Workers ?? worker ???
type Workers struct {
	store     *config.Store
	pools     *pool.Pools
	registry  *protocol.ChannelRegistry
	queues    map[string]chan Command
	queuesMu  sync.RWMutex
	heartbeat *HeartbeatSupervisor
}

// NewWorkers ???? workers?
func NewWorkers(store *config.Store, pools *pool.Pools, registry *protocol.ChannelRegistry) *Workers {
	return &Workers{
		store:     store,
		pools:     pools,
		registry:  registry,
		queues:    make(map[string]chan Command),
		heartbeat: newHeartbeatSupervisor(store, pools, registry),
	}
}

// Enqueue ?????????
//
// ????1024??? ErrQueueFull?? API ?? 503?
func (w *Workers) Enqueue(cmd Command) error {
	// 入队前校验：没有 write worker 的 device 写了也永远没人消费。
	//
	// 不校验的话，命令会静静躺在队列里直到 API 超时（504），
	// 用户看到的是"网关超时"而不是"这个设备不支持回写" ——
	// 现场排查方向完全被带偏。
	if err := w.validateEnqueue(cmd); err != nil {
		return err
	}
	q := w.ensureQueue(cmd.DeviceBizID)

	select {
	case q <- cmd:
		w.heartbeat.SignalHeartbeat(cmd.DeviceBizID)
		return nil
	default:
		return ErrQueueFull
	}
}

// validateEnqueue 校验命令是否有对应的 write worker 可以消费。
//
// 判定依据与 spawnAll 完全一致（enabled + write_enabled），
// 保证"能入队"等价于"会被消费"。
func (w *Workers) validateEnqueue(cmd Command) error {
	cfg := w.store.Current()
	if cfg == nil {
		return &NotWritableError{Reason: "配置未加载"}
	}
	var found bool
	for _, dev := range cfg.Devices {
		if dev.DeviceBizID != cmd.DeviceBizID {
			continue
		}
		found = true
		if !dev.Enabled {
			return &NotWritableError{Reason: "设备已停用"}
		}
		if !deviceWriteEnabled(dev) {
			return &NotWritableError{Reason: "设备未启用回写（source.write_enabled=false）"}
		}
		return nil
	}
	if !found {
		return &NotWritableError{Reason: "设备不存在"}
	}
	return nil
}

// NotWritableError 表示该设备没有可用的 write worker。
//
// API 层应把它映射成 4xx（配置/参数问题），而不是 504 超时 ——
// 用户能立刻知道"需要先启用回写"，而不是怀疑网络或 PLC。
type NotWritableError struct {
	Reason string
}

func (e *NotWritableError) Error() string {
	return "设备不可回写: " + e.Reason
}

// ensureQueue 返回该 device 的写队列，不存在则创建。
//
// 关键：queue 必须由"一定会有人消费它"的一方创建 ——
// Enqueue（入队方）和 runWriter（消费方）都走这里，
// 这样 queue 的生命周期与 worker 严格对齐，不会出现
// "命令躺在无人消费的队列里" 的情况。
func (w *Workers) ensureQueue(bizID string) chan Command {
	w.queuesMu.RLock()
	q, ok := w.queues[bizID]
	w.queuesMu.RUnlock()
	if ok {
		return q
	}
	w.queuesMu.Lock()
	defer w.queuesMu.Unlock()
	q, ok = w.queues[bizID]
	if !ok {
		q = make(chan Command, 1024)
		w.queues[bizID] = q
	}
	return q
}

// ErrQueueFull ??????
var ErrQueueFull = &QueueFullError{}

type QueueFullError struct{}

func (e *QueueFullError) Error() string { return "write queue full" }

// Start ???? worker + ???
func (w *Workers) Start(ctx context.Context) {
	go w.runSupervisor(ctx)
	w.heartbeat.Start(ctx)
}

// TriggerSpawnNow 立即触发一次 worker spawnAll（跳过 ticker 等待）。
//
// 主要用于测试：避免等 5s ticker。
// 生产代码不应调用此方法。
func (w *Workers) TriggerSpawnNow(ctx context.Context) {
	cfg := w.store.Current()
	if cfg == nil {
		return
	}
	w.spawnAll(ctx, cfg)
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
			w.pruneQueues(nil) // 清空
			return
		case <-ticker.C:
			cfg := w.store.Current()
			if cfg == nil {
				continue
			}
			if cfg.Version != currentGen {
				cancel()
				// reload 时只保留当前 cfg 仍启用的 device queue；其他删掉避免泄漏
				w.pruneQueues(cfg)
				ctxChild, c := context.WithCancel(ctx)
				cancel = c
				currentGen = cfg.Version
				w.spawnAll(ctxChild, cfg)
			}
		}
	}
}

// pruneQueues 在 reload/shutdown 时清理 stale device queue：
//   - cfg==nil：全清
//   - cfg!=nil：只保留 cfg 中存在的 deviceBizID，其他删除（对应的 writer 已退出）
func (w *Workers) pruneQueues(cfg *platform.Config) {
	w.queuesMu.Lock()
	defer w.queuesMu.Unlock()
	if cfg == nil {
		w.queues = make(map[string]chan Command)
		return
	}
	keep := map[string]bool{}
	for _, dev := range cfg.Devices {
		if dev.Enabled && deviceWriteEnabled(dev) {
			keep[dev.DeviceBizID] = true
		}
	}
	for bizID := range w.queues {
		if !keep[bizID] {
			delete(w.queues, bizID)
		}
	}
}

func (w *Workers) spawnAll(ctx context.Context, cfg *platform.Config) {
	for _, dev := range cfg.Devices {
		if !dev.Enabled {
			continue
		}
		if !deviceWriteEnabled(dev) {
			continue
		}
		dev := dev
		_ = w.pools.SubmitWriteBlocking(ctx, "write-"+dev.DeviceBizID, func(ctx context.Context) {
			w.runWriter(ctx, dev)
		}, 100*time.Millisecond)
	}
}

func deviceWriteEnabled(dev *platform.Device) bool {
	for _, src := range dev.Sources {
		if src.WriteEnabled {
			return true
		}
	}
	return false
}

// runWriter 是单个 device 的写 worker 主循环。
func (w *Workers) runWriter(ctx context.Context, dev *platform.Device) {
	// 关键：queue 必须在此处取得/创建，不能因为“还没人写过”而直接 return。
	// 旧实现依赖 Enqueue 先创建 queue，但 spawnAll 在配置加载时就跑，
	// 此时 queue 尚不存在 -> writer 直接退出 -> 写命令永远无人消费（超时）。
	q := w.ensureQueue(dev.DeviceBizID)

	// 拿 device 上第一个 write-enabled 的 collect channel（与采集 worker 复用同一 Registry + 同一 socket）
	channel := lookupFirstCollectChannelCfg(w.store, dev)
	if channel == nil {
		logging.Error(logging.EventWriteConnectFail).
			Str("device", dev.DeviceBizID).Msg("writePath 无法定位 collect channel")
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-q:
			// 每次写都通过 Registry 取一次 channel 句柄，引用计数保证采集与写入共用底层 socket；
			// 写完立即 Release；如此既避免 socket 泄漏，也避免与采集 worker 形成并发竞争。
			cli, err := w.registry.Acquire(ctx, channel)
			if err != nil {
				if cmd.ResultChan != nil {
					select {
					case cmd.ResultChan <- Result{Result: "fail", Error: "channel 获取失败: " + err.Error()}:
					default:
					}
					close(cmd.ResultChan)
				}
				continue
			}
			w.execute(ctx, dev, cmd, cli)
			w.registry.Release(channel.ChannelBizID, cli)
		}
	}
}

func (w *Workers) execute(ctx context.Context, dev *platform.Device, cmd Command, plugin protocol.ProtocolChannel) {
	if cmd.TraceID == "" {
		cmd.TraceID = uuid.NewString()
	}
	timeout := time.Duration(cmd.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 500 * time.Millisecond
	}

	start := time.Now()
	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 点位元数据（地址 + string 长度）一次查出来。
	// string 长度必须传给协议层：S7 无头布局没有长度字段，
	// 不知道内容区多长就无法正确填充，PLC 侧会读到残留旧数据。
	addr, strLen := lookupPointMeta(w.store, dev.DeviceBizID, cmd.PointBizID)

	var err error
	if sw, ok := plugin.(protocol.StringLengthWriter); ok && cmd.DataType == "string" {
		err = sw.WriteStringLen(writeCtx, addr, cmd.Value, cmd.DataType, strLen)
	} else {
		err = plugin.Write(writeCtx, addr, cmd.Value, cmd.DataType)
	}

	result := Result{DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		result.Result = "fail"
		result.Error = err.Error()
		logging.Warn(logging.EventWriteFail).
			Str("device", dev.DeviceBizID).Str("point", cmd.PointBizID).
			Str("event", "write_fail").Str("trace_id", cmd.TraceID).Err(err).Msg("回写失败")
		metrics.WriteFailTotal.WithLabelValues(dev.DeviceBizID).Inc()
		metrics.WriteSuccessTotal.WithLabelValues(dev.DeviceBizID, "fail").Inc()
	} else {
		result.Result = "success"
		metrics.WriteSuccessTotal.WithLabelValues(dev.DeviceBizID, "success").Inc()
	}

	if cmd.ResultChan != nil {
		select {
		case cmd.ResultChan <- result:
		default:
		}
		close(cmd.ResultChan)
	}
}

// lookupFirstCollectChannelCfg 返回第一个 write-enabled collect channel 的配置（而非 ProtocolChannel）。
// 调用方负责调用 Registry.Acquire / Release 管理连接生命周期。
func lookupFirstCollectChannelCfg(store *config.Store, dev *platform.Device) *platform.Channel {
	cfg := store.Current()
	if cfg == nil {
		return nil
	}
	for _, src := range dev.Sources {
		if !src.WriteEnabled {
			continue
		}
		for _, ch := range cfg.Channels {
			if ch.ChannelBizID == src.ChannelBizID && ch.Role == "collect" {
				return ch
			}
		}
	}
	return nil
}

func lookupPointAddress(store *config.Store, deviceBizID, pointBizID string) string {
	addr, _ := lookupPointMeta(store, deviceBizID, pointBizID)
	return addr
}

// lookupPointMeta 查找点位元数据：返回地址与字符串长度。
//
// stringLen 来自 platform.Point.StringLength（仅 string 类型点位有意义），
// 单位是**字符数**（与读取路径的语义一致）；S7 协议层会按编码换算成字节。
func lookupPointMeta(store *config.Store, deviceBizID, pointBizID string) (addr string, stringLen int) {
	cfg := store.Current()
	if cfg == nil {
		return "", 0
	}
	for _, dev := range cfg.Devices {
		if dev.DeviceBizID != deviceBizID {
			continue
		}
		for _, src := range dev.Sources {
			for _, p := range src.Points {
				if p.PointBizID == pointBizID {
					return p.Address, p.StringLength
				}
			}
		}
	}
	return "", 0
}

// Stop ?????
func (w *Workers) Stop(timeout time.Duration) {
	w.heartbeat.Stop()
	_ = timeout
}
