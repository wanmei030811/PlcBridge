// Package target 定义转发目标插件接口。
//
// MVP 内置 2 个 Target（均为完整实现，非 stub）：
//   - mqtt      同时支持 MQTT v5（paho.golang + autopaho）和 v3.1.1（paho.mqtt.golang）
//   - influxdb  支持 V1.x、V1.8+ Flux、V2.x、V3.x（V3 走 V2 兼容 endpoint）
//
// 数据流：collectPool → 按 Target.point_biz_ids 过滤（自动推导 Source 范围）→ 入 Target 队列 → forwardPool 投递。
//
// Target 失败处理（§6.4.3）：不重试、不写 WAL，直接丢弃 + ERROR 日志 + Prometheus 计数。
package target

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/metrics"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// PointValue 是要转发的点位值（带 trace_id）。
type PointValue struct {
	TraceID     string
	DeviceBizID string
	SourceBizID string
	PointBizID  string
	Timestamp   time.Time
	Value       any
	DataType    string
}

// TargetPlugin 是转发目标的扩展点（Plan.md §9.2）。
type TargetPlugin interface {
	Name() string
	Version() string
	ConfigSchema() json.RawMessage

	// Capabilities 声明本 Target 的运行时能力（Plan2 主题 A 3.6，forward 侧对称扩展）。
	//
	// 用途：
	//   - Web 端：按 SupportsHistory 决定是否渲染「历史趋势」按钮
	//     （不再在前端硬编码 channel type 白名单，避免前后端两份事实源）
	//   - 文档生成：自动产出「Target 能力矩阵」
	//
	// 实现要求：返回零值 Capability 即可（不实现任何高级特性）。
	Capabilities() Capability

	Init(ctx context.Context) error
	Close() error

	CreateTarget(t *platform.Target, channel *platform.Channel) (TargetClient, error)
}

// TargetClient 是单个 Target 的客户端实例。
type TargetClient interface {
	Send(ctx context.Context, batch []PointValue) error
	HealthCheck(ctx context.Context) error

	// === Runtime 状态（供 web 仪表盘 / 通道列表展示） ===
	// 与 ProtocolChannel 对齐，让 collect + forward 用同一套 ChannelSnapshot 呈现。

	// IsConnected 返回本地视图的连接状态（是否曾 connect 过 / 还没被 Close）。
	// **不能**用于判断远端真活；调用方应节流调 HealthCheck 确认。
	IsConnected() bool

	// LastError 返回最近一次失败的错误信息；空字符串表示无最近错误。
	LastError() string

	// Stats 返回 Target 特定运行时数据（用于 web 仪表盘诊断）。
	// 推荐字段：
	//   - "connection_state": "Connected" / "Disconnected" / "Reconnecting"
	//   - "<protocol>_write_count" / "<protocol>_error_count"
	//   - "last_send_ok_ms": 最近一次 Send 成功的 unix ms
	//   - 协议特定：如 mqtt 的 "broker_url"/"topic"/"queue_depth"，influxdb 的 "version"/"org"
	// 实现要求同 ProtocolChannel.Stats：并发安全 + json-marshalable + panic 安全。
	Stats() map[string]any

	// ProbeConnect 主动探测并建立到远端服务器的连接（可选实现）。
	//
	// 用途：forwarder 启动时立即触发连接（不等 Send 才连），
	// 让 IsConnected/Stats 在无数据时也能准确反映连接状态。
	// 若 Target 连接由 Send 隐式建立（如 InfluxDB ensureClient），可空实现。
	// 不返回错误时视为探测成功（连接已建立）。
	ProbeConnect(ctx context.Context) error
}

// TargetCloser 是可选接口：实现 Close 的 TargetClient 可被 forward.Workers 释放底层资源。
//
// 这是可选接口，不实现也不会编译失败（forward.Workers 用 type assertion）。
// 推荐所有 TargetClient 实现 Close 以避免 reload 时的资源泄漏
// （MQTT conn、InfluxDB HTTP conn pool、autopaho background goroutine）。
type TargetCloser interface {
	Close() error
}

// HistoryQueryer 是可选接口：支持历史数据查询的 TargetClient（Plan3 3.5）。
//
// 目前只有 InfluxDB 实现（用于前端历史趋势图）。
// 实现要求：
//   - 自行限制查询时间范围（默认最大 7 天）与返回点数（默认最大 10000）
//   - 不接受任意 SQL 注入风险的高危操作（Flux 是查询语言，风险较低，
//     但实现方仍应校验 measurement 白名单）
//
// 设计说明（2026-10-21 修正）：
//
//	早期签名是 QueryHistory(ctx, query string)，query 由 api 层用
//	buildInfluxFluxQuery 拼好的 Flux 文本。这把「V2 专用的 Flux 方言」硬编码
//	进了接口，导致 V1 客户端（version=1.x，走 net/http 自实现）无法实现该接口——
//	V1 没有 Flux 引擎，收到 Flux 串根本没法执行。
//	现在改为传结构化参数 HistoryQuery，由 Target 自行翻译成本版本查询语句，
//	接口对 V1 / V2 通用。
type HistoryQueryer interface {
	// QueryHistory 执行一次历史查询，返回行数组（每行是 map[string]any）。
	// 通用字段约定：
	//   - "_time"   : 时间戳（time.Time 或 int64 毫秒）
	//   - "_value"  : 点位值
	//   - 其他 key  : tag / field 名
	QueryHistory(ctx context.Context, q HistoryQuery) (any, error)
}

// HistoryQuery 是历史查询的结构化参数（与具体存储实现无关）。
//
// 由 api 层从 HTTP query string 解析并做安全收敛后传入，
// Target 客户端负责翻译成自己版本的查询语句（Flux / InfluxQL）。
type HistoryQuery struct {
	// Measurement 是 measurement 名（InfluxDB 的 _measurement）。
	// 写入侧用 Target.Topic 作为 measurement 名。
	Measurement string

	// Field 是字段名（InfluxDB 的 _field）。
	// 写入侧用 Point.PointBizID 作为 field key。
	// 空串表示不限字段。
	Field string

	// Duration 是时间窗口，形如 "1h" / "15m" / "7d"。
	// 已由 api 层收敛到 1m ~ 7d。
	Duration string

	// Limit 是返回点数上限。已由 api 层收敛到 1 ~ 10000。
	Limit int
}

// Capability 描述 Target 的运行时能力（Plan2 主题 A 3.6，forward 侧对称扩展）。
//
// 与 protocol.Capability 对称，但字段集合是 forward 侧特有的。
// 命名约定：
//   - Supports*  → 运行时能力（该 Target 能做什么）
type Capability struct {
	// SupportsHistory 是否支持历史数据查询（实现 target.HistoryQueryer 接口）。
	//
	// 目前只有 InfluxDB 的 V1（InfluxQL）与 V2/V1.8+/V3（Flux）客户端实现了该接口。
	// 前端据此决定是否渲染「历史趋势」按钮——避免前端硬编码 channel type 白名单。
	SupportsHistory bool `json:"supports_history"`
}

// === 全局注册表 ===
var (
	registry   = map[string]TargetPlugin{}
	registryMu sync.RWMutex
)

func Register(p TargetPlugin) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[p.Name()]; exists {
		return fmt.Errorf("Target %s 已注册", p.Name())
	}
	registry[p.Name()] = p
	return nil
}

func MustRegister(p TargetPlugin) {
	if err := Register(p); err != nil {
		panic(err)
	}
}

func Get(name string) (TargetPlugin, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("Target %s 未注册", name)
	}
	return p, nil
}

// Unregister 注销 Target 插件（仅供测试用）。
//
// 与 protocol.Unregister 对称。
func Unregister(name string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, name)
}

// TargetInfo 是单个 Target 插件对外暴露的元信息（Plan2 主题 A 3.3）。
//
// 与 protocol.ProtocolInfo 对称：供 GET /api/v1/protocols 接口统一返回 collect + forward 两类。
type TargetInfo struct {
	Name         string          `json:"name"`
	Version      string          `json:"version"`
	Role         string          `json:"role"` // 固定 "forward"
	ConfigSchema json.RawMessage `json:"config_schema"`
	// Capabilities 声明该 Target 的运行时能力（与 protocol.ProtocolInfo.Capabilities 对称）。
	//
	// 前端按 SupportsHistory 决定是否渲染「历史趋势」按钮。
	Capabilities Capability `json:"capabilities"`
}

// AllInfo 返回所有已注册 Target 插件的元信息（按 name 字典序）。
//
// 线程安全：读持有 registryMu.RLock；plugin 内部方法（Name/Version/ConfigSchema/Capabilities）
// 必须是无状态读。
func AllInfo() []TargetInfo {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]TargetInfo, 0, len(registry))
	for _, p := range registry {
		out = append(out, TargetInfo{
			Name:         p.Name(),
			Version:      p.Version(),
			Role:         "forward",
			ConfigSchema: p.ConfigSchema(),
			Capabilities: p.Capabilities(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// 注意：MVP 内置 Target 的注册已从 MustRegisterDefaults 改为各 plugin 文件末尾的 init() 自注册（Plan2 主题 A 3.2）。
// 加新 Target：新建 plugin 文件 → 实现 TargetPlugin 接口 → 文件末尾加 init() 调 Register 即可。

// Queue 是 Target 的有界队列（§6.1）。
//
// 队列满时**删除队头**（drop-oldest），保证不阻塞采集主路径。
//
// 选择 drop-oldest 而非 drop-newest 的理由（§6.4.3）：
//   - 工业场景下"最新数据"比"旧数据"更有业务价值
//   - 下一周期有新数据补上
type Queue struct {
	ch       chan PointValue
	dropped  atomic.Uint64
	enqueued atomic.Uint64
	closed   atomic.Bool

	// closeMu 保护 ch 的 close 动作与 send 动作的互斥（Plan2 7.2 竞态修复）。
	//
	// 为什么需要它（2026-10-26 修）：
	//   "q.closed.Load() == false" 与 "q.ch <- v" 之间存在 toctou 窗口，
	//   期间另一个 goroutine 调 Close → close(q.ch) 与 send 并发 → data race
	//   （且在旧实现里会 panic: send on closed channel）。
	//
	// 为什么用 RWMutex 而不是只靠 recover 兜底：
	//   recover 只能防住 panic，消不掉 race —— `-race` 下 CI 依然判失败。
	//
	// 为什么是 RWMutex 而不是普通 Mutex：
	//   Enqueue 是采集热路径（每点位每周期一次），Close 是低频事件（配置重载）。
	//   读锁允许多个 Enqueue 并发进行，互斥只发生在真正 close 的瞬间。
	//
	// 锁序：closeMu 不与其他锁嵌套，Close 内部不调用任何会回调进来的方法。
	closeMu sync.RWMutex

	// targetBizID 用于 Prometheus 指标打标签（plcbridge_target_dropped_total）。
	//
	// 为什么存 ID 而不是只靠调用方传：
	// 队列满丢数据发生在 Enqueue 内部（drop-oldest 策略），
	// 那一刻只有 Queue 自己知道丢了多少 —— 如果不上报，
	// "下游消费慢导致持续丢数据"这个最重要的生产故障就完全不可见。
	targetBizID string
}

// NewQueue 创建容量为 size 的有界队列。
//
// targetBizID 用于 Prometheus 指标标签；传空串则不上报丢弃计数
// （测试场景可以传空）。
func NewQueue(size int, targetBizID ...string) *Queue {
	if size <= 0 {
		size = 1000
	}
	id := ""
	if len(targetBizID) > 0 {
		id = targetBizID[0]
	}
	return &Queue{ch: make(chan PointValue, size), targetBizID: id}
}

// reportDropped 上报丢弃计数到 Prometheus。
func (q *Queue) reportDropped(reason string, n uint64) {
	if q.targetBizID == "" || n == 0 {
		return
	}
	metrics.TargetDroppedTotal.WithLabelValues(q.targetBizID, reason).Add(float64(n))
}

// Close 关闭队列。已关闭后 Enqueue 直接 drop；Dequeue/Raw 立即收到 closed 信号。
// 关闭后 router 不再持有该 queue，避免 reload 后旧 buffer 永久累积。
//
// 2026-10-26 修（消除 close/send data race）：
//
//	旧实现：CompareAndSwap 与 close(q.ch) 都没有锁保护，
//	与 Enqueue 的 "q.ch <- v" 并发 → data race（旧代码还有 panic: send on closed channel）。
//
//	现在：整个"标记关闭 + 关闭 channel"都在 closeMu 写锁内完成。
//	与 Enqueue 的读锁互斥，于是形成严格的 happens-before：
//	  - Enqueue 持读锁期间看到 closed==false → Close 不可能同时持写锁 → channel 一定未关 → send 安全
//	  - Close 完成后再进来的 Enqueue → 看到 closed==true → 走 drop 分支
//
//	CAS 放在锁内而不是锁外，是为了避免"标记已关闭"与"真正关闭 channel"之间出现窗口：
//	若 CAS 在锁外，Enqueue 可能在 CAS 之后、close 之前拿到读锁并发送 —— 虽然此时
//	channel 还没关不会 panic，但会往一个"已标记关闭"的队列写入数据（语义错误）。
//
//	死锁检查：Close 只持写锁并调用 close()（不回调任何用户代码）；
//	Enqueue 只持读锁且内部不请求写锁 → 不存在循环等待。
func (q *Queue) Close() {
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	if !q.closed.CompareAndSwap(false, true) {
		return
	}
	close(q.ch)
}

// Clear 非阻塞排空队列中的所有数据（不清除统计计数器）。用于停用 device
// 时清理 router 中缓存的历史数据，避免 forward worker 启动后一次性冲刷。
//
// 2026-10-26 说明：这里**不**持 closeMu 读锁。
//
//	依据：Go 内存模型中 "close(ch)" 与 "从 ch 接收" 安全同步（race detector 不报）。
//	Clear 只做接收，与 Enqueue 的发送之间也不冲突（channel 本身支持并发收发）。
//	需要互斥的只有"发送"与"关闭"，那已由 closeMu 覆盖。
func (q *Queue) Clear() int {
	n := 0
	for {
		select {
		case <-q.ch:
			n++
		default:
			q.dropped.Add(uint64(n))
			q.reportDropped("cleared", uint64(n))
			return n
		}
	}
}

// Enqueue 非阻塞入队，满了删除队头。queue 已关闭时直接 drop。
//
// 关闭竞态（Plan2 7.2 bugfix，2026-10-26 修正）：
//
//	Enqueue 与 Close 不在同一个锁里，"closed.Load() == false" 与 "q.ch <- v" 之间存在
//	toctou 窗口：另一个 goroutine 正好在此期间调 Close → 关闭后的 channel send 会 panic，
//	且 close/send 并发本身就是 data race（-race 下 CI 判失败，recover 消不掉）。
//
//	现在的修法：send 全程持有 closeMu 读锁，Close 的 close() 持写锁 → 两者互斥，
//	既不会 panic，也没有 race。recover 保留作为兜底（防止将来有人绕过锁直接 send）。
//
//	性能：读锁是热路径上的轻量操作（与 PLC 周期 1ms 相比可忽略）；
//	且多 producer 之间不互斥（RWMutex 读锁可并发）。
func (q *Queue) Enqueue(v PointValue) {
	// 快路径：已关闭则直接 drop，不进锁（reload 后队列已废弃时 Enqueue 仍会被调用，
	// 不走锁能避免无谓的锁竞争）。
	if q.closed.Load() {
		q.dropped.Add(1)
		q.reportDropped("closed", 1)
		return
	}

	// closeMu 读锁：与 Close 的写锁互斥，保证 send 期间 channel 不会被 close。
	q.closeMu.RLock()
	defer q.closeMu.RUnlock()

	// 锁内复查（权威检查）：
	//   Close 的 "CAS closed=true + close(ch)" 整体在写锁内完成。
	//   本 goroutine 若在 Close 之前拿到读锁，则 Close 还在等写锁 → channel 未关 → send 安全；
	//   若在 Close 之后拿到读锁，则必然看到 closed==true → 走 drop 分支。
	//   这次复查与 Close 的 CAS 之间存在 happens-before 关系，因此是可靠的。
	if q.closed.Load() {
		q.dropped.Add(1)
		q.reportDropped("closed", 1)
		return
	}

	for {
		// trySend 把 send 包到闭包里，作为兜底防止任何漏网的 "send on closed channel" panic
		// 返回 (ok, panicked)：ok 表示 send 真的完成；panicked 表示与 Close 竞态 → drop
		trySend := func() (ok, panicked bool) {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
				}
			}()
			select {
			case q.ch <- v:
				return true, false
			default:
				return false, false
			}
		}
		ok, panicked := trySend()
		if panicked {
			q.dropped.Add(1)
			q.reportDropped("closed_race", 1)
			logging.Warn("target_queue_send_on_closed").
				Str("event", "target_queue_race").
				Msg("Enqueue 与 Close 竞态：send-on-closed-channel 已被 recover，丢弃该值")
			return
		}
		if ok {
			q.enqueued.Add(1)
			return
		}
		// select default：队列满 → 丢弃队头 + 重试
		if q.closed.Load() {
			q.dropped.Add(1)
			q.reportDropped("closed", 1)
			return
		}
		select {
		case <-q.ch:
			q.dropped.Add(1)
			q.reportDropped("queue_full", 1)
			logging.Warn(logging.EventTargetQueueFull).
				Str("event", "target_queue_full").
				Msg("Target 队列满，丢弃队头")
		default:
		}
	}
}

// Dequeue 阻塞出队，ctx 取消或 queue 关闭时退出。
//
// 2026-10-26 说明：这里**不**持 closeMu 读锁。
//
//	依据：Go 内存模型中 "close(ch)" 与 "从 ch 接收" 是正确同步的一对，
//	race detector 不会把二者报告为竞争（已用最小复现验证）。
//	若在此处持读锁反而会死锁：Dequeue 阻塞在接收上不放锁，
//	而 Close 需要写锁 → 队列永远不会被关闭 → Dequeue 永远不返回。
func (q *Queue) Dequeue(ctx context.Context) (PointValue, bool) {
	select {
	case v, ok := <-q.ch:
		return v, ok
	case <-ctx.Done():
		return PointValue{}, false
	}
}

// Raw 暴露原始 channel（用于非阻塞 select）。
//
// 2026-10-26 说明：返回裸 channel 不引入 race —— 调用方从中接收，
// 与 Close 的 close() 之间是 Go 内存模型认可的安全组合。
// 真正需要互斥的是"发送"与"关闭"，已由 closeMu 保证（见 Enqueue）。
func (q *Queue) Raw() <-chan PointValue { return q.ch }

// Closed 返回 queue 是否已关闭。
func (q *Queue) Closed() bool { return q.closed.Load() }

// Stats 返回队列统计。
func (q *Queue) Stats() (length, dropped, enqueued int) {
	return len(q.ch), int(q.dropped.Load()), int(q.enqueued.Load())
}

// Cap 返回队列容量。
func (q *Queue) Cap() int { return cap(q.ch) }
