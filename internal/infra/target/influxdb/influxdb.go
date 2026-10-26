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
package influxdb

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
	"github.com/plcbridge/plcbridge/internal/infra/target"
	"github.com/plcbridge/plcbridge/internal/platform"
)

const influxMeasurement = "plc"

// InfluxDBVersion 协议版本。
type InfluxDBVersion string

const (
	InfluxV1   InfluxDBVersion = "1.x"      // V1.0–1.7（无 V2 兼容 endpoint）
	InfluxV1_8 InfluxDBVersion = "1.8-flux" // V1.8+ Flux（走 V2 兼容）
	InfluxV2   InfluxDBVersion = "2.0"      // V2.x
	InfluxV3   InfluxDBVersion = "3.x"      // V3.x（server 默认开 V2 兼容，client 也走 V2 endpoint）
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

func newPlugin() target.TargetPlugin { return &influxdbPlugin{} }

func (p *influxdbPlugin) Name() string    { return "influxdb" }
func (p *influxdbPlugin) Version() string { return "1.1.0" }

// Capabilities InfluxDB Target：支持历史查询。
//
// V1（InfluxQL）与 V2/V1.8+/V3（Flux）客户端都实现了 target.HistoryQueryer，
// 因此 SupportsHistory = true。前端据此渲染「历史趋势」按钮。
func (p *influxdbPlugin) Capabilities() target.Capability {
	return target.Capability{
		SupportsHistory: true,
	}
}

// ConfigSchema：version 字段决定协议分支，其它字段按 version 动态校验。
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
func (p *influxdbPlugin) Close() error                 { return nil }

func (p *influxdbPlugin) CreateTarget(t *platform.Target, channel *platform.Channel) (target.TargetClient, error) {
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

	// PR1+PR2 运行时统计
	lastErr           atomic.Pointer[string]
	lastErrAt         atomic.Int64
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
	// 取一份 cli 快照再 Ping。
	//
	// 为什么需要快照（2026-10-25 修复）：ensureClient 可能在另一个 goroutine
	// （Send / 后台健康巡检）里把 c.cli 置 nil 并 Close 掉，直接读 c.cli 会
	// 触发 data race（-race 下会直接报错）+ 潜在 nil 解引用。
	c.mu.Lock()
	cli := c.cli
	c.mu.Unlock()
	if cli == nil {
		err := errors.New("InfluxDB 客户端未初始化")
		c.markUnhealthy()
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	pingCtx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.ConnectionTimeoutMs)*time.Millisecond)
	defer cancel()
	ok, err := cli.Ping(pingCtx)
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
	// 探活成功 → 恢复健康态并清空最近错误。
	//
	// 为什么需要（2026-10-25 修复）：之前成功路径什么都不做，于是
	//   - healthy 永远是 false（一旦失败就再也回不来）
	//   - last_error 永远残留 → 前端「状态」列一直显示"异常"
	// 结果：InfluxDB 恢复后界面依然显示异常，必须手动触发诊断才刷新。
	c.markHealthy()
	c.setLastError(nil)
	return nil
}

func (c *influxDBV2Client) markUnhealthy() {
	c.mu.Lock()
	c.healthy = false
	c.mu.Unlock()
}

// markHealthy 标记连接健康（探活成功后调用）。
func (c *influxDBV2Client) markHealthy() {
	c.mu.Lock()
	c.healthy = true
	c.mu.Unlock()
}

func (c *influxDBV2Client) ProbeConnect(ctx context.Context) error {
	return c.HealthCheck(ctx)
}

func (c *influxDBV2Client) Send(ctx context.Context, batch []target.PointValue) error {
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

	// 取一份 write 快照再写。
	//
	// 为什么需要快照（2026-10-25 修复）：ensureClient / Close 会把 c.write 置 nil，
	// 直接读 c.write 会与后台健康巡检（HealthCheck → ensureClient）产生 data race，
	// 且可能 nil 解引用崩溃。
	c.mu.Lock()
	w := c.write
	c.mu.Unlock()
	if w == nil {
		err := errors.New("InfluxDB 写入通道未初始化")
		c.markUnhealthy()
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	if err := w.WritePoint(ctx, points...); err != nil {
		c.markUnhealthy()
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return fmt.Errorf("InfluxDB WritePoint 失败: %w", err)
	}
	c.setLastError(nil)
	c.sentCount.Add(1)
	c.sentPoints.Add(uint64(len(points)))
	c.lastSendOkMs.Store(time.Now().UnixMilli())
	return nil
}

func (c *influxDBV2Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cli != nil && c.healthy
}

func (c *influxDBV2Client) LastError() string {
	if p := c.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

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
		"connection_state":     state,
		"protocol":             "influxdb",
		"version":              string(InfluxV2),
		"address":              addr,
		"org":                  org,
		"bucket":               bucket,
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

// QueryHistory 查询 InfluxDB 历史数据（Plan3 3.5）。
//
// V2 路径用 Flux 查询。
//
// 安全约束（Plan3 3.5）：
//   - 最大查询时间范围 7 天
//   - 最大返回点数 10000
//   - 不允许跨 measurement 查询
//
// 取数策略（2026-10-22 修正）：**取原始点 + Go 侧保真抽稀**，不用桶聚合。
// 原因与 V1 路径一致，见 buildInfluxFluxRawQuery 的注释。
func (c *influxDBV2Client) QueryHistory(ctx context.Context, hq target.HistoryQuery) (any, error) {
	if c.cli == nil {
		return nil, errors.New("InfluxDB client not connected")
	}
	limit := sanitizeLimit(hq.Limit)
	// 多取一些原始点，给抽稀留出余量。
	rawLimit := sanitizeLimit(limit * 10)

	// 结构化参数 → 受控 Flux 查询串（measurement / field 白名单过滤 + limit 上限）。
	query := buildInfluxFluxRawQuery(hq.Measurement, hq.Field, hq.Duration, rawLimit)
	// 强制追加 LIMIT 和 time range 保护
	secure := query
	if !strings.Contains(strings.ToUpper(secure), "LIMIT") {
		secure += " LIMIT 10000"
	}
	rows, err := c.queryFluxRows(ctx, secure)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		// 原始点查询没数据时再试一次桶聚合查询。
		// （正常情况下原始点一定有数据；这里只是兜底，
		//  比如 retention policy 把明细数据清掉了但聚合数据还在。）
		aggRows, aggErr := c.queryFluxRows(ctx, buildInfluxFluxQuery(hq.Measurement, hq.Field, hq.Duration, limit))
		if aggErr == nil && len(aggRows) > 0 {
			return aggRows, nil
		}
	}
	return decimateHistoryRows(rows, limit), nil
}

// queryFluxRows 执行一条 Flux 查询并转成 []map[string]any。
func (c *influxDBV2Client) queryFluxRows(ctx context.Context, query string) ([]map[string]any, error) {
	q := c.cli.QueryAPI(c.cfg.Org)
	result, err := q.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("InfluxDB query failed: %w", err)
	}
	var rows []map[string]any
	for result.Next() {
		rec := result.Record()
		row := map[string]any{
			"_time":  rec.Time(),
			"_value": rec.Value(),
		}
		if tags := rec.Values(); tags != nil {
			for k, v := range tags {
				if k != "_time" && k != "_value" {
					row[k] = v
				}
			}
		}
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("InfluxDB query result error: %w", err)
	}
	return rows, nil
}

// === V1 客户端（自实现 net/http） ===

type influxDBV1Client struct {
	addr   string
	cfg    InfluxDBConfig
	target *platform.Target

	mu      sync.Mutex
	httpCli *http.Client
	healthy bool

	lastErr           atomic.Pointer[string]
	lastErrAt         atomic.Int64
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

// markHealthy 标记连接健康（探活成功后调用）。
func (c *influxDBV1Client) markHealthy() {
	c.mu.Lock()
	c.healthy = true
	c.mu.Unlock()
}

func (c *influxDBV1Client) Send(ctx context.Context, batch []target.PointValue) error {
	if len(batch) == 0 {
		return nil
	}
	c.ensureClient()

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

	// 取一份 httpCli 快照再发请求（与 V2 同理，避免与后台健康巡检 data race）。
	c.mu.Lock()
	httpCli := c.httpCli
	c.mu.Unlock()
	if httpCli == nil {
		err := errors.New("InfluxDB V1 HTTP 客户端未初始化")
		c.markUnhealthy()
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	resp, err := httpCli.Do(req)
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

func (c *influxDBV1Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.httpCli != nil && c.healthy
}

func (c *influxDBV1Client) LastError() string {
	if p := c.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

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
		"connection_state":     state,
		"protocol":             "influxdb",
		"version":              string(InfluxV1),
		"address":              addr,
		"db":                   db,
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

func (c *influxDBV1Client) buildV1WriteURL() (string, error) {
	u, err := url.Parse(c.addr)
	if err != nil {
		return "", fmt.Errorf("InfluxDB V1 address 解析失败: %w", err)
	}
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
	// 取一份 httpCli 快照再发请求（避免与后台健康巡检 data race）。
	c.mu.Lock()
	httpCli := c.httpCli
	c.mu.Unlock()
	if httpCli == nil {
		err := errors.New("InfluxDB V1 HTTP 客户端未初始化")
		c.markUnhealthy()
		c.setLastError(err)
		c.errorCount.Add(1)
		c.lastErrorMs.Store(time.Now().UnixMilli())
		return err
	}
	resp, err := httpCli.Do(req)
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
	// 探活成功 → 恢复健康态并清空最近错误（同 V2，2026-10-25 修复）。
	c.markHealthy()
	c.setLastError(nil)
	return nil
}

func (c *influxDBV1Client) ProbeConnect(ctx context.Context) error {
	return c.HealthCheck(ctx)
}

// === 共享：Line Protocol 构建 ===

func buildInfluxPoint(pv target.PointValue, tp *platform.Target) (*write.Point, error) {
	p := influxdb2.NewPointWithMeasurement(resolveMeasurement(tp))
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

func resolveMeasurement(tp *platform.Target) string {
	if tp != nil && tp.Topic != "" {
		return tp.Topic
	}
	return influxMeasurement
}

func buildV1LineProtocol(pv target.PointValue, tp *platform.Target) (string, error) {
	fieldVal, err := influxFieldValue(pv.Value, pv.DataType)
	if err != nil {
		return "", err
	}
	ts := pv.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	var sb strings.Builder
	sb.WriteString(escapeTag(resolveMeasurement(tp)))
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
	r := strings.NewReplacer(` `, `\ `, `,`, `\,`, `=`, `\=`)
	return r.Replace(s)
}

func escapeStringField(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}

func formatField(key string, v any) string {
	k := escapeTag(key)
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

func formatTimestamp(ts time.Time, precision string) string {
	switch precision {
	case "s":
		return fmt.Sprintf("%d", ts.Unix())
	case "ms":
		return fmt.Sprintf("%d", ts.UnixMilli())
	case "us":
		return fmt.Sprintf("%d", ts.UnixMicro())
	default:
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

// init InfluxDB Target 自注册（_all.go 触发）。
func init() {
	if err := target.Register(newPlugin()); err != nil {
		panic(err)
	}
}
