// InfluxDB Target 插件实现，覆盖 V1.x / V1.8+ / V2.x / V3.x。
//
// 协议差异（HTTP 写入路径）：
//
//	V1.0–1.7: POST /write?db=<db>&precision=ns        无 Authorization 头（可选 Basic Auth）
//	V1.8+ (Flux): POST /api/v2/write?org=...&bucket=...  Authorization: Token <token>
//	V2.x:        POST /api/v2/write?org=...&bucket=...  Authorization: Token <token>
//	V3.x:        POST /api/v2/write（V3 server 默认开 V2 兼容 endpoint，库自动走 V2）
//
// Line Protocol 格式跨版本完全一致：
//
//	plc,device=...,source=...,point=... value=<v> <ts_ns>
//
// 库依赖：
//   - V2 / V1.8+ / V3: github.com/influxdata/influxdb-client-go/v2（官方）
//   - V1.0–1.7: 无第三方库，自己 net/http 拼请求
//
// 失败处理（§6.4.3）：库内部自动重试；最终失败 → 返回 error → forwarder 计数后丢弃（不重试）。
package target

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/api/write"

	"github.com/plcbridge/plcbridge/internal/platform"
)

const influxMeasurement = "plc"

// InfluxDBVersion 协议版本。
type InfluxDBVersion string

const (
	InfluxV1   InfluxDBVersion = "1.x"        // V1.0–1.7（无 V2 兼容 endpoint）
	InfluxV1_8 InfluxDBVersion = "1.8-flux"   // V1.8+ Flux（走 V2 兼容）
	InfluxV2   InfluxDBVersion = "2.0"        // V2.x
	InfluxV3   InfluxDBVersion = "3.x"        // V3.x（server 默认开 V2 兼容，client 也走 V2 endpoint）
)

// IsValid 检查版本字符串是否合法。
func (v InfluxDBVersion) IsValid() bool {
	switch v {
	case InfluxV1, InfluxV1_8, InfluxV2, InfluxV3:
		return true
	}
	return false
}

// === 插件注册 ===

type influxdbPlugin struct{}

func newInfluxDBPlugin() TargetPlugin { return &influxdbPlugin{} }

func (p *influxdbPlugin) Name() string    { return "influxdb" }
func (p *influxdbPlugin) Version() string { return "1.1.0" }

// ConfigSchema：version 字段决定协议分支，其它字段按 version 动态校验。
//   - V1.x：必填 db（数据库名）、username/password（可选）
//   - V1.8+/V2.x/V3.x：必填 org、bucket、token
func (p *influxdbPlugin) ConfigSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"version": {
				"type": "string",
				"enum": ["1.x", "1.8-flux", "2.0", "3.x"],
				"default": "2.0",
				"description": "InfluxDB 版本：1.x（V1 协议）、1.8-flux（V1.8+ 走 V2 兼容）、2.0、3.x（V3 server 默认 V2 兼容）"
			},
			"db":                {"type": "string", "description": "V1.x 必填：数据库名"},
			"username":          {"type": "string", "description": "V1.x 可选：Basic Auth 用户名"},
			"password":          {"type": "string", "description": "V1.x 可选：Basic Auth 密码"},
			"org":               {"type": "string", "description": "V1.8+/V2/V3 必填：组织名"},
			"bucket":            {"type": "string", "description": "V1.8+/V2/V3 必填：bucket 名"},
			"token":             {"type": "string", "description": "V1.8+/V2/V3 必填：API token"},
			"connection_timeout_ms": {"type": "integer", "default": 2000, "minimum": 100, "maximum": 30000},
			"batch_size":        {"type": "integer", "default": 1000, "minimum": 1},
			"flush_interval_ms": {"type": "integer", "default": 1000, "minimum": 100},
			"precision":         {"type": "string", "enum": ["ns","us","ms","s"], "default": "ns",
			                       "description": "Line Protocol 时间戳精度"}
		}
	}`)
}

// InfluxDBConfig 是统一的配置结构（覆盖 4 个版本）。
type InfluxDBConfig struct {
	Version InfluxDBVersion `json:"version"`

	// V1.x 字段
	DB       string `json:"db"`
	Username string `json:"username"`
	Password string `json:"password"`

	// V1.8+/V2/V3 字段
	Org    string `json:"org"`
	Bucket string `json:"bucket"`
	Token  string `json:"token"`

	// 通用
	ConnectionTimeoutMs int    `json:"connection_timeout_ms"`
	BatchSize           int    `json:"batch_size"`
	FlushIntervalMs     int    `json:"flush_interval_ms"`
	Precision           string `json:"precision"`
}

func (p *influxdbPlugin) ParseConfig(raw []byte) (any, error) {
	var cfg InfluxDBConfig
	if len(raw) == 0 {
		return nil, errors.New("InfluxDB Target 配置不能为空")
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("InfluxDB 配置解析失败: %w", err)
	}
	if cfg.Version == "" {
		cfg.Version = InfluxV2 // 默认 V2
	}
	if !cfg.Version.IsValid() {
		return nil, fmt.Errorf("InfluxDB version %q 非法（合法: 1.x / 1.8-flux / 2.0 / 3.x）", cfg.Version)
	}
	// 按版本校验必填字段
	switch cfg.Version {
	case InfluxV1:
		if cfg.DB == "" {
			return nil, errors.New("InfluxDB V1.x 必须配置 db（数据库名）")
		}
	case InfluxV1_8, InfluxV2, InfluxV3:
		if cfg.Org == "" || cfg.Bucket == "" || cfg.Token == "" {
			return nil, fmt.Errorf("InfluxDB %s 必须配置 org/bucket/token", cfg.Version)
		}
	}
	if cfg.ConnectionTimeoutMs == 0 {
		cfg.ConnectionTimeoutMs = 2000
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 1000
	}
	if cfg.FlushIntervalMs == 0 {
		cfg.FlushIntervalMs = 1000
	}
	if cfg.Precision == "" {
		cfg.Precision = "ns"
	}
	return cfg, nil
}

func (p *influxdbPlugin) Init(_ context.Context) error { return nil }
func (p *influxdbPlugin) Close() error                { return nil }

func (p *influxdbPlugin) CreateTarget(t *platform.Target, channel *platform.Channel) (TargetClient, error) {
	if channel.Type != "influxdb" {
		return nil, fmt.Errorf("InfluxDB Target 需要 influxdb Channel（got %s）", channel.Type)
	}
	if t == nil || t.Topic == "" {
		return nil, errors.New("InfluxDB Target 必填 topic（用作 InfluxDB measurement 名）")
	}
	cfgRaw, err := p.ParseConfig(channel.Config)
	if err != nil {
		return nil, err
	}
	cfg := cfgRaw.(InfluxDBConfig)

	switch cfg.Version {
	case InfluxV1:
		return &influxDBV1Client{
			addr:   channel.Address,
			cfg:    cfg,
			target: t,
		}, nil
	case InfluxV1_8, InfluxV2, InfluxV3:
		// V3 server 默认提供 V2 兼容 endpoint，client 用 V2 库直连即可
		return &influxDBV2Client{
			addr:   channel.Address,
			cfg:    cfg,
			target: t,
		}, nil
	}
	return nil, fmt.Errorf("未实现的 InfluxDB 版本: %s", cfg.Version)
}

// === V2 / V1.8+ / V3 客户端（共用 V2 协议） ===

type influxDBV2Client struct {
	addr   string
	cfg    InfluxDBConfig
	target *platform.Target

	mu      sync.Mutex
	cli     influxdb2.Client
	write   api.WriteAPIBlocking
	healthy bool

	// PR1+PR2 运行时统计（web 仪表盘用）
	lastErr     atomic.Pointer[string]
	lastErrAt   atomic.Int64
	sentCount   atomic.Uint64   // PR1: 累计成功 batch 数
	sentPoints  atomic.Uint64   // PR2: 累计成功 point 数
	errorCount  atomic.Uint64
	lastSendOkMs atomic.Int64
	lastErrorMs  atomic.Int64  // PR2: 与 lastErrAt 等价但用 ms
	connectCount atomic.Uint64 // PR2: ensureClient 成功次数
	lastConnectMs atomic.Int64
	healthCheckCount atomic.Uint64
	lastHealthCheckMs atomic.Int64
}

func (c *influxDBV2Client) ensureClient() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cli != nil && c.healthy {
		return nil
	}
	if c.cli != nil {
		c.cli.Close()
		c.cli = nil
		c.write = nil
	}
	cli := influxdb2.NewClient(c.addr, c.cfg.Token)
	c.cli = cli
	c.write = cli.WriteAPIBlocking(c.cfg.Org, c.cfg.Bucket)
	c.healthy = true
	c.connectCount.Add(1)
	c.lastConnectMs.Store(time.Now().UnixMilli())
	return nil
}

// Close 关闭 V2 客户端，释放 HTTP conn pool。
//
// 幂等：多次调用安全。
func (c *influxDBV2Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cli == nil {
		return nil
	}
	c.cli.Close()
	c.cli = nil
	c.write = nil
	c.healthy = false
	return nil
}

func (c *influxDBV2Client) HealthCheck(ctx context.Context) error {
	c.healthCheckCount.Add(1)
	c.lastHealthCheckMs.Store(time.Now().UnixMilli())
	if err := c.ensureClient(); err != nil {
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	pingCtx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.ConnectionTimeoutMs)*time.Millisecond)
	defer cancel()
	ok, err := c.cli.Ping(pingCtx)
	if err != nil {
		c.markUnhealthy()
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	if !ok {
		c.markUnhealthy()
		err := errors.New("InfluxDB Ping 返回 false")
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	return nil
}

func (c *influxDBV2Client) markUnhealthy() {
	c.mu.Lock()
	c.healthy = false
	c.mu.Unlock()
}

// ProbeConnect 主动探测 InfluxDB V2 连接。
func (c *influxDBV2Client) ProbeConnect(ctx context.Context) error {
	return c.HealthCheck(ctx)
}

func (c *influxDBV2Client) Send(ctx context.Context, batch []PointValue) error {
	if len(batch) == 0 {
		return nil
	}
	if err := c.ensureClient(); err != nil {
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}

	points := make([]*write.Point, 0, len(batch))
	for _, pv := range batch {
		p, err := buildInfluxPoint(pv, c.target)
		if err != nil {
			continue
		}
		points = append(points, p)
	}
	if len(points) == 0 {
		return nil
	}

	if err := c.write.WritePoint(ctx, points...); err != nil {
		c.markUnhealthy()
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return fmt.Errorf("InfluxDB WritePoint 失败: %w", err)
	}
	c.setLastError(nil)
	c.sentCount.Add(1)              // PR2: 按 batch 计
	c.sentPoints.Add(uint64(len(points))) // PR2: 按 point 计
	c.lastSendOkMs.Store(time.Now().UnixMilli())
	return nil
}

// IsConnected 返回本地视图：cli 非空且 healthy。
func (c *influxDBV2Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cli != nil && c.healthy
}

// LastError 返回最近一次 Send / HealthCheck 失败的错误信息。
func (c *influxDBV2Client) LastError() string {
	if p := c.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

// setLastError 原子地记录最近一次错误（nil = 清空）。
func (c *influxDBV2Client) setLastError(err error) {
	if err == nil {
		c.lastErr.Store(nil)
		c.lastErrAt.Store(0)
		return
	}
	msg := err.Error()
	c.lastErr.Store(&msg)
	c.lastErrAt.Store(time.Now().UnixNano())
}

// Stats 返回 InfluxDB V2 协议特定运行时数据。
func (c *influxDBV2Client) Stats() map[string]any {
	c.mu.Lock()
	healthy := c.healthy && c.cli != nil
	addr := c.addr
	org := c.cfg.Org
	bucket := c.cfg.Bucket
	c.mu.Unlock()
	state := "Disconnected"
	if healthy {
		state = "Connected"
	}
	return map[string]any{
		"connection_state":    state,
		"protocol":            "influxdb",
		"version":             string(InfluxV2),
		"address":             addr,
		"org":                 org,
		"bucket":              bucket,
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

// === V1 客户端（自实现 net/http） ===

type influxDBV1Client struct {
	addr   string
	cfg    InfluxDBConfig
	target *platform.Target

	mu      sync.Mutex
	httpCli *http.Client
	healthy bool

	// PR1+PR2 运行时统计
	lastErr     atomic.Pointer[string]
	lastErrAt   atomic.Int64
	sentCount   atomic.Uint64
	sentPoints  atomic.Uint64
	errorCount  atomic.Uint64
	lastSendOkMs atomic.Int64
	lastErrorMs  atomic.Int64
	connectCount atomic.Uint64
	lastConnectMs atomic.Int64
	healthCheckCount atomic.Uint64
	lastHealthCheckMs atomic.Int64
}

// ensureClient 只在初始化阶段上锁；后续 Send 不需要持锁（httpCli 本身并发安全）。
func (c *influxDBV1Client) ensureClient() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.httpCli != nil && c.healthy {
		return
	}
	c.httpCli = &http.Client{
		Timeout: time.Duration(c.cfg.ConnectionTimeoutMs) * time.Millisecond,
	}
	c.healthy = true
	c.connectCount.Add(1)
	c.lastConnectMs.Store(time.Now().UnixMilli())
}

func (c *influxDBV1Client) markUnhealthy() {
	c.mu.Lock()
	c.healthy = false
	c.mu.Unlock()
}

// Send 走 V1 协议：
//
//	POST {addr}/write?db={cfg.db}&precision={cfg.precision}
//	Authorization: Basic <base64(user:pass)>  （仅当配置 username 时）
//	Body: <Line Protocol，每行一个 point，\n 分隔>
func (c *influxDBV1Client) Send(ctx context.Context, batch []PointValue) error {
	if len(batch) == 0 {
		return nil
	}
	c.ensureClient()

	// 先算 point 数（用于 sentPoints 计数）
	pointCount := 0
	var sb strings.Builder
	for _, pv := range batch {
		lp, err := buildV1LineProtocol(pv, c.target)
		if err != nil {
			continue
		}
		sb.WriteString(lp)
		sb.WriteByte('\n')
		pointCount++
	}
	body := sb.String()
	if body == "" {
		return nil
	}
	// Influx V1 strict parser 把每个非空行当一行；尾随换行一般无害，但部分 v1.8+
	// 在 line protocol 末尾追加空行时报警，去掉以减少噪声。
	body = strings.TrimRight(body, "\n") + "\n"

	endpoint, err := c.buildV1WriteURL()
	if err != nil {
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(body)))
	if err != nil {
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if c.cfg.Username != "" {
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}

	resp, err := c.httpCli.Do(req)
	if err != nil {
		c.markUnhealthy()
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return fmt.Errorf("InfluxDB V1 POST 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.markUnhealthy()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("InfluxDB V1 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	c.setLastError(nil)
	c.sentCount.Add(1)
	c.sentPoints.Add(uint64(pointCount))
	c.lastSendOkMs.Store(time.Now().UnixMilli())
	return nil
}

// IsConnected 返回本地视图：httpCli 非空且 healthy。
func (c *influxDBV1Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.httpCli != nil && c.healthy
}

// LastError 返回最近一次 Send / HealthCheck 失败的错误信息。
func (c *influxDBV1Client) LastError() string {
	if p := c.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

// setLastError 原子地记录最近一次错误。
func (c *influxDBV1Client) setLastError(err error) {
	if err == nil {
		c.lastErr.Store(nil)
		c.lastErrAt.Store(0)
		return
	}
	msg := err.Error()
	c.lastErr.Store(&msg)
	c.lastErrAt.Store(time.Now().UnixNano())
}

// Stats 返回 InfluxDB V1 协议特定运行时数据。
func (c *influxDBV1Client) Stats() map[string]any {
	c.mu.Lock()
	healthy := c.healthy && c.httpCli != nil
	addr := c.addr
	db := c.cfg.DB
	c.mu.Unlock()
	state := "Disconnected"
	if healthy {
		state = "Connected"
	}
	return map[string]any{
		"connection_state":    state,
		"protocol":            "influxdb",
		"version":             string(InfluxV1),
		"address":             addr,
		"db":                  db,
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

func (c *influxDBV1Client) buildV1WriteURL() (string, error) {
	u, err := url.Parse(c.addr)
	if err != nil {
		return "", fmt.Errorf("InfluxDB V1 address 解析失败: %w", err)
	}
	// 拼路径：{scheme}://{host}/write
	u.Path = "/write"
	q := u.Query()
	q.Set("db", c.cfg.DB)
	q.Set("precision", c.cfg.Precision)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *influxDBV1Client) HealthCheck(ctx context.Context) error {
	c.healthCheckCount.Add(1)
	c.lastHealthCheckMs.Store(time.Now().UnixMilli())
	c.ensureClient()
	// V1 用 /ping endpoint（无需鉴权）
	u, err := url.Parse(c.addr)
	if err != nil {
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	u.Path = "/ping"
	pingCtx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.ConnectionTimeoutMs)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(pingCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	resp, err := c.httpCli.Do(req)
	if err != nil {
		c.markUnhealthy()
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.markUnhealthy()
		err := fmt.Errorf("InfluxDB V1 /ping 返回 %d", resp.StatusCode)
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	return nil
}

// ProbeConnect 主动探测 InfluxDB V1 连接。
//
// 复用 HealthCheck：ensureClient + /ping。区别是 healthCheckCount 不递增（不算探活次数），
// 但 connectCount 在 ensureClient 里会递增（首次建立连接）。
func (c *influxDBV1Client) ProbeConnect(ctx context.Context) error {
	return c.HealthCheck(ctx)
}

// === 共享：Line Protocol 构建 ===

// buildInfluxPoint 给 V2/V3 用，构造官方 write.Point。
//
// 写入约定（2026-10-10 与 InfluxDB 字段冲突故障后修订，2026-10-10 晚再次修订：
// measurement 来源由硬编码常量改为 target.Topic）：
//   - measurement: 来自 target.Topic（用户在 web UI 配的 TOPIC 字段；1 个 InfluxDB
//                  channel 可配多 Target，分别写到不同 measurement）
//                  兜底：target 为 nil 或 topic 为空 → 走默认 "plc"（历史兼容）
//   - tags:        device, source （按 device 维度切分；point 不再做 tag，原因是
//                  防止 point 高基数 + 让 field key 充当 point 标识，匹配 MQTT 语义）
//   - fields:      每个 point 写一个独立 field，key = point_biz_id
//                  （关键：同一 measurement 内不同 point 落到不同 field key，
//                    避免 int/string 类型冲突）
//   - timestamp:   从 PV 取，纳秒精度
func buildInfluxPoint(pv PointValue, target *platform.Target) (*write.Point, error) {
	p := influxdb2.NewPointWithMeasurement(resolveMeasurement(target))
	p.AddTag("device", pv.DeviceBizID)
	p.AddTag("source", pv.SourceBizID)

	fieldVal, err := influxFieldValue(pv.Value, pv.DataType)
	if err != nil {
		return nil, fmt.Errorf("InfluxDB 字段值转换失败 point=%s: %w", pv.PointBizID, err)
	}
	p.AddField(pv.PointBizID, fieldVal)

	ts := pv.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	p.SetTime(ts)
	return p, nil
}

// resolveMeasurement 解析 InfluxDB measurement 名：优先用 target.Topic。
//
// CreateTarget 已 reject 空 topic，正常路径 target.Topic 必有值；这里留兜底
// 防运行期 target 为 nil 等异常场景。
func resolveMeasurement(target *platform.Target) string {
	if target != nil && target.Topic != "" {
		return target.Topic
	}
	return influxMeasurement
}

// buildV1LineProtocol 给 V1 用，纯字符串拼 Line Protocol（V1 库 `influx` 的 Go 版做法）。
//
// 输出格式（2026-10-10 修订后，与 buildInfluxPoint 保持一致）：
//
//	{topic},device=OP01,source=1 M100=2i 1695849600000000000
//	{topic},device=OP01,source=1 No="ABCDEFG" 1695849600000000001
//
// 注意：tag/field 值需要 escape（空格、逗号、等号）：
//   - tag value: 替换 `,` `=` ` ` 为 `\`
//   - field value（字符串）：替换 `"` 为 `\"`，用双引号包裹
func buildV1LineProtocol(pv PointValue, target *platform.Target) (string, error) {
	fieldVal, err := influxFieldValue(pv.Value, pv.DataType)
	if err != nil {
		return "", err
	}
	ts := pv.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	var sb strings.Builder
	sb.WriteString(escapeTag(resolveMeasurement(target)))
	sb.WriteString(",device=")
	sb.WriteString(escapeTag(pv.DeviceBizID))
	sb.WriteString(",source=")
	sb.WriteString(escapeTag(pv.SourceBizID))
	sb.WriteByte(' ')
	sb.WriteString(formatField(pv.PointBizID, fieldVal))
	sb.WriteByte(' ')
	sb.WriteString(formatTimestamp(ts, "ns"))
	return sb.String(), nil
}

func escapeTag(s string) string {
	// Line Protocol tag/measurement 转义：空格、逗号、等号 → `\`
	r := strings.NewReplacer(` `, `\ `, `,`, `\,`, `=`, `\=`)
	return r.Replace(s)
}

func escapeStringField(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}

// formatField 拼 `key=value`，按值类型走 Line Protocol 格式：
//
//	int64 → `100i`
//	float64 → `3.14`
//	bool → `t` / `f`
//	string → `"escaped"`
//
// field key 也需要 escape（空格/逗号/等号），因为 point_biz_id 现在直接当 field key
// 使用（2026-10-10 修订后），用户可能给 point 起名含特殊字符。
func formatField(key string, v any) string {
	k := escapeTag(key) // 复用 tag escape：空格/逗号/等号 → \
	switch x := v.(type) {
	case bool:
		if x {
			return k + "=t"
		}
		return k + "=f"
	case int64:
		return fmt.Sprintf("%s=%di", k, x)
	case float64:
		return fmt.Sprintf("%s=%v", k, x)
	case string:
		return fmt.Sprintf("%s=\"%s\"", k, escapeStringField(x))
	}
	return fmt.Sprintf("%s=\"%v\"", k, escapeStringField(fmt.Sprint(v)))
}

// formatTimestamp 把 time.Time 转成 Line Protocol 的数字时间戳（按 precision）。
func formatTimestamp(ts time.Time, precision string) string {
	switch precision {
	case "s":
		return fmt.Sprintf("%d", ts.Unix())
	case "ms":
		return fmt.Sprintf("%d", ts.UnixMilli())
	case "us":
		return fmt.Sprintf("%d", ts.UnixMicro())
	default: // "ns"
		return fmt.Sprintf("%d", ts.UnixNano())
	}
}

// === 共享：字段类型转换 ===

func influxFieldValue(v any, dataType string) (any, error) {
	switch dataType {
	case "bool":
		switch x := v.(type) {
		case bool:
			return x, nil
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return toInt64Any(x) != 0, nil
		default:
			return fmt.Sprintf("%v", v), nil
		}
	case "int8", "int16", "int32", "int64", "uint8", "uint16", "uint32", "uint64":
		n, err := toInt64AnyErr(v)
		if err == nil {
			return n, nil
		}
		f, ferr := toFloat64Any(v)
		if ferr == nil {
			return f, nil
		}
		return nil, err
	case "float32", "float64":
		f, err := toFloat64Any(v)
		if err != nil {
			return nil, err
		}
		return f, nil
	case "string":
		return fmt.Sprintf("%v", v), nil
	}
	return fmt.Sprintf("%v", v), nil
}

func toInt64Any(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	case float32:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

func toInt64AnyErr(v any) (int64, error) {
	switch v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return toInt64Any(v), nil
	}
	return 0, fmt.Errorf("无法转 int64: %T", v)
}

func toFloat64Any(v any) (float64, error) {
	switch x := v.(type) {
	case float32:
		return float64(x), nil
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int8:
		return float64(x), nil
	case int16:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case uint:
		return float64(x), nil
	case uint8:
		return float64(x), nil
	case uint16:
		return float64(x), nil
	case uint32:
		return float64(x), nil
	case uint64:
		return float64(x), nil
	}
	return 0, fmt.Errorf("无法转 float64: %T", v)
}
