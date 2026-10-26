// Package platform 定义 plcbridge 核心领域模型（对应 Plan.md §7 平台数据模型）。
//
// 5 个核心实体：Channel / Device / Source / Point / Target
// + Config 聚合根持有完整配置快照。
//
// 设计原则：
//  1. 纯数据结构，无业务逻辑，不引用任何 internal 包
//  2. 与 config.json JSON Schema 严格对齐
//  3. 与 Web 后台 SQLite schema 严格对齐（§7.7）
package platform

import "encoding/json"

// Channel 网络通道抽象（§4 术语表 / §7.2）。
//
// role 决定是采集还是转发（collect / forward）。同一 Channel 不能同时承担两种角色。
type Channel struct {
	ChannelBizID string          `json:"channel_biz_id"`
	Role         string          `json:"role"` // "collect" | "forward"
	Type         string          `json:"type"` // "s7" | "modbus_tcp" | "opcua" | "mqtt" | "influxdb"
	Address      string          `json:"address"`
	Config       json.RawMessage `json:"config,omitempty"` // 协议私有参数（JSON Schema 驱动 UI，§9.1）

	// 心跳配置（§6.3，仅 S7 / ModbusTCP 需要）
	HeartbeatAddress    string `json:"heartbeat_address,omitempty"`
	HeartbeatMaxMs      int    `json:"heartbeat_max_interval_ms,omitempty"` // 默认 5000
	HeartbeatEnabled    bool   `json:"heartbeat_enabled,omitempty"`
	HeartbeatDebounceMs int    `json:"heartbeat_debounce_ms,omitempty"` // 默认 50

	// 队列与超时（§3 关键参数）
	QueueSize int `json:"queue_size,omitempty"` // Target 队列容量，默认 10000
	TimeoutMs int `json:"timeout_ms,omitempty"` // 单次操作超时，默认 300（采集）/ 500（回写）
}

// Device 物理设备（§7.3）。
//
// 一个 Device 可有多种协议接入（多个 Source），通过 channel_biz_id 引用 collect Channel。
type Device struct {
	DeviceBizID string    `json:"device_biz_id"`
	Name        string    `json:"name"`
	Enabled     bool      `json:"enabled"`
	Sources     []*Source `json:"sources"`
	Targets     []*Target `json:"targets"`
}

// Source 数据采集源（§7.4）。
//
// Source 1:1 引用 collect Channel（一个 Source 用一个采集连接）；
// 多个 Source 可共享同一 collect Channel（Channel 内部按连接复用）。
type Source struct {
	SourceBizID       string   `json:"source_biz_id"`
	ChannelBizID      string   `json:"channel_biz_id"`
	CollectEnabled    bool     `json:"collect_enabled"`
	WriteEnabled      bool     `json:"write_enabled"`
	CollectIntervalMs int      `json:"collect_interval_ms"` // 默认 1000
	TimeoutMs         int      `json:"timeout_ms,omitempty"`
	Points            []*Point `json:"points"`
}

// Point 数据点位（§7.5）。
//
// 点位本身没有转发开关（§6.5），是否被转发由 Target.point_biz_ids 决定。
type Point struct {
	PointBizID     string `json:"point_biz_id"`
	Name           string `json:"name"`
	Address        string `json:"address"`   // 设备侧地址，如 "DB1.DBD0"
	DataType       string `json:"data_type"` // bool / int16 / uint16 / int32 / uint32 / float32 / float64 / string
	CollectEnabled bool   `json:"collect_enabled"`
	WriteEnabled   bool   `json:"write_enabled"`
	// StringLength 仅 data_type=="string" 时使用：S7 STRING layout 字符数 N（S7 header 4 字节 + N 字节 = 4+N 总长）。
	// 取值 1..55，0 表示未配置（解码时回退到 64，并打点为"未配置"以便用户在 dashboard 改）。
	StringLength int `json:"string_length,omitempty"`
}

// Target 转发目标（§7.6）。
//
// Target 1:1 引用 forward Channel（一个 Target 用一个下游连接）；
// Target 通过 point_biz_ids 指定转发哪些点位，所匹配的 Source 范围由所选 Point 的归属自动推导。
type Target struct {
	TargetBizID         string   `json:"target_biz_id"`
	ForwardChannelBizID string   `json:"forward_channel_biz_id"`
	PointBizIDs         []string `json:"point_biz_ids"`    // ["*"] 表示所有点位
	Topic               string   `json:"topic,omitempty"`  // 转发到的 MQTT Topic（必填）
	Format              string   `json:"format,omitempty"` // "json" | "line_protocol" | "raw"
}

// Config 是 plcbridge 完整运行时配置（对应 config.json）。
//
// 与 Plan.md §4 / §7 数据模型严格对齐：5 个核心实体（Channel / Device / Source / Point / Target）
// + Target ↔ Point 关联通过 Target.point_biz_ids 表达（Source 范围由 Point 归属自动推导）。
type Config struct {
	Version  int        `json:"version"`
	Channels []*Channel `json:"channels"`
	Devices  []*Device  `json:"devices"`
}

// Snapshot 是 config.json 在内存中的原子快照。
type Snapshot struct {
	LoadedAt    int64  // Unix timestamp
	ConfigBytes []byte // 原始 JSON bytes
}

// ChannelByBizID 返回 channel_biz_id 对应的 Channel，不存在返回 nil。
func (c *Config) ChannelByBizID(id string) *Channel {
	for _, ch := range c.Channels {
		if ch.ChannelBizID == id {
			return ch
		}
	}
	return nil
}

// DeviceByBizID 返回 device_biz_id 对应的 Device，不存在返回 nil。
func (c *Config) DeviceByBizID(id string) *Device {
	for _, d := range c.Devices {
		if d.DeviceBizID == id {
			return d
		}
	}
	return nil
}

// AllChannels 返回所有 Channel。
func (c *Config) AllChannels() []*Channel { return c.Channels }

// AllDevices 返回所有 Device。
func (c *Config) AllDevices() []*Device { return c.Devices }

// CollectChannels 返回所有 collect 角色的 Channel。
func (c *Config) CollectChannels() []*Channel {
	out := make([]*Channel, 0, len(c.Channels))
	for _, ch := range c.Channels {
		if ch.Role == "collect" {
			out = append(out, ch)
		}
	}
	return out
}

// ForwardChannels 返回所有 forward 角色的 Channel。
func (c *Config) ForwardChannels() []*Channel {
	out := make([]*Channel, 0, len(c.Channels))
	for _, ch := range c.Channels {
		if ch.Role == "forward" {
			out = append(out, ch)
		}
	}
	return out
}
