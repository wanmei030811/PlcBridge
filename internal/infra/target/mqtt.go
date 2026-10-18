// MQTT Target 插件（双协议：MQTT v5 / v3.1.1）。
//
// 实现要点（Plan.md §6.3.5 / §6.4.3）：
//   - 同时支持 MQTT v5 和 MQTT v3.1.1，由 Channel.Config.protocol_version 字段选择
//     v5 → github.com/eclipse/paho.golang (autopaho 自动重连 + 指数退避 + LWT)
//     v3.1.1 → github.com/eclipse/paho.mqtt.golang (auto-reconnect + LWT)
//   - 单 Target = 单 broker 连接（clientCache 按 target 缓存）
//   - QoS 默认 1（保证至少一次到达）；客户端去重由上层（Source + trace_id）承担
//   - LWT：连接成功时注册到 broker；网络异常断开时 broker 自动推送 offline 主题
//     替代应用层翻转心跳（§6.3.3）
//   - payload：JSON，顶层拍平：{"device":"...","source":"...","timestamp":"yyyy-MM-dd HH:mm:ss.SSS +0800","<point_biz_id>":<value>,...}
//     同一个 batch 内的所有点位压成单条 JSON，key=point_biz_id
//   - topic：来自 target.Topic（一个 target = 一个 topic；不再拼接前缀 + device/point）
//   - 失败处理：不重试、不写 WAL，Send 返回 error → 上层 forwardPool 打 ERROR + Prometheus 计数
//
// 配置选择协议版本：
//
//	"protocol_version": "v5"     // 默认；mosquitto ≥2.1 / emqx 默认都支持
//	"protocol_version": "v3.1.1" // 兼容老 broker / 只开 v3.x 的 listener
//
// 为什么需要两个客户端库：Eclipse Paho 团队故意将 v3.x 和 v5 拆为两个独立仓库：
//   - github.com/eclipse/paho.golang          → v5 only
//   - github.com/eclipse/paho.mqtt.golang      → v3.1 / v3.1.1 only
// 协议版本字段在 CONNECT 报文里写死，单一 client 库无法跨版本。工业现场常见 broker 配置
// 为 accept_protocol_versions 3,4（mosquitto ≥2.1）或仅 v3.x，因此双协议能力必须支持。
package target

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pahov5 "github.com/eclipse/paho.golang/autopaho"
	pahov5paho "github.com/eclipse/paho.golang/paho"
	pahov311 "github.com/eclipse/paho.mqtt.golang"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/infra/timefmt"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// ProtocolVersion MQTT 协议版本。
type ProtocolVersion string

const (
	// ProtocolV5 MQTT v5.0（paho.golang + autopaho）。
	ProtocolV5 ProtocolVersion = "v5"
	// ProtocolV311 MQTT v3.1.1（paho.mqtt.golang）。
	ProtocolV311 ProtocolVersion = "v3.1.1"
)

// Validate 校验版本字符串。
func (p ProtocolVersion) Validate() error {
	switch p {
	case ProtocolV5, ProtocolV311:
		return nil
	case "":
		return nil // 空 = 默认 v5，由调用方在解析后填
	default:
		return fmt.Errorf("MQTT protocol_version 不支持: %q（仅 v5 / v3.1.1）", string(p))
	}
}

// mqttPlugin 是 MQTT Target 的 TargetPlugin 实现。
type mqttPlugin struct{}

func newMQTTPlugin() TargetPlugin { return &mqttPlugin{} }

func (p *mqttPlugin) Name() string    { return "mqtt" }
func (p *mqttPlugin) Version() string { return "1.1.0" }

// ConfigSchema MQTT Target JSON Schema（驱动 Web UI）。
func (p *mqttPlugin) ConfigSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"client_id":      {"type": "string", "description": "MQTT ClientID（QOS>0 必填，需唯一）"},
			"username":       {"type": "string"},
			"password":       {"type": "string"},
			"protocol_version": {"type": "string", "enum": ["v5", "v3.1.1"], "default": "v5",
				"description": "MQTT 协议版本。v5 默认；老 broker 仅支持 v3.x 时改 v3.1.1"},
			"qos":            {"type": "integer", "enum": [0, 1, 2], "default": 1},
			"retained":       {"type": "boolean", "default": false},
			"keepalive_sec":  {"type": "integer", "default": 30, "minimum": 5, "maximum": 3600},
			"clean_session":  {"type": "boolean", "default": true},
			"lwt_topic":      {"type": "string", "description": "Last Will Topic；为空则不启用 LWT"},
			"lwt_payload":    {"type": "string", "default": "offline"},
			"lwt_qos":        {"type": "integer", "enum": [0, 1, 2], "default": 1},
			"lwt_retained":   {"type": "boolean", "default": true},
			"connect_timeout_sec": {"type": "integer", "default": 10, "minimum": 1, "maximum": 60}
		}
	}`)
}

// MQTTConfig MQTT Target 私有配置。
type MQTTConfig struct {
	ProtocolVersion ProtocolVersion `json:"protocol_version"` // 默认 v5
	ClientID        string          `json:"client_id"`
	Username        string          `json:"username"`
	Password        string          `json:"password"`
	QoS             *int            `json:"qos,omitempty"` // nil = 未设置，默认 1
	Retained        bool            `json:"retained"`
	KeepaliveSec    int             `json:"keepalive_sec"`
	CleanSession    bool            `json:"clean_session"`
	LWTTopic        string          `json:"lwt_topic"`
	LWTPayload      string          `json:"lwt_payload"`
	LWTQoS          *int            `json:"lwt_qos,omitempty"` // nil = 默认 1
	LWTRetained     bool            `json:"lwt_retained"`

	ConnectTimeoutSec int `json:"connect_timeout_sec"`
}

// effectiveQoS 返回 QoS，未设置则填 1（fire-and-forget 不安全，强制至少一次）。
func (c *MQTTConfig) effectiveQoS() int {
	if c.QoS == nil {
		return 1
	}
	return *c.QoS
}

// effectiveLWTQoS 同上。
func (c *MQTTConfig) effectiveLWTQoS() int {
	if c.LWTQoS == nil {
		return 1
	}
	return *c.LWTQoS
}

func (p *mqttPlugin) Init(_ context.Context) error { return nil }
func (p *mqttPlugin) Close() error                { return nil }

func (p *mqttPlugin) CreateTarget(t *platform.Target, channel *platform.Channel) (TargetClient, error) {
	if channel.Type != "mqtt" {
		return nil, fmt.Errorf("MQTT Target 需要 mqtt Channel（got %s）", channel.Type)
	}
	var cfg MQTTConfig
	if len(channel.Config) > 0 {
		if err := json.Unmarshal(channel.Config, &cfg); err != nil {
			return nil, fmt.Errorf("MQTT 配置解析失败: %w", err)
		}
	}
	if cfg.ProtocolVersion == "" {
		cfg.ProtocolVersion = ProtocolV5 // 默认 v5
	}
	if err := cfg.ProtocolVersion.Validate(); err != nil {
		return nil, err
	}
	qos := cfg.effectiveQoS()
	if qos < 0 || qos > 2 {
		return nil, fmt.Errorf("MQTT qos 非法: %d", qos)
	}
	if cfg.KeepaliveSec == 0 {
		cfg.KeepaliveSec = 30
	}
	lwtQos := cfg.effectiveLWTQoS()
	if lwtQos < 0 || lwtQos > 2 {
		return nil, fmt.Errorf("MQTT lwt_qos 非法: %d", lwtQos)
	}
	if cfg.ConnectTimeoutSec == 0 {
		cfg.ConnectTimeoutSec = 10
	}
	return newMQTTTarget(t, channel, &cfg)
}

// === mqttTarget ===

// mqttBackend 抽象 v5 / v3.1.1 两种 client 实现的共同接口。
//
// 设计：mqttTarget 对外接口（Send / HealthCheck / Connect）不变；底层用 mqttBackend 适配。
type mqttBackend interface {
	// Connect 阻塞直到首次连接成功或 ctx 超时。
	Connect(ctx context.Context) error
	// HealthCheck 检查连接是否在线。
	HealthCheck(ctx context.Context) error
	// Publish 发送单条消息。
	Publish(ctx context.Context, topic string, payload []byte) error
	// Close 关闭底层连接；返回 nil 即视为已关闭。
	Close() error
	// Done 返回一个 channel，连接完全终止后关闭（用于 Close 等待）。
	Done() <-chan struct{}
	// IsConnected 返回底层客户端是否已建立连接（v5: cm 状态；v3: client.IsConnected）。
	// 与 mqttTarget.IsConnected 对齐；Stats 也要用。
	IsConnected() bool
	// QueueDepth 返回当前 outbound 队列深度（暂未实现，v5/v3 都返回 0 即可）。
	// ok=false 表示该 backend 不支持该指标。
	QueueDepth() (int, bool)
}

// mqttTarget 是单 Target 的 MQTT 客户端。
type mqttTarget struct {
	target  *platform.Target
	channel *platform.Channel
	cfg     *MQTTConfig

	backend mqttBackend
	cancel  context.CancelFunc
	started atomic.Bool
	mu      sync.Mutex

	// lastErr 最近一次 Send 失败的错误（forwarder 每次失败更新；成功清空）。
	lastErr   atomic.Pointer[string]
	lastErrAt atomic.Int64 // unix ns

	// PR2 详细统计（已含 PR1 基础 3 字段）
	sentCount      atomic.Uint64 // Send 成功累计 batch 数（每 batch 累 1，value 数另算）
	sentPoints     atomic.Uint64 // Send 成功累计 point 数（len(batch) 求和）
	errorCount     atomic.Uint64
	lastSendOkMs   atomic.Int64  // 最近 Send 成功
	lastErrorMs    atomic.Int64  // 最近一次失败（PR2 新增，与 lastErrAt 等价但用 ms）
	connectCount   atomic.Uint64 // 累计 Connect 成功次数
	lastConnectMs  atomic.Int64  // 最近 Connect 成功 unix ms
	healthCheckCount atomic.Uint64
	lastHealthCheckMs atomic.Int64
}

func newMQTTTarget(t *platform.Target, channel *platform.Channel, cfg *MQTTConfig) (*mqttTarget, error) {
	brokerURL, err := normalizeBrokerURL(channel.Address)
	if err != nil {
		return nil, fmt.Errorf("MQTT 地址解析失败 %q: %w", channel.Address, err)
	}

	var (
		backend mqttBackend
		cancel  context.CancelFunc
	)
	switch cfg.ProtocolVersion {
	case ProtocolV311:
		b, c, err := newV311Backend(t, brokerURL, cfg)
		if err != nil {
			return nil, err
		}
		backend, cancel = b, c
	default: // ProtocolV5（默认）
		b, c, err := newV5Backend(t, brokerURL, cfg)
		if err != nil {
			return nil, err
		}
		backend, cancel = b, c
	}

	return &mqttTarget{
		target:  t,
		channel: channel,
		cfg:     cfg,
		backend: backend,
		cancel:  cancel,
	}, nil
}

// Connect 等待首次连接建立（阻塞直到就绪或超时）。
//
// 幂等：并发调用安全——多个 caller 同时触发时只有第一个会真正连 broker，
// 后续 caller 等其结果。
func (c *mqttTarget) Connect(ctx context.Context) error {
	c.mu.Lock()
	backend := c.backend
	c.mu.Unlock()
	if backend == nil {
		return errors.New("MQTT Target 已关闭")
	}
	if err := backend.Connect(ctx); err != nil {
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	c.started.Store(true)
	c.connectCount.Add(1)
	c.lastConnectMs.Store(time.Now().UnixMilli())
	return nil
}

// HealthCheck 检查连接是否在线。
func (c *mqttTarget) HealthCheck(ctx context.Context) error {
	c.healthCheckCount.Add(1)
	c.lastHealthCheckMs.Store(time.Now().UnixMilli())
	c.mu.Lock()
	backend := c.backend
	c.mu.Unlock()
	if backend == nil {
		return errors.New("MQTT Target 已关闭")
	}
	return backend.HealthCheck(ctx)
}

// ProbeConnect 主动探测并建立 MQTT 连接。
//
// 用途：forwarder 启动时立即触发连接（不等 Send 才连），
// 让 IsConnected/Stats 在无数据时也能准确反映连接状态。
// MQTT 的 Connect() 已包含完整逻辑（首次连接 + 统计计数），直接复用。
func (c *mqttTarget) ProbeConnect(ctx context.Context) error {
	return c.Connect(ctx)
}

// Send 把一个 batch 内所有点位压成一条 JSON 发布到 target.Topic。
//
// payload 形状（顶层拍平）：
//
//	{
//	  "device":   "<device_biz_id>",
//	  "source":   "<source_biz_id>",
//	  "timestamp":"<RFC3339Nano UTC>",
//	  "<pt1>":    <value1>,
//	  "<pt2>":    <value2>,
//	  ...
//	}
//
// 设计要点：
//   - 整批一条 publish，减少 broker 负载、保持消息顺序
//   - 同名 point（不同 source 同 biz_id）后值覆盖前值（同 batch 罕见，行为可接受）
//   - 编码失败 → 整批失败返回（与"单条失败不阻塞"策略相反，因为现在整批只发一条）
func (c *mqttTarget) Send(ctx context.Context, batch []PointValue) error {
	if len(batch) == 0 {
		return nil
	}
	c.mu.Lock()
	backend := c.backend
	c.mu.Unlock()
	if backend == nil {
		c.setLastError(errors.New("MQTT Target 已关闭"))
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return errors.New("MQTT Target 已关闭")
	}
	if !c.started.Load() {
		// 未启动过 → 等首次连接
		if err := c.Connect(ctx); err != nil {
			return err
		}
	}
	topic := c.target.Topic
	if topic == "" {
		return fmt.Errorf("MQTT Target %s 未配置 topic", c.target.TargetBizID)
	}
	payload, err := encodeBatchPayload(batch)
	if err != nil {
		c.setLastError(fmt.Errorf("MQTT payload 编码失败 (target=%s): %w", c.target.TargetBizID, err))
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return fmt.Errorf("MQTT payload 编码失败 (target=%s): %w", c.target.TargetBizID, err)
	}
	// 二次检查 backend（Close 在 encodeBatchPayload 期间可能发生）
	c.mu.Lock()
	backend = c.backend
	c.mu.Unlock()
	if backend == nil {
		return errors.New("MQTT Target 已关闭")
	}
	if err := backend.Publish(ctx, topic, payload); err != nil {
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return fmt.Errorf("MQTT publish %s 失败: %w", topic, err)
	}
	// 成功：清 lastErr + 计数 + 时间戳
	c.setLastError(nil)
	c.sentCount.Add(1)              // 按 batch 计
	c.sentPoints.Add(uint64(len(batch))) // 按 point 计（PR2 区分）
	c.lastSendOkMs.Store(time.Now().UnixMilli())
	return nil
}

// setLastError 原子地记录最近一次错误（nil = 清空）。
func (c *mqttTarget) setLastError(err error) {
	if err == nil {
		c.lastErr.Store(nil)
		c.lastErrAt.Store(0)
		return
	}
	msg := err.Error()
	c.lastErr.Store(&msg)
	c.lastErrAt.Store(time.Now().UnixNano())
}

// IsConnected 返回本地视图：backend 非 nil + backend.HealthCheck 不报错视为 connected。
// 优先用 backend 的真实状态（v5/v311 内部 IsConnected）。
func (c *mqttTarget) IsConnected() bool {
	c.mu.Lock()
	backend := c.backend
	c.mu.Unlock()
	if backend == nil {
		return false
	}
	return backend.IsConnected()
}

// LastError 返回最近一次 Send 失败的错误信息；空字符串表示无最近错误。
func (c *mqttTarget) LastError() string {
	if p := c.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

// Stats 返回 MQTT 协议特定运行时数据。
//
// 字段（PR2 扩展）：
//   - "connection_state": "Connected" / "Disconnected" / "Reconnecting"
//   - "protocol_version": "v5" / "v3.1.1"
//   - "broker_url" / "topic": 目标 broker + topic
//   - "queue_depth": 暂未实现（paho 无公开 API）
//   - "sent_count": 累计成功 batch 数；"sent_points": 累计成功 point 数
//   - "error_count" / "last_error_ms" / "last_send_ok_ms": 失败/成功时间
//   - "connect_count" / "last_connect_ms": Connect 次数 + 最近成功时间
//   - "health_check_count" / "last_health_check_ms": HealthCheck 计数
func (c *mqttTarget) Stats() map[string]any {
	c.mu.Lock()
	backend := c.backend
	protocol := string(c.cfg.ProtocolVersion)
	if protocol == "" {
		protocol = "v5"
	}
	topic := c.target.Topic
	brokerURL := c.channel.Address
	c.mu.Unlock()

	state := "Disconnected"
	queueDepth := 0
	if backend != nil {
		if backend.IsConnected() {
			state = "Connected"
		}
		if q, ok := backend.QueueDepth(); ok {
			queueDepth = q
		}
	}
	return map[string]any{
		"connection_state":    state,
		"protocol":            "mqtt",
		"protocol_version":    protocol,
		"broker_url":          brokerURL,
		"topic":               topic,
		"queue_depth":         queueDepth,
		"sent_count":          c.sentCount.Load(),
		"sent_points":         c.sentPoints.Load(),
		"error_count":         c.errorCount.Load(),
		"last_send_ok_ms":     c.lastSendOkMs.Load(),
		"last_error_ms":       c.lastErrorMs.Load(),
		"connect_count":       c.connectCount.Load(),
		"last_connect_ms":     c.lastConnectMs.Load(),
		"health_check_count":  c.healthCheckCount.Load(),
		"last_health_check_ms": c.lastHealthCheckMs.Load(),
	}
}

// Close 关闭 MQTT 连接。
//
// 幂等：并发多次调用安全；返回 nil 表示已完全关闭。
// 实现：
//   - 调 cancel() 通知 backend 退出（v5 路径会触发 autopaho 终止）
//   - 等 backend.Done() 超时兜底；超时即视为已关
//   - backend.Close() 真正释放底层资源（v3 调 Disconnect；v5 已有 cancel 触发）
//   - 最后 backend = nil 防后续调用 Use-after-close
func (c *mqttTarget) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backend == nil {
		return nil // 幂等：已关过
	}
	c.cancel()
	// 等 backend 完成终止（最多 3 秒后硬退）
	if d := c.backend.Done(); d != nil {
		select {
		case <-d:
		case <-time.After(3 * time.Second):
			logging.Warn(logging.EventTargetCloseTimeout).
				Str("target", c.target.TargetBizID).
				Msg("MQTT Target 关闭超时，强制退出")
		}
	}
	_ = c.backend.Close()
	c.backend = nil
	return nil
}

// === v5 backend（autopaho + paho.golang）===

// v5Backend 包装 autopaho ConnectionManager，对外实现 mqttBackend。
type v5Backend struct {
	cm     *pahov5.ConnectionManager
	cfg    *MQTTConfig
	target *platform.Target
	broker *url.URL
	closed atomic.Bool
}

func newV5Backend(t *platform.Target, brokerURL *url.URL, cfg *MQTTConfig) (*v5Backend, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(context.Background())

	clientCfg := pahov5paho.ClientConfig{
		ClientID: cfg.ClientID,
		OnClientError: func(err error) {
			logging.Warn(logging.EventTargetConnectFail).
				Str("target", t.TargetBizID).
				Str("protocol", "v5").
				Err(err).
				Msg("MQTT 客户端错误")
		},
		OnServerDisconnect: func(d *pahov5paho.Disconnect) {
			if d.Properties != nil && d.Properties.ReasonString != "" {
				logging.Warn(logging.EventTargetDisconnect).
					Str("target", t.TargetBizID).
					Str("protocol", "v5").
					Str("reason", d.Properties.ReasonString).
					Msg("MQTT server disconnect")
			}
		},
	}

	cliCfg := pahov5.ClientConfig{
		ServerUrls:                    []*url.URL{brokerURL},
		KeepAlive:                     uint16(cfg.KeepaliveSec),
		CleanStartOnInitialConnection: cfg.CleanSession,
		ConnectTimeout:                time.Duration(cfg.ConnectTimeoutSec) * time.Second,
		ReconnectBackoff:              defaultBackoff,
		ClientConfig:                  clientCfg,
		OnConnectionUp: func(_ *pahov5.ConnectionManager, _ *pahov5paho.Connack) {
			logging.Info(logging.EventTargetConnectOK).
				Str("target", t.TargetBizID).
				Str("protocol", "v5").
				Str("broker", brokerURL.String()).
				Msg("MQTT Target 已连接")
		},
		OnConnectError: func(err error) {
			logging.Warn(logging.EventTargetConnectFail).
				Str("target", t.TargetBizID).
				Str("protocol", "v5").
				Err(err).
				Msg("MQTT Target 连接出错")
		},
		OnConnectionDown: func() bool {
			logging.Warn(logging.EventTargetDisconnect).
				Str("target", t.TargetBizID).
				Str("protocol", "v5").
				Msg("MQTT Target 断开，准备重连")
			return true // 继续重连
		},
	}

	if cfg.Username != "" {
		cliCfg.ConnectUsername = cfg.Username
		cliCfg.ConnectPassword = []byte(cfg.Password)
	}

	if cfg.LWTTopic != "" {
		cliCfg.WillMessage = &pahov5paho.WillMessage{
			Topic:   cfg.LWTTopic,
			Payload: []byte(cfg.LWTPayload),
			QoS:     byte(cfg.effectiveLWTQoS()),
			Retain:  cfg.LWTRetained,
		}
	}

	cm, err := pahov5.NewConnection(ctx, cliCfg)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("MQTT NewConnection 失败: %w", err)
	}

	return &v5Backend{
		cm:     cm,
		cfg:    cfg,
		target: t,
		broker: brokerURL,
	}, cancel, nil
}

func (b *v5Backend) Connect(ctx context.Context) error {
	if b.closed.Load() {
		return errors.New("MQTT Target 已关闭")
	}
	connectCtx, cancel := context.WithTimeout(ctx, time.Duration(b.cfg.ConnectTimeoutSec)*time.Second)
	defer cancel()
	if err := b.cm.AwaitConnection(connectCtx); err != nil {
		return fmt.Errorf("MQTT 首次连接失败: %w", err)
	}
	return nil
}

// IsConnected 返回 autopaho ConnectionManager 当前是否已建立连接。
//
// 实现说明：autopaho 没有公开的 IsConnected()，但内部通过 ctx 协调；这里用
// AwaitConnection(短 ctx) 作为轻量探活（成功=已连接，失败=未连接）。
// 注意：这个调用比较重（1ms~100ms），Stats 路径只在 1Hz 轮询下调用，可接受。
func (b *v5Backend) IsConnected() bool {
	if b.closed.Load() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	return b.cm.AwaitConnection(ctx) == nil
}

// QueueDepth v5 backend 不暴露 queue 深度（autopaho 内部管 publish queue，无公开 API）。
func (b *v5Backend) QueueDepth() (int, bool) { return 0, false }

func (b *v5Backend) HealthCheck(ctx context.Context) error {
	if b.closed.Load() {
		return errors.New("MQTT Target 已停止")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := b.cm.AwaitConnection(probeCtx); err != nil {
		// ctx 超时但连接仍存在 → 当作 OK（重连进行中）
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil
		}
		return fmt.Errorf("MQTT 探活失败: %w", err)
	}
	return nil
}

func (b *v5Backend) Publish(ctx context.Context, topic string, payload []byte) error {
	if b.closed.Load() {
		return errors.New("MQTT Target 已关闭")
	}
	pub := &pahov5paho.Publish{
		QoS:     byte(b.cfg.effectiveQoS()),
		Topic:   topic,
		Payload: payload,
		Retain:  b.cfg.Retained,
	}
	if _, err := b.cm.Publish(ctx, pub); err != nil {
		return err
	}
	return nil
}

// Close 关闭 v5 backend（幂等）。
//
// 实现：autopaho 的 Disconnect(ctx) = cancelCtx() + 等 c.done 关闭。完成后 Done() 已 close。
func (b *v5Backend) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.cm.Disconnect(ctx)
	}
	return nil
}

// Done 转发 autopaho 的 cm.Done()，由 mqttTarget.Close() 用于等待后台 goroutine 退出。
func (b *v5Backend) Done() <-chan struct{} {
	return b.cm.Done()
}

// === v3.1.1 backend（paho.mqtt.golang）===

// v311Backend 包装 paho.mqtt.golang Client。
//
// 关键设计点（相对 v5 的差异）：
//   - v3 client 库内部 goroutine 与我们 host 程序的 goroutine 不直观对齐
//   - Connect 只能调用一次，重复调用会 panic
//   - Publish token.Wait() 不会被 ctx 取消，且在 Disconnect 后可能永久阻塞
// 因此本 backend 引入：
//   - connectedOnce 标志，保证 Connect 幂等
//   - closed 标志，Close 与 Publish 之间的竞态防护
//   - Publish 包装 goroutine + ctx 控制，永不阻塞上层 forwardPool
type v311Backend struct {
	client     pahov311.Client
	cfg        *MQTTConfig
	target     *platform.Target
	broker     *url.URL
	opts       *pahov311.ClientOptions
	connectErr error // 首次 Connect 错误缓存，给后续 Connect 调用返回
	connected  atomic.Bool
	closed     atomic.Bool
	doneCh     chan struct{}
	doneOnce   sync.Once

	// connectMu 保护 client.Connect() 的二次调用限制
	connectMu sync.Mutex
}

func newV311Backend(t *platform.Target, brokerURL *url.URL, cfg *MQTTConfig) (*v311Backend, context.CancelFunc, error) {
	// v311 backend 不需要外部 ctx，因为 paho.mqtt.golang 内部管 goroutine。
	// 关闭通过 Disconnect + doneCh 协调。
	cancel := func() {} // no-op；保留接口一致性

	opts := pahov311.NewClientOptions().
		AddBroker(brokerURL.String()).
		SetClientID(cfg.ClientID).
		SetUsername(cfg.Username).
		SetPassword(cfg.Password).
		SetCleanSession(cfg.CleanSession).
		SetKeepAlive(time.Duration(cfg.KeepaliveSec) * time.Second).
		SetConnectTimeout(time.Duration(cfg.ConnectTimeoutSec) * time.Second).
		SetConnectRetry(true).
		SetConnectRetryInterval(2 * time.Second).
		SetMaxReconnectInterval(5 * time.Minute).
		SetAutoReconnect(true).
		SetOrderMatters(false). // 避免 onMessage handler 阻塞导致死锁；我们是 publish-only，无订阅
		SetProtocolVersion(4).  // 4 = MQTT 3.1.1；显式设值后不会再回退到 3.1
		SetResumeSubs(false)    // publish-only；不需要 resume subs

	if cfg.LWTTopic != "" {
		opts.SetWill(cfg.LWTTopic, cfg.LWTPayload, byte(cfg.effectiveLWTQoS()), cfg.LWTRetained)
	}

	opts.OnConnect = func(_ pahov311.Client) {
		logging.Info(logging.EventTargetConnectOK).
			Str("target", t.TargetBizID).
			Str("protocol", "v3.1.1").
			Str("broker", brokerURL.String()).
			Msg("MQTT Target 已连接")
	}
	opts.OnConnectionLost = func(_ pahov311.Client, err error) {
		logging.Warn(logging.EventTargetDisconnect).
			Str("target", t.TargetBizID).
			Str("protocol", "v3.1.1").
			Err(err).
			Msg("MQTT Target 断开")
	}

	client := pahov311.NewClient(opts)

	return &v311Backend{
		client: client,
		cfg:    cfg,
		target: t,
		broker: brokerURL,
		opts:   opts,
		doneCh: make(chan struct{}),
	}, cancel, nil
}

// Connect 等待首次连接建立，幂等。
//
// 实现：
//   - 只调一次 paho 的 client.Connect()；二次调用返回缓存结果
//   - 用 ctx 控制总等待时间
//   - 失败时缓存错误，下次直接返回（避免反复 token.Wait）
func (b *v311Backend) Connect(ctx context.Context) error {
	if b.closed.Load() {
		return errors.New("MQTT Target 已关闭")
	}
	if b.connected.Load() {
		return nil
	}
	b.connectMu.Lock()
	defer b.connectMu.Unlock()
	// double-check
	if b.connected.Load() {
		return nil
	}
	if b.connectErr != nil {
		return fmt.Errorf("MQTT 首次连接失败: %w", b.connectErr)
	}

	type result struct{ err error }
	resCh := make(chan result, 1)
	go func() {
		tok := b.client.Connect()
		tok.Wait()
		resCh <- result{tok.Error()}
	}()

	timeout := time.Duration(b.cfg.ConnectTimeoutSec) * time.Second
	select {
	case r := <-resCh:
		if r.err != nil {
			b.connectErr = r.err
			return fmt.Errorf("MQTT 首次连接失败: %w", r.err)
		}
		b.connected.Store(true)
		return nil
	case <-ctx.Done():
		// ctx 取消时，调用 Disconnect 让 paho 的 goroutine 快速结束
		b.client.Disconnect(0)
		b.connectErr = ctx.Err()
		return fmt.Errorf("MQTT 首次连接失败: %w", ctx.Err())
	case <-time.After(timeout):
		b.client.Disconnect(0)
		b.connectErr = fmt.Errorf("timeout %v", timeout)
		return fmt.Errorf("MQTT 首次连接失败: %w", b.connectErr)
	}
}

func (b *v311Backend) HealthCheck(ctx context.Context) error {
	if b.closed.Load() {
		return errors.New("MQTT Target 已停止")
	}
	if b.client.IsConnected() {
		return nil
	}
	// 短暂确认：仍然未连接 → 视为重连中（paho 库在 autoReconnect=true 下会自动重连）。
	// 行为对齐 v5: HealthCheck 在重连中不报错，让上层 forwardPool 自行 retry Send。
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil
		}
		return ctx.Err()
	case <-time.After(100 * time.Millisecond):
	}
	return nil
}

// IsConnected 返回 paho.mqtt.golang 客户端是否已建立连接。
func (b *v311Backend) IsConnected() bool {
	if b.closed.Load() {
		return false
	}
	return b.connected.Load() && b.client.IsConnected()
}

// QueueDepth v3.1.1 backend 不暴露 queue 深度（paho 内部管，无公开 API）。
func (b *v311Backend) QueueDepth() (int, bool) { return 0, false }

// Publish 发送单条消息。
//
// 使用 Token.Done() 直接 select，不会泄漏 goroutine。
//   - paho 的 Token.Done channel 在以下情况 close：流完成 / client 关闭 / 错误
//   - 因此 ctx 取消或 backend 关闭都能立即返回
//   - 即使 broker 长时间不响应，client 关闭后 token 也会被 finish
func (b *v311Backend) Publish(ctx context.Context, topic string, payload []byte) error {
	if b.closed.Load() {
		return errors.New("MQTT Target 已关闭")
	}
	if b.client == nil {
		return errors.New("MQTT Target 未初始化")
	}
	tok := b.client.Publish(topic, byte(b.cfg.effectiveQoS()), b.cfg.Retained, payload)
	if tok == nil {
		return errors.New("MQTT Publish 返回 nil token")
	}
	select {
	case <-tok.Done():
		// token 已结束，再查 ctx 和 err
		if err := tok.Error(); err != nil {
			return fmt.Errorf("MQTT publish %s: %w", topic, err)
		}
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("MQTT publish %s: %w", topic, cerr)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("MQTT publish %s: %w", topic, ctx.Err())
	}
}

func (b *v311Backend) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil // 已关闭，幂等返回
	}
	// Disconnect(quiesce=2000)：库内部等 in-flight 排空后断开
	b.client.Disconnect(2000)
	b.doneOnce.Do(func() { close(b.doneCh) })
	return nil
}

func (b *v311Backend) Done() <-chan struct{} {
	return b.doneCh
}

// === 工具 ===

// normalizeBrokerURL 把 channel.Address 归一化为可被两个 client 库接受的字符串/URL。
//
// 支持格式：
//   tcp://192.168.1.10:1883        → mqtt://192.168.1.10:1883
//   mqtt://host:1883                → mqtt://host:1883
//   tls://host:8883                 → tls://host:8883
//   ssl://host:8883                 → tls://host:8883
//   192.168.1.10:1883               → mqtt://192.168.1.10:1883
//   ws://broker:8083/mqtt           → ws://broker:8083/mqtt
//
// 返回的 *url.URL 既给 autopaho（v5），又给 paho.mqtt.golang（v3.1.1，
// 通过 .String() 取字符串形式 AddBroker）。两个库都接受 mqtt:// / tls:// / ws:// / wss://。
func normalizeBrokerURL(addr string) (*url.URL, error) {
	a := strings.TrimSpace(addr)
	if a == "" {
		return nil, errors.New("MQTT broker 地址为空")
	}
	if !strings.Contains(a, "://") {
		a = "mqtt://" + a
	}
	if strings.HasPrefix(a, "tcp://") {
		a = "mqtt://" + strings.TrimPrefix(a, "tcp://")
	}
	if strings.HasPrefix(a, "ssl://") {
		a = "tls://" + strings.TrimPrefix(a, "ssl://")
	}
	u, err := url.Parse(a)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "mqtt" && u.Scheme != "tls" && u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("MQTT scheme 不支持: %s", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("MQTT broker 缺少 host: %s", a)
	}
	return u, nil
}

// encodeBatchPayload 把一个 batch 的 PointValue 压成单条 JSON（顶层拍平）。
//
// 输出形如：
//
//	{"device":"d1","source":"s1","timestamp":"2026-...","M100":0,"V200":42.5}
//
// device/source/timestamp 取自 batch[0]（forwardPool 同一 batch 来自同一次采集，
// device 与 source 一致；保留第一个的值即代表整批的来源）。
func encodeBatchPayload(batch []PointValue) ([]byte, error) {
	if len(batch) == 0 {
		return nil, errors.New("空 batch")
	}
	first := batch[0]
	// 用 map[string]interface{} 以允许点名为任意字符串；其余元信息固定字段。
	out := make(map[string]interface{}, 4+len(batch))
	out["device"] = first.DeviceBizID
	out["source"] = first.SourceBizID
	out["timestamp"] = timefmt.FormatTime(first.Timestamp)
	for _, pv := range batch {
		out[pv.PointBizID] = pv.Value
	}
	return json.Marshal(out)
}

// defaultBackoff 指数退避（10s 起，最大 5min），用于 v5 重连。
func defaultBackoff(attempt int) time.Duration {
	const maxBackoff = 5 * time.Minute
	const base = 10 * time.Second
	d := base * time.Duration(1<<uint(min(attempt, 6)))
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
