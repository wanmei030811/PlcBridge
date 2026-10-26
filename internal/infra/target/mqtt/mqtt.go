// MQTT Target 插件（双协议：MQTT v5 / v3.1.1）。
//
// 同时支持 MQTT v5 和 MQTT v3.1.1：
//   - v5 → github.com/eclipse/paho.golang (autopaho 自动重连 + 指数退避 + LWT)
//   - v3.1.1 → github.com/eclipse/paho.mqtt.golang (auto-reconnect + LWT)
//   - 单 Target = 单 broker 连接
//   - QoS 默认 1（保证至少一次到达）
//   - LWT：连接成功时注册到 broker；网络异常断开时 broker 自动推送 offline 主题
//   - payload：JSON，顶层拍平：{"device":"...","source":"...","timestamp":"...","<point_biz_id>":<value>,...}
//   - topic：来自 target.Topic（一个 target = 一个 topic）
//   - 失败处理：不重试、不写 WAL，Send 返回 error → 上层 forwardPool 打 ERROR + Prometheus 计数
package mqtt

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
	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/infra/timefmt"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// ProtocolVersion MQTT 协议版本。
type ProtocolVersion string

const (
	ProtocolV5   ProtocolVersion = "v5"
	ProtocolV311 ProtocolVersion = "v3.1.1"
)

func (p ProtocolVersion) Validate() error {
	switch p {
	case ProtocolV5, ProtocolV311:
		return nil
	case "":
		return nil
	default:
		return fmt.Errorf("MQTT protocol_version 不支持: %q（仅 v5 / v3.1.1）", string(p))
	}
}

// === 插件注册 ===

type mqttPlugin struct{}

func newPlugin() target.TargetPlugin { return &mqttPlugin{} }

func (p *mqttPlugin) Name() string    { return "mqtt" }
func (p *mqttPlugin) Version() string { return "1.1.0" }

// Capabilities MQTT Target：不支持历史查询。
//
// MQTT 是纯消息投递通道，没有时序存储，客户端未实现 target.HistoryQueryer，
// 前端据此不渲染「历史趋势」按钮。
func (p *mqttPlugin) Capabilities() target.Capability {
	return target.Capability{
		SupportsHistory: false,
	}
}

func (p *mqttPlugin) ConfigSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"client_id":      {"type": "string", "description": "MQTT ClientID（QOS>0 必填，需唯一）"},
			"username":       {"type": "string"},
			"password":       {"type": "string"},
			"protocol_version": {"type": "string", "enum": ["v5", "v3.1.1"], "default": "v5"},
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

type MQTTConfig struct {
	ProtocolVersion   ProtocolVersion `json:"protocol_version"`
	ClientID          string          `json:"client_id"`
	Username          string          `json:"username"`
	Password          string          `json:"password"`
	QoS               *int            `json:"qos,omitempty"`
	Retained          bool            `json:"retained"`
	KeepaliveSec      int             `json:"keepalive_sec"`
	CleanSession      bool            `json:"clean_session"`
	LWTTopic          string          `json:"lwt_topic"`
	LWTPayload        string          `json:"lwt_payload"`
	LWTQoS            *int            `json:"lwt_qos,omitempty"`
	LWTRetained       bool            `json:"lwt_retained"`
	ConnectTimeoutSec int             `json:"connect_timeout_sec"`
}

func (c *MQTTConfig) effectiveQoS() int {
	if c.QoS == nil {
		return 1
	}
	return *c.QoS
}

func (c *MQTTConfig) effectiveLWTQoS() int {
	if c.LWTQoS == nil {
		return 1
	}
	return *c.LWTQoS
}

func (p *mqttPlugin) Init(_ context.Context) error { return nil }
func (p *mqttPlugin) Close() error                 { return nil }

func (p *mqttPlugin) CreateTarget(t *platform.Target, channel *platform.Channel) (target.TargetClient, error) {
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
		cfg.ProtocolVersion = ProtocolV5
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

type mqttBackend interface {
	Connect(ctx context.Context) error
	HealthCheck(ctx context.Context) error
	Publish(ctx context.Context, topic string, payload []byte) error
	Close() error
	Done() <-chan struct{}
	IsConnected() bool
	QueueDepth() (int, bool)
}

type mqttTarget struct {
	target  *platform.Target
	channel *platform.Channel
	cfg     *MQTTConfig

	backend mqttBackend
	cancel  context.CancelFunc
	started atomic.Bool
	mu      sync.Mutex

	lastErr   atomic.Pointer[string]
	lastErrAt atomic.Int64

	sentCount         atomic.Uint64
	sentPoints        atomic.Uint64
	errorCount        atomic.Uint64
	lastSendOkMs      atomic.Int64
	lastErrorMs       atomic.Int64
	connectCount      atomic.Uint64
	lastConnectMs     atomic.Int64
	healthCheckCount  atomic.Uint64
	lastHealthCheckMs atomic.Int64
}

func newMQTTTarget(t *platform.Target, channel *platform.Channel, cfg *MQTTConfig) (*mqttTarget, error) {
	brokerURL, err := normalizeBrokerURL(channel.Address)
	if err != nil {
		return nil, fmt.Errorf("MQTT 地址解析失败 %q: %w", channel.Address, err)
	}

	var backend mqttBackend
	var cancel context.CancelFunc
	switch cfg.ProtocolVersion {
	case ProtocolV311:
		b, c, err := newV311Backend(t, brokerURL, cfg)
		if err != nil {
			return nil, err
		}
		backend, cancel = b, c
	default:
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

func (c *mqttTarget) HealthCheck(ctx context.Context) error {
	c.healthCheckCount.Add(1)
	c.lastHealthCheckMs.Store(time.Now().UnixMilli())
	c.mu.Lock()
	backend := c.backend
	c.mu.Unlock()
	if backend == nil {
		return errors.New("MQTT Target 已关闭")
	}
	// 与 InfluxDB 一致：探活结果要落到 lastErr 上，
	// 否则 UI 只能看到"不在线"，看不到原因（broker 地址错误 / 认证失败 等）。
	//
	// 同时：探活成功要**清空** lastErr，否则一次失败会让状态永远停在"异常"
	// —— 这正是本次修复的第二个问题（InfluxDB 停掉很久后状态无法自动恢复）。
	if err := backend.HealthCheck(ctx); err != nil {
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	c.setLastError(nil)
	return nil
}

func (c *mqttTarget) ProbeConnect(ctx context.Context) error {
	return c.Connect(ctx)
}

func (c *mqttTarget) Send(ctx context.Context, batch []target.PointValue) error {
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
	c.setLastError(nil)
	c.sentCount.Add(1)
	c.sentPoints.Add(uint64(len(batch)))
	c.lastSendOkMs.Store(time.Now().UnixMilli())
	return nil
}

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

func (c *mqttTarget) IsConnected() bool {
	c.mu.Lock()
	backend := c.backend
	c.mu.Unlock()
	if backend == nil {
		return false
	}
	return backend.IsConnected()
}

func (c *mqttTarget) LastError() string {
	if p := c.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

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
		"connection_state":     state,
		"protocol":             "mqtt",
		"protocol_version":     protocol,
		"broker_url":           brokerURL,
		"topic":                topic,
		"queue_depth":          queueDepth,
		"sent_count":           c.sentCount.Load(),
		"sent_points":          c.sentPoints.Load(),
		"error_count":          c.errorCount.Load(),
		"last_send_ok_ms":      c.lastSendOkMs.Load(),
		"last_error_ms":        c.lastErrorMs.Load(),
		"connect_count":        c.connectCount.Load(),
		"last_connect_ms":      c.lastConnectMs.Load(),
		"health_check_count":   c.healthCheckCount.Load(),
		"last_health_check_ms": c.lastHealthCheckMs.Load(),
	}
}

func (c *mqttTarget) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backend == nil {
		return nil
	}
	c.cancel()
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

// === v5 backend ===

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
			return true
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

	return &v5Backend{cm: cm, cfg: cfg, target: t, broker: brokerURL}, cancel, nil
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

func (b *v5Backend) IsConnected() bool {
	if b.closed.Load() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	return b.cm.AwaitConnection(ctx) == nil
}

func (b *v5Backend) QueueDepth() (int, bool) { return 0, false }

func (b *v5Backend) HealthCheck(ctx context.Context) error {
	if b.closed.Load() {
		return errors.New("MQTT Target 已停止")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := b.cm.AwaitConnection(probeCtx); err != nil {
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

func (b *v5Backend) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.cm.Disconnect(ctx)
	}
	return nil
}

func (b *v5Backend) Done() <-chan struct{} {
	return b.cm.Done()
}

// === v3.1.1 backend ===

type v311Backend struct {
	client     pahov311.Client
	cfg        *MQTTConfig
	target     *platform.Target
	broker     *url.URL
	opts       *pahov311.ClientOptions
	connectErr error
	connected  atomic.Bool
	closed     atomic.Bool
	doneCh     chan struct{}
	doneOnce   sync.Once
	connectMu  sync.Mutex
}

func newV311Backend(t *platform.Target, brokerURL *url.URL, cfg *MQTTConfig) (*v311Backend, context.CancelFunc, error) {
	cancel := func() {}

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
		SetOrderMatters(false).
		SetProtocolVersion(4).
		SetResumeSubs(false)

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

func (b *v311Backend) Connect(ctx context.Context) error {
	if b.closed.Load() {
		return errors.New("MQTT Target 已关闭")
	}
	if b.connected.Load() {
		return nil
	}
	b.connectMu.Lock()
	defer b.connectMu.Unlock()
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

func (b *v311Backend) IsConnected() bool {
	if b.closed.Load() {
		return false
	}
	return b.connected.Load() && b.client.IsConnected()
}

func (b *v311Backend) QueueDepth() (int, bool) { return 0, false }

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
		return nil
	}
	b.client.Disconnect(2000)
	b.doneOnce.Do(func() { close(b.doneCh) })
	return nil
}

func (b *v311Backend) Done() <-chan struct{} {
	return b.doneCh
}

// === 工具 ===

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

func encodeBatchPayload(batch []target.PointValue) ([]byte, error) {
	if len(batch) == 0 {
		return nil, errors.New("空 batch")
	}
	first := batch[0]
	out := make(map[string]interface{}, 4+len(batch))
	out["device"] = first.DeviceBizID
	out["source"] = first.SourceBizID
	out["timestamp"] = timefmt.FormatTime(first.Timestamp)
	for _, pv := range batch {
		out[pv.PointBizID] = pv.Value
	}
	return json.Marshal(out)
}

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

// init MQTT Target 自注册（_all.go 触发）。
func init() {
	if err := target.Register(newPlugin()); err != nil {
		panic(err)
	}
}
