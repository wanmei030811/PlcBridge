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
	"sync"
	"sync/atomic"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/logging"
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
//（MQTT conn、InfluxDB HTTP conn pool、autopaho background goroutine）。
type TargetCloser interface {
	Close() error
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

func MustRegisterDefaults() {
	MustRegister(newMQTTPlugin())
	MustRegister(newInfluxDBPlugin())
}

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
}

// NewQueue 创建容量为 size 的有界队列。
func NewQueue(size int) *Queue {
	if size <= 0 {
		size = 1000
	}
	return &Queue{ch: make(chan PointValue, size)}
}

// Close 关闭队列。已关闭后 Enqueue 直接 drop；Dequeue/Raw 立即收到 closed 信号。
// 关闭后 router 不再持有该 queue，避免 reload 后旧 buffer 永久累积。
func (q *Queue) Close() {
	if !q.closed.CompareAndSwap(false, true) {
		return
	}
	close(q.ch)
}

// Clear 非阻塞排空队列中的所有数据（不清除统计计数器）。用于停用 device
// 时清理 router 中缓存的历史数据，避免 forward worker 启动后一次性冲刷。
func (q *Queue) Clear() int {
	n := 0
	for {
		select {
		case <-q.ch:
			n++
		default:
			q.dropped.Add(uint64(n))
			return n
		}
	}
}

// Enqueue 非阻塞入队，满了删除队头。queue 已关闭时直接 drop。
func (q *Queue) Enqueue(v PointValue) {
	if q.closed.Load() {
		q.dropped.Add(1)
		return
	}
	for {
		select {
		case q.ch <- v:
			q.enqueued.Add(1)
			return
		default:
			// 关闭后 channel 上 send 会 panic，需重新检查
			if q.closed.Load() {
				q.dropped.Add(1)
				return
			}
			select {
			case <-q.ch:
				q.dropped.Add(1)
				logging.Warn(logging.EventTargetQueueFull).
					Str("event", "target_queue_full").
					Msg("Target 队列满，丢弃队头")
			default:
			}
		}
	}
}

// Dequeue 阻塞出队，ctx 取消或 queue 关闭时退出。
func (q *Queue) Dequeue(ctx context.Context) (PointValue, bool) {
	select {
	case v, ok := <-q.ch:
		return v, ok
	case <-ctx.Done():
		return PointValue{}, false
	}
}

// Raw 暴露原始 channel（用于非阻塞 select）。
func (q *Queue) Raw() <-chan PointValue { return q.ch }

// Closed 返回 queue 是否已关闭。
func (q *Queue) Closed() bool { return q.closed.Load() }

// Stats 返回队列统计。
func (q *Queue) Stats() (length, dropped, enqueued int) {
	return len(q.ch), int(q.dropped.Load()), int(q.enqueued.Load())
}
