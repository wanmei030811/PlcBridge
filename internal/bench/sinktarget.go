// Package bench —— sinktarget：一个 in-process 的 TargetPlugin，
// 用于压测时替代真实 MQTT/InfluxDB。
//
// 注册名："sink"，配置字段：
//   - 任意 JSON 对象都会被忽略
//
// Send 行为：把 batch 计入 atomic 计数，端到端不打日志/不打网络，往 ctx.Done 即返回。
package bench

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// SinkPlugin 实现了 target.TargetPlugin，所有 target 共享一个 *SinkStats。
type SinkPlugin struct {
	stats *SinkStats
}

// SinkStats 是 sink target 的全局计数（端到端压力指标）。
type SinkStats struct {
	Sent       atomic.Uint64 // 收到的 total PointValue 数
	Batches    atomic.Uint64 // Send 调用次数
	LastSentAt atomic.Int64  // UnixNano of last batch
	MaxLatency atomic.Int64  // max Send latency in ms
}

// NewSinkPlugin 创建一个 sink plugin，共享一份 stats。
func NewSinkPlugin(stats *SinkStats) *SinkPlugin {
	if stats == nil {
		stats = &SinkStats{}
	}
	return &SinkPlugin{stats: stats}
}

func (p *SinkPlugin) Name() string    { return "sink" }
func (p *SinkPlugin) Version() string { return "bench-1.0" }
func (p *SinkPlugin) ConfigSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}

// Capabilities sink Target：不支持历史查询（仅内存计数，无时序存储）。
func (p *SinkPlugin) Capabilities() target.Capability {
	return target.Capability{
		SupportsHistory: false,
	}
}

func (p *SinkPlugin) Init(_ context.Context) error { return nil }
func (p *SinkPlugin) Close() error                 { return nil }

func (p *SinkPlugin) CreateTarget(_ *platform.Target, _ *platform.Channel) (target.TargetClient, error) {
	return &sinkClient{stats: p.stats}, nil
}

type sinkClient struct {
	stats *SinkStats
}

func (c *sinkClient) Send(ctx context.Context, batch []target.PointValue) error {
	start := time.Now()
	n := len(batch)
	c.stats.Batches.Add(1)
	c.stats.Sent.Add(uint64(n))
	c.stats.LastSentAt.Store(time.Now().UnixNano())
	// 检查 ctx 的代价不应太低（避免编译器优化掉），但也不能真 sleep 拖慢基准。
	// 直接返回。
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	latencyMs := time.Since(start).Milliseconds()
	if latencyMs > 0 {
		// 多数情况 latency < 1ms，跳过最大更新避免噪声
	}
	for {
		prev := c.stats.MaxLatency.Load()
		if latencyMs <= prev {
			break
		}
		if c.stats.MaxLatency.CompareAndSwap(prev, latencyMs) {
			break
		}
	}
	logging.Info(logging.EventForwardSendOK).Msg("sink target sent batch") // 控制台输出节流靠 log lever
	return nil
}

func (c *sinkClient) HealthCheck(_ context.Context) error { return nil }

// ProbeConnect sink 是 in-process target，探测始终成功。
func (c *sinkClient) ProbeConnect(_ context.Context) error { return nil }

// IsConnected sink 是 in-process target，永远 connected。
func (c *sinkClient) IsConnected() bool { return true }

// LastError sink 不报错。
func (c *sinkClient) LastError() string { return "" }

// Stats 返回 sink target 运行时数据。
func (c *sinkClient) Stats() map[string]any {
	return map[string]any{
		"connection_state": "Connected",
		"protocol":         "sink",
		"sent_count":       c.stats.Sent.Load(),
		"batch_count":      c.stats.Batches.Load(),
		"max_latency_ms":   c.stats.MaxLatency.Load(),
		"last_sent_at_ms":  c.stats.LastSentAt.Load() / int64(time.Millisecond),
	}
}

// Close 实现 target.TargetCloser（pointer receiver，bench 中调用 nil-safe）
func (c *sinkClient) Close() error { return nil }
