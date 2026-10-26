// OPC UA 协议插件（基于 github.com/gopcua/opcua v0.9，TCP 二进制协议）。
//
// 实现要点：
//   - 单 Channel = 一个 gopcua.Client 会话（endpoint + 安全策略 + 鉴权一组 = 一个 channel）
//   - 所有点共享一个 client session，节点用真实 NodeID（"ns=2;s=Channel1.Device1.Tag" 等）
//   - BatchRead：构造 ReadRequest 多节点，一次请求拿回所有点
//   - Write / BatchWrite：WriteRequest 多节点写
//   - 订阅（Plan.md §6.1 占位 → 现已实现）：Subscribe + MonitoredItems（DataChange）
//   - 订阅以 channel 维度建立，按 (NodeID) 复用 clientHandle；可选 PublishInterval
//   - 通过回调 OnChange(nodeID, value) 暴露，外部消费者（如 collect.Workers 转发）
//   - 安全策略：MVP 只支持 None + Anonymous（证书/UserName 保留配置字段，启用待后续）
package opcua

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
	"github.com/plcbridge/plcbridge/internal/infra/logging"
	"github.com/plcbridge/plcbridge/internal/infra/protocol"
	"github.com/plcbridge/plcbridge/internal/platform"
)

// opcuaPlugin 是 OPC UA 协议的 ProtocolPlugin 实现。
type opcuaPlugin struct{}

func newPlugin() protocol.ProtocolPlugin { return &opcuaPlugin{} }

func (p *opcuaPlugin) Name() string    { return "opcua" }
func (p *opcuaPlugin) Version() string { return "1.0.0" }

// Capabilities OPC UA：支持批量读 / 协议层心跳（Session KeepAlive）。
func (p *opcuaPlugin) Capabilities() protocol.Capability {
	return protocol.Capability{
		SupportsBatchRead: true,
		SupportsHeartbeat: true,
	}
}

// ConfigSchema OPC UA 协议 JSON Schema（驱动 Web UI）。
func (p *opcuaPlugin) ConfigSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"endpoint":              {"type": "string", "description": "opc.tcp://host:port"},
			"security_mode":         {"type": "string", "enum": ["None", "Sign", "SignAndEncrypt"], "default": "None"},
			"security_policy":       {"type": "string", "enum": ["None", "Basic128Rsa15", "Basic256", "Basic256Sha256"], "default": "None"},
			"auth_type":            {"type": "string", "enum": ["Anonymous", "UserName", "Certificate"], "default": "Anonymous"},
			"username":             {"type": "string"},
			"password":             {"type": "string"},
			"session_timeout_ms":   {"type": "integer", "default": 600000},
			"publish_interval_ms":  {"type": "integer", "default": 1000, "minimum": 100, "description": "订阅发布间隔（仅订阅模式下有效）"},
			"auto_select_endpoint": {"type": "boolean", "default": true, "description": "GetEndpoints 后按 security 自动选端点"}
		},
		"required": ["endpoint"]
	}`)
}

// OPCUAConfig OPC UA 通道私有配置。
type OPCUAConfig struct {
	Endpoint           string `json:"endpoint"`
	SecurityMode       string `json:"security_mode"`
	SecurityPolicy     string `json:"security_policy"`
	AuthType           string `json:"auth_type"`
	Username           string `json:"username"`
	Password           string `json:"password"`
	SessionTimeoutMs   int    `json:"session_timeout_ms"`
	PublishIntervalMs  int    `json:"publish_interval_ms"`
	AutoSelectEndpoint bool   `json:"auto_select_endpoint"`
}

func (p *opcuaPlugin) ParseConfig(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, errors.New("OPC UA 配置不能为空（需 endpoint）")
	}
	var cfg OPCUAConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("OPC UA 配置解析失败: %w", err)
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("OPC UA endpoint 必填")
	}
	// MVP 限制：仅 None + Anonymous 通过校验。
	if !strings.EqualFold(cfg.SecurityMode, "None") || !strings.EqualFold(cfg.SecurityPolicy, "None") {
		return nil, fmt.Errorf("OPC UA MVP 仅支持 security_mode=None + security_policy=None")
	}
	if !strings.EqualFold(cfg.AuthType, "Anonymous") {
		return nil, fmt.Errorf("OPC UA MVP 仅支持 auth_type=Anonymous")
	}
	if cfg.SessionTimeoutMs == 0 {
		cfg.SessionTimeoutMs = 600000
	}
	if cfg.PublishIntervalMs == 0 {
		cfg.PublishIntervalMs = 1000
	}
	return &cfg, nil
}

func (p *opcuaPlugin) Init(_ context.Context) error { return nil }
func (p *opcuaPlugin) Close() error                 { return nil }

func (p *opcuaPlugin) CreateChannel(ch *platform.Channel) (protocol.ProtocolChannel, error) {
	parsed, err := p.ParseConfig(ch.Config)
	if err != nil {
		return nil, err
	}
	cfg, ok := parsed.(*OPCUAConfig)
	if !ok {
		return nil, fmt.Errorf("OPC UA CreateChannel: ParseConfig 返回类型 %T", parsed)
	}
	return newOPCUAChannel(ch.ChannelBizID, cfg)
}

// === opcuaChannel ===

// ValueChangeFunc 是订阅回调签名：nodeID → 新值（已转 Value）。
type ValueChangeFunc func(address string, v protocol.Value)

// opcuaChannel 是单 endpoint + 单会话的 OPC UA 通道。
type opcuaChannel struct {
	channelID string
	cfg       *OPCUAConfig

	client   *opcua.Client
	sub      *opcua.Subscription
	notifyCh chan *opcua.PublishNotificationData
	cancel   context.CancelFunc

	mu        sync.Mutex
	closed    atomic.Bool
	connected atomic.Bool
	lastUsed  atomic.Int64
	lastErr   atomic.Pointer[string]
	lastErrAt atomic.Int64

	// writeMu 串行化所有对 cli 的写操作（Write + heartbeat flip），避免与订阅派发并发
	// 造成 OPC UA 客户端内部状态错乱（gopcua 客户端非线程安全）
	writeMu sync.Mutex

	// 订阅映射
	subMu       sync.Mutex
	handleSeq   uint32
	handleToA   map[uint32]string // handle → address
	handleToNID map[uint32]*ua.NodeID
	subEnabled  bool
	onChange    ValueChangeFunc

	// PR2：详细统计
	readCount     atomic.Uint64
	writeCount    atomic.Uint64
	errorCount    atomic.Uint64
	lastReadOkMs  atomic.Int64
	lastWriteOkMs atomic.Int64
	lastErrorMs   atomic.Int64
}

func newOPCUAChannel(channelID string, cfg *OPCUAConfig) (*opcuaChannel, error) {
	if cfg == nil {
		return nil, errors.New("OPC UA 配置为空")
	}
	return &opcuaChannel{
		channelID:   channelID,
		cfg:         cfg,
		handleToA:   map[uint32]string{},
		handleToNID: map[uint32]*ua.NodeID{},
	}, nil
}

func (c *opcuaChannel) Connect(ctx context.Context) error {
	if c.closed.Load() {
		return fmt.Errorf("OPC UA channel %s 已关闭", c.channelID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return nil // 幂等
	}
	return c.connectLocked(ctx)
}

// Reconnect 强制重建 OPC UA 连接。
func (c *opcuaChannel) Reconnect(ctx context.Context) error {
	if c.closed.Load() {
		return fmt.Errorf("OPC UA channel %s 已关闭", c.channelID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		c.disableSubscription()
		_ = c.client.Close(context.Background())
		c.client = nil
		c.connected.Store(false)
	}
	return c.connectLocked(ctx)
}

// connectLocked 在 c.mu 持有时执行实际连接建立。
func (c *opcuaChannel) connectLocked(ctx context.Context) error {
	opts := []opcua.Option{
		opcua.SecurityMode(ua.MessageSecurityModeNone),
		opcua.SecurityPolicy(ua.SecurityPolicyURINone),
		opcua.AuthAnonymous(),
		opcua.SessionTimeout(time.Duration(c.cfg.SessionTimeoutMs) * time.Millisecond),
		opcua.DialTimeout(10 * time.Second),
	}

	// 可选：自动 SelectEndpoint
	if c.cfg.AutoSelectEndpoint {
		eps, err := opcua.GetEndpoints(ctx, c.cfg.Endpoint)
		if err != nil {
			return fmt.Errorf("OPC UA GetEndpoints 失败: %w", err)
		}
		ep, err := opcua.SelectEndpoint(eps, c.cfg.SecurityPolicy, ua.MessageSecurityModeNone)
		if err != nil {
			return fmt.Errorf("OPC UA SelectEndpoint 失败: %w", err)
		}
		ep.EndpointURL = c.cfg.Endpoint
		opts = append(opts, opcua.SecurityFromEndpoint(ep, ua.UserTokenTypeAnonymous))
	}

	cli, err := opcua.NewClient(c.cfg.Endpoint, opts...)
	if err != nil {
		c.setErr(err)
		return fmt.Errorf("OPC UA NewClient 失败: %w", err)
	}
	if err := cli.Connect(ctx); err != nil {
		c.setErr(err)
		return fmt.Errorf("OPC UA Connect 失败: %w", err)
	}

	c.client = cli
	c.connected.Store(true)
	c.touch()
	c.setErr(nil)
	logging.Info(logging.EventChannelEstablished).
		Str("channel", c.channelID).
		Str("endpoint", c.cfg.Endpoint).
		Msg("OPC UA channel 已建立")
	return nil
}

func (c *opcuaChannel) Disconnect() error {
	if c.closed.Load() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return nil
	}

	// 先停订阅
	c.disableSubscription()

	// 关闭 client
	if err := c.client.Close(context.Background()); err != nil {
		logging.Warn(logging.EventChannelClosed).
			Str("channel", c.channelID).
			Err(err).
			Msg("OPC UA Close 出错")
	}
	c.client = nil
	c.connected.Store(false)
	logging.Info(logging.EventChannelClosed).
		Str("channel", c.channelID).
		Msg("OPC UA channel 已断开")
	return nil
}

func (c *opcuaChannel) HealthCheck(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil || !c.connected.Load() {
		return fmt.Errorf("OPC UA channel %s 未连接", c.channelID)
	}
	c.touch()
	return nil
}

// EnableSubscription 启用订阅模式（一次性通道）。onChange 收到数据变更时回调。
func (c *opcuaChannel) EnableSubscription(onChange ValueChangeFunc) error {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if c.subEnabled {
		c.onChange = onChange
		return nil
	}
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil {
		return errors.New("OPC UA channel 未连接，无法启用订阅")
	}

	interval := time.Duration(c.cfg.PublishIntervalMs) * time.Millisecond
	notifyCh := make(chan *opcua.PublishNotificationData, 64)
	sub, err := cli.Subscribe(context.Background(), &opcua.SubscriptionParameters{
		Interval: interval,
	}, notifyCh)
	if err != nil {
		return fmt.Errorf("OPC UA Subscribe 失败: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.notifyCh = notifyCh
	c.sub = sub
	c.cancel = cancel
	c.onChange = onChange
	c.subEnabled = true

	// 把 channel 副本传给派发 goroutine —— 它只读自己的局部副本，
	// 不再读取共享字段 c.notifyCh（避免与 disableSubscription 的置 nil 竞争）。
	go c.dispatchNotifications(ctx, notifyCh)
	logging.Info(logging.EventChannelEstablished).
		Str("channel", c.channelID).
		Dur("interval", interval).
		Msg("OPC UA 订阅已启用")
	return nil
}

func (c *opcuaChannel) disableSubscription() {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if !c.subEnabled {
		return
	}
	if c.cancel != nil {
		c.cancel()
	}
	if c.sub != nil {
		_ = c.sub.Cancel(context.Background())
	}
	// 2026-10-26 修（去掉 close(c.notifyCh)）：
	//   gopcua 的 publish 循环会在它自己的 goroutine 里向 notifyCh 发送，
	//   Subscription.Cancel 不保证此刻没有发送正在进行 →
	//   close(notifyCh) 存在 "send on closed channel" 的 panic 风险。
	//
	//   正确做法：不关闭 channel。上面的 cancel() 已经让消费侧
	//   dispatchNotifications 通过 ctx.Done() 退出；channel 无生产者后自然回收。
	//
	//   旧实现里 close(c.notifyCh) 与 dispatchNotifications 读 c.notifyCh 的
	//   并发，还构成了 -race 报告的 data race。
	c.sub = nil
	c.notifyCh = nil
	c.cancel = nil
	c.subEnabled = false
	c.handleToA = map[uint32]string{}
	c.handleToNID = map[uint32]*ua.NodeID{}
	logging.Info(logging.EventChannelClosed).
		Str("channel", c.channelID).
		Msg("OPC UA 订阅已停用")
}

// AddMonitoredItem 在当前订阅中添加一个节点（按 NodeID）。
func (c *opcuaChannel) AddMonitoredItem(ctx context.Context, address string) error {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if !c.subEnabled || c.sub == nil {
		return errors.New("OPC UA 订阅未启用")
	}

	nid, err := parseOPCUANodeID(address)
	if err != nil {
		return err
	}

	// 去重
	for _, existing := range c.handleToNID {
		if existing.Namespace() == nid.Namespace() &&
			existing.IntID() == nid.IntID() &&
			existing.StringID() == nid.StringID() {
			return nil
		}
	}

	c.handleSeq++
	handle := c.handleSeq
	req := opcua.NewMonitoredItemCreateRequestWithDefaults(nid, ua.AttributeIDValue, handle)

	res, err := c.sub.Monitor(ctx, ua.TimestampsToReturnBoth, req)
	if err != nil {
		return fmt.Errorf("OPC UA Monitor 失败: %w", err)
	}
	if len(res.Results) == 0 || res.Results[0].StatusCode != ua.StatusOK {
		st := ua.StatusCode(0)
		if len(res.Results) > 0 {
			st = res.Results[0].StatusCode
		}
		return fmt.Errorf("OPC UA Monitor 状态非 OK: %v", st)
	}

	c.handleToA[handle] = address
	c.handleToNID[handle] = nid
	return nil
}

func (c *opcuaChannel) dispatchNotifications(ctx context.Context, notifyCh chan *opcua.PublishNotificationData) {
	// 2026-10-26 修（data race）：
	//   原来这里读的是共享字段 c.notifyCh，而 disableSubscription 会在 subMu 锁内
	//   把它置 nil → "读 c.notifyCh" 与 "写 c.notifyCh = nil" 构成 data race
	//   （-race 下 CI 判失败）。
	//
	//   修法：把 channel 作为**参数**传进来 —— 每个订阅周期一份不可变副本，
	//   goroutine 全程只读自己的局部变量，不碰共享字段 → race 消除。
	//
	//   生命周期：ctx 取消（本函数唯一的退出路径）由 disableSubscription 触发，
	//   退出后 goroutine 返回，channel 随之被 GC。
	//
	//   为什么不再 close(notifyCh)：
	//   gopcua 的 Subscription.publish 循环会在**它自己的 goroutine**里向
	//   notifyCh 发送（见 client_sub.go: notifySubscription → sub.notify → s.Notifs <- data），
	//   而 Subscription.Cancel 并不保证此刻没有发送正在进行 →
	//   close(notifyCh) 存在 "send on closed channel" 的 panic 风险。
	//   正确做法是**不关闭** channel，靠 ctx 取消让消费侧退出；
	//   channel 无生产者时自然被回收。
	for {
		select {
		case <-ctx.Done():
			return
		case res, ok := <-notifyCh:
			if !ok {
				return
			}
			if res == nil || res.Error != nil {
				continue
			}
			dcn, ok := res.Value.(*ua.DataChangeNotification)
			if !ok {
				continue
			}
			c.subMu.Lock()
			cb := c.onChange
			c.subMu.Unlock()
			if cb == nil {
				continue
			}
			for _, item := range dcn.MonitoredItems {
				c.subMu.Lock()
				addr := c.handleToA[item.ClientHandle]
				c.subMu.Unlock()
				if addr == "" {
					continue
				}
				quality := "good"
				if item.Value != nil && item.Value.Status != ua.StatusOK {
					quality = "bad"
				}
				v := valueFromDataChange(item.Value)
				v.Quality = quality
				cb(addr, v)
			}
		}
	}
}

// === 读 / 写 ===

func (c *opcuaChannel) Read(ctx context.Context, address string, dataType string) (protocol.Value, error) {
	nid, err := parseOPCUANodeID(address)
	if err != nil {
		return protocol.Value{}, err
	}
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil {
		return protocol.Value{}, errors.New("OPC UA 未连接")
	}

	req := &ua.ReadRequest{
		MaxAge: 2000,
		NodesToRead: []*ua.ReadValueID{
			{NodeID: nid, AttributeID: ua.AttributeIDValue},
		},
		TimestampsToReturn: ua.TimestampsToReturnBoth,
	}
	resp, err := cli.Read(ctx, req)
	if err != nil {
		c.setErr(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return protocol.Value{}, fmt.Errorf("OPC UA Read 失败: %w", err)
	}
	c.setErr(nil)
	if resp == nil || len(resp.Results) == 0 {
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return protocol.Value{}, errors.New("OPC UA Read 无结果")
	}
	r0 := resp.Results[0]
	if r0.Status != ua.StatusOK {
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return protocol.Value{DataType: dataType, Quality: "bad"}, fmt.Errorf("OPC UA Read 状态非 OK: %v", r0.Status)
	}
	v := valueFromVariant(r0.Value, dataType)
	v.Quality = "good"
	c.readCount.Add(1)
	c.lastReadOkMs.Store(time.Now().UnixMilli())
	return v, nil
}

func (c *opcuaChannel) BatchRead(ctx context.Context, points []*platform.Point) (map[string]protocol.Value, error) {
	result := make(map[string]protocol.Value, len(points))
	if len(points) == 0 {
		return result, nil
	}

	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil {
		return nil, errors.New("OPC UA 未连接")
	}

	nodes := make([]*ua.ReadValueID, 0, len(points))
	idMap := make([]string, 0, len(points))
	for _, p := range points {
		if !p.CollectEnabled {
			continue
		}
		nid, err := parseOPCUANodeID(p.Address)
		if err != nil {
			return nil, fmt.Errorf("点位 %s 地址解析失败: %w", p.PointBizID, err)
		}
		nodes = append(nodes, &ua.ReadValueID{NodeID: nid, AttributeID: ua.AttributeIDValue})
		idMap = append(idMap, p.PointBizID)
	}
	if len(nodes) == 0 {
		return result, nil
	}

	req := &ua.ReadRequest{
		MaxAge:             2000,
		NodesToRead:        nodes,
		TimestampsToReturn: ua.TimestampsToReturnBoth,
	}
	resp, err := cli.Read(ctx, req)
	if err != nil {
		c.setErr(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return nil, fmt.Errorf("OPC UA BatchRead 失败: %w", err)
	}
	c.setErr(nil)
	if resp == nil {
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return nil, errors.New("OPC UA BatchRead 无响应")
	}
	for i, r := range resp.Results {
		if i >= len(idMap) {
			break
		}
		id := idMap[i]
		pt := points[i]
		dt := ""
		if pt != nil {
			dt = pt.DataType
		}
		if r.Status != ua.StatusOK {
			result[id] = protocol.Value{DataType: dt, Quality: "bad"}
			continue
		}
		v := valueFromVariant(r.Value, dt)
		v.Quality = "good"
		result[id] = v
	}
	c.readCount.Add(1)
	c.lastReadOkMs.Store(time.Now().UnixMilli())
	return result, nil
}

func (c *opcuaChannel) Write(ctx context.Context, address string, value any, dataType string) error {
	nid, err := parseOPCUANodeID(address)
	if err != nil {
		return err
	}
	v, err := variantFromValue(value, dataType)
	if err != nil {
		return err
	}

	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil {
		return errors.New("OPC UA 未连接")
	}

	// writeMu 串行化所有写请求（包括心跳 Write），避免与订阅通知并发访问 cli
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return errors.New("OPC UA channel 已关闭")
	}

	req := &ua.WriteRequest{
		NodesToWrite: []*ua.WriteValue{
			{NodeID: nid, AttributeID: ua.AttributeIDValue, Value: &ua.DataValue{Value: v}},
		},
	}
	resp, err := cli.Write(ctx, req)
	if err != nil {
		c.setErr(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return fmt.Errorf("OPC UA Write 失败: %w", err)
	}
	c.setErr(nil)
	if resp == nil || len(resp.Results) == 0 || resp.Results[0] != ua.StatusOK {
		st := ua.StatusCode(0)
		if resp != nil && len(resp.Results) > 0 {
			st = resp.Results[0]
		}
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return fmt.Errorf("OPC UA Write 状态非 OK: %v", st)
	}
	c.writeCount.Add(1)
	c.lastWriteOkMs.Store(time.Now().UnixMilli())
	return nil
}

func (c *opcuaChannel) BatchWrite(ctx context.Context, writes []protocol.WriteOp) error {
	if len(writes) == 0 {
		return nil
	}
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil {
		return errors.New("OPC UA 未连接")
	}

	nv := make([]*ua.WriteValue, 0, len(writes))
	for _, w := range writes {
		nid, err := parseOPCUANodeID(w.Address)
		if err != nil {
			return fmt.Errorf("地址 %s 解析失败: %w", w.Address, err)
		}
		v, err := variantFromValue(w.Value, w.DataType)
		if err != nil {
			return fmt.Errorf("值 %v 转换失败: %w", w.Value, err)
		}
		nv = append(nv, &ua.WriteValue{
			NodeID:      nid,
			AttributeID: ua.AttributeIDValue,
			Value:       &ua.DataValue{Value: v},
		})
	}
	req := &ua.WriteRequest{NodesToWrite: nv}
	resp, err := cli.Write(ctx, req)
	if err != nil {
		c.setErr(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return fmt.Errorf("OPC UA BatchWrite 失败: %w", err)
	}
	c.setErr(nil)
	for i, st := range resp.Results {
		if st != ua.StatusOK {
			c.errorCount.Add(1)
			c.lastErrorMs.Store(time.Now().UnixMilli())
			return fmt.Errorf("OPC UA BatchWrite[%d] 状态非 OK: %v", i, st)
		}
	}
	c.writeCount.Add(1)
	c.lastWriteOkMs.Store(time.Now().UnixMilli())
	return nil
}

// touch 更新最近使用时间。
func (c *opcuaChannel) touch() { c.lastUsed.Store(time.Now().UnixNano()) }

// IsConnected 返回本地视图的连接状态。
func (c *opcuaChannel) IsConnected() bool {
	return c.connected.Load() && !c.closed.Load()
}

// LastActiveAt 返回最近一次成功操作的本地时间。
func (c *opcuaChannel) LastActiveAt() time.Time {
	return time.Unix(0, c.lastUsed.Load())
}

// LastError 返回最近一次失败的错误信息。
func (c *opcuaChannel) LastError() string {
	if p := c.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

// ProbeActive 探活：查询 gopcua 客户端的 ConnState。
func (c *opcuaChannel) ProbeActive(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		err := fmt.Errorf("OPC UA channel %s 未连接", c.channelID)
		c.setErr(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	st := c.client.State()
	if st != opcua.Connected {
		err := fmt.Errorf("OPC UA channel %s state=%s", c.channelID, st)
		c.setErr(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	c.touch()
	c.setErr(nil)
	return nil
}

// setErr 原子地记录最近一次错误。
func (c *opcuaChannel) setErr(err error) {
	if err == nil {
		c.lastErr.Store(nil)
		c.lastErrAt.Store(0)
		return
	}
	msg := err.Error()
	c.lastErr.Store(&msg)
	c.lastErrAt.Store(time.Now().UnixNano())
}

// LastErrorAt 返回最近一次 lastErr 被设置为非空的时间戳。
func (c *opcuaChannel) LastErrorAt() time.Time {
	ns := c.lastErrAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func (c *opcuaChannel) LastUsedAt() time.Time {
	return time.Unix(0, c.lastUsed.Load())
}

// Stats 返回 OPC UA 协议特定运行时数据。
func (c *opcuaChannel) Stats() map[string]any {
	c.mu.Lock()
	var state string
	if c.client != nil {
		state = c.client.State().String()
	} else {
		state = "Disconnected"
	}
	endpoint := c.cfg.Endpoint
	secMode := c.cfg.SecurityMode
	secPol := c.cfg.SecurityPolicy
	authType := c.cfg.AuthType
	pubIntMs := c.cfg.PublishIntervalMs
	c.mu.Unlock()

	c.subMu.Lock()
	subCount := len(c.handleToA)
	subEnabled := c.subEnabled
	c.subMu.Unlock()

	probeMs := int64(0)
	if lastUsedNs := c.lastUsed.Load(); lastUsedNs != 0 {
		probeMs = time.Unix(0, lastUsedNs).UnixMilli()
	}
	return map[string]any{
		"connection_state":    state,
		"endpoint":            endpoint,
		"security_mode":       secMode,
		"security_policy":     secPol,
		"auth_type":           authType,
		"subscription_active": subEnabled,
		"subscription_count":  subCount,
		"publish_interval_ms": pubIntMs,
		"read_count":          c.readCount.Load(),
		"write_count":         c.writeCount.Load(),
		"error_count":         c.errorCount.Load(),
		"last_read_ok_ms":     c.lastReadOkMs.Load(),
		"last_write_ok_ms":    c.lastWriteOkMs.Load(),
		"last_error_ms":       c.lastErrorMs.Load(),
		"last_probe_ok_ms":    probeMs,
	}
}

// === 工具 ===

// parseOPCUANodeID 解析 OPC UA 地址到 *ua.NodeID.
//
// 支持格式：
//
//	ns=2;s=Channel1.Device1.Tag       字符串节点（s= String）
//	ns=2;i=1234                       数字节点（i= Int / Numeric）
//	ns=2;g=00000000-0000-0000-0000-000000000000  GUID
//	ns=2;b=Base64                      ByteString
//	s=Channel1.Device1.Tag             简写（默认 ns=0）
//	i=1234                             简写
//	/Channel1/Device1/Tag             斜杠简写（→ ns=0;s=/Channel1/Device1/Tag）
func parseOPCUANodeID(addr string) (*ua.NodeID, error) {
	a := strings.TrimSpace(addr)
	if a == "" {
		return nil, errors.New("OPC UA 地址为空")
	}
	if strings.Contains(a, "=") {
		nid, err := ua.ParseNodeID(a)
		if err != nil {
			return nil, fmt.Errorf("OPC UA NodeID 解析失败 %q: %w", a, err)
		}
		return nid, nil
	}
	if strings.HasPrefix(a, "/") {
		return ua.NewStringNodeID(0, a), nil
	}
	if n, err := strconv.ParseUint(a, 10, 32); err == nil {
		return ua.NewNumericNodeID(0, uint32(n)), nil
	}
	return ua.NewStringNodeID(0, a), nil
}

// valueFromVariant 把 ua.Variant 解码为统一 Value。
func valueFromVariant(v *ua.Variant, dataType string) protocol.Value {
	out := protocol.Value{DataType: dataType}
	if v == nil {
		out.Quality = "bad"
		return out
	}
	switch dataType {
	case "bool":
		if x, ok := v.Value().(bool); ok {
			out.Bool = x
		}
	case "int16", "int32", "int64":
		if x, ok := toInt64Any(v.Value()); ok {
			out.Int64 = x
		}
	case "uint16", "uint32", "uint64":
		if x, ok := toUint64Any(v.Value()); ok {
			out.Uint64 = x
		}
	case "float32", "float64":
		if x, ok := toFloat64Any(v.Value()); ok {
			out.Float64 = x
		}
	case "string":
		if x, ok := v.Value().(string); ok {
			out.String = x
		}
	default:
		rv := v.Value()
		switch x := rv.(type) {
		case bool:
			out.Bool = x
			out.DataType = "bool"
		case int8, int16, int32, int64, int:
			if n, ok := toInt64Any(rv); ok {
				out.Int64 = n
				out.DataType = "int64"
			}
		case uint8, uint16, uint32, uint64, uint:
			if n, ok := toUint64Any(rv); ok {
				out.Uint64 = n
				out.DataType = "uint64"
			}
		case float32, float64:
			if f, ok := toFloat64Any(rv); ok {
				out.Float64 = f
				out.DataType = "float64"
			}
		case string:
			out.String = x
			out.DataType = "string"
		default:
			out.String = fmt.Sprintf("%v", rv)
			out.DataType = "string"
		}
	}
	return out
}

func valueFromDataChange(dv *ua.DataValue) protocol.Value {
	if dv == nil {
		return protocol.Value{Quality: "bad"}
	}
	return valueFromVariant(dv.Value, "")
}

// variantFromValue 把外部 any → ua.Variant。
func variantFromValue(v any, dataType string) (*ua.Variant, error) {
	switch dataType {
	case "bool":
		b, ok := toBool(v)
		if !ok {
			return nil, fmt.Errorf("bool 类型转换失败: %T", v)
		}
		return mustVariant(b)
	case "int16":
		n, err := toInt64(v)
		if err != nil {
			return nil, err
		}
		return mustVariant(int16(n))
	case "int32":
		n, err := toInt64(v)
		if err != nil {
			return nil, err
		}
		return mustVariant(int32(n))
	case "int64":
		n, err := toInt64(v)
		if err != nil {
			return nil, err
		}
		return mustVariant(n)
	case "uint16":
		n, err := toInt64(v)
		if err != nil {
			return nil, err
		}
		return mustVariant(uint16(n))
	case "uint32":
		n, err := toInt64(v)
		if err != nil {
			return nil, err
		}
		return mustVariant(uint32(n))
	case "uint64":
		n, err := toInt64(v)
		if err != nil {
			return nil, err
		}
		return mustVariant(uint64(n))
	case "float32":
		f, ok := toFloat64Any(v)
		if !ok {
			return nil, fmt.Errorf("float32 类型转换失败: %T", v)
		}
		return mustVariant(float32(f))
	case "float64":
		f, ok := toFloat64Any(v)
		if !ok {
			return nil, fmt.Errorf("float64 类型转换失败: %T", v)
		}
		return mustVariant(f)
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("string 类型转换失败: %T", v)
		}
		return mustVariant(s)
	default:
		return nil, fmt.Errorf("OPC UA 不支持的数据类型: %s", dataType)
	}
}

func mustVariant(v any) (*ua.Variant, error) {
	return ua.NewVariant(v)
}

func toInt64Any(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case uint8:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint64:
		return int64(x), true
	case float32:
		return int64(x), true
	case float64:
		return int64(x), true
	}
	return 0, false
}

func toUint64Any(v any) (uint64, bool) {
	switch x := v.(type) {
	case int:
		return uint64(x), true
	case int8:
		return uint64(x), true
	case int16:
		return uint64(x), true
	case int32:
		return uint64(x), true
	case int64:
		return uint64(x), true
	case uint8:
		return uint64(x), true
	case uint16:
		return uint64(x), true
	case uint32:
		return uint64(x), true
	case uint64:
		return x, true
	}
	return 0, false
}

func toFloat64Any(v any) (float64, bool) {
	switch x := v.(type) {
	case float32:
		return float64(x), true
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}

func toBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case int:
		return x != 0, true
	case int64:
		return x != 0, true
	case float64:
		return x != 0, true
	}
	return false, false
}

func toInt64(v any) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint:
		return int64(x), nil
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		return int64(x), nil
	case float32:
		return int64(x), nil
	case float64:
		return int64(x), nil
	}
	return 0, fmt.Errorf("无法转 int64: %T", v)
}

// init OPC UA 协议自注册（_all.go 触发）。
func init() {
	if err := protocol.Register(newPlugin()); err != nil {
		panic(err)
	}
}
