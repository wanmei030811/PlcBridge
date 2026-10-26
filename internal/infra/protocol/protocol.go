// Package protocol 定义协议插件接口（采集 + 回写）。
//
// MVP 内置 3 个协议实现（均为完整实现，非 stub）：
//   - s7        S7-200 SMART / S7-1200 / S7-1500（基于 github.com/robinson/gos7）
//   - modbus_tcp ModbusTCP 客户端（基于 github.com/grid-x/modbus）
//   - opcua     OPC UA 客户端（基于 github.com/gopcua/opcua，含订阅模式）
//
// 扩展接口契约见 Plan.md §9.1。
package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/plcbridge/plcbridge/internal/platform"
)

// ProtocolPlugin 是采集协议的扩展点（Plan.md §9.1）。
//
// 一个 ProtocolPlugin 实例对应一种协议类型（如 S7），由 Name() 标识。
// 同一协议类型可创建多个 ProtocolChannel（每个 Channel 一个连接）。
type ProtocolPlugin interface {
	Name() string
	Version() string
	ConfigSchema() json.RawMessage

	// Capabilities 声明本协议的能力与必填字段（Plan2 主题 A 3.6）。
	//
	// 用途：
	//   - Web 端：按 RequiresRackSlot / RequiresSlaveID 等字段决定是否渲染对应输入框
	//   - handler：BatchWrite 失败时按 SupportsBatchWrite 决定是否降级为单点写
	//   - 文档生成：自动产出"协议能力矩阵"（无须再维护一份硬编码表）
	//
	// 实现要求：返回零值 Capability 即可（不强制各协议都支持 batch 等高级特性）。
	Capabilities() Capability

	// ParseConfig 解析 Channel.config JSON 字段为协议私有配置。
	ParseConfig(raw []byte) (any, error)

	// Init 初始化插件（一次性，如建立协议客户端单例）。
	Init(ctx context.Context) error
	Close() error

	// CreateChannel 为一个 Channel 配置创建一个连接通道。
	CreateChannel(cfg *platform.Channel) (ProtocolChannel, error)
}

// Capability 描述协议的能力与配置约束（Plan2 主题 A 3.6）。
//
// 命名约定：
//   - Supports*  → 运行时能力（collect/forward 可用）
//   - Requires*  → 配置必填项（Channel.config 必须包含）
//
// JSON tag 必须是 snake_case（2026-10-22 修正）：
//   - api/openapi.yaml 的 ProtocolInfo.capabilities 声明的是 supports_batch_read 等
//   - web/src/api/protocols.ts 的 ProtocolCapability 同样声明 snake_case
//   - 之前没写 json tag，Go 默认按字段名输出 "SupportsBatchRead"，
//     与前端 TS 接口对不上（读 supports_batch_read 永远是 undefined）。
type Capability struct {
	// SupportsBatchRead  是否支持批量读（多地址一次请求）。
	SupportsBatchRead bool `json:"supports_batch_read"`
	// SupportsBatchWrite 是否支持批量写。
	SupportsBatchWrite bool `json:"supports_batch_write"`
	// SupportsHeartbeat  是否支持协议层心跳（区别于应用层 HealthCheck）。
	SupportsHeartbeat bool `json:"supports_heartbeat"`
	// RequiresRackSlot   S7 协议：Channel.config 必须含 rack / slot 字段。
	RequiresRackSlot bool `json:"requires_rack_slot"`
	// RequiresSlaveID    Modbus 协议：Channel.config 必须含 slave_id 字段。
	RequiresSlaveID bool `json:"requires_slave_id"`
}

// ProtocolChannel 是一个具体的连接通道（读 / 写）。
//
// ProtocolChannel 由 collectPool / writePool 借用，**不跨池共享**（§6.2.2 隔离）。
type ProtocolChannel interface {
	Read(ctx context.Context, address string, dataType string) (Value, error)
	BatchRead(ctx context.Context, points []*platform.Point) (map[string]Value, error)
	Write(ctx context.Context, address string, value any, dataType string) error
	BatchWrite(ctx context.Context, writes []WriteOp) error
	Connect(ctx context.Context) error
	// Reconnect 强制重建连接：关闭现有 handler 后从头 Connect。
	// 用于：registry 探测到 LastError 持续非空，怀疑底层 socket 已死。
	// 与 Connect 的区别：Connect 在 client != nil 时幂等返回；Reconnect 无脑替换。
	Reconnect(ctx context.Context) error
	Disconnect() error
	HealthCheck(ctx context.Context) error

	// === Runtime 状态（供 web 仪表盘 / 通道表展示） ===

	// IsConnected 返回本地视图：句柄非空即 true。**不能**用于判断远端真活。
	// 用于快速展示"是否已被 connect 过 / 还没被 disconnect"。
	IsConnected() bool

	// ProbeActive 主动发一个轻量请求探活（无副作用）。
	//   - 成功：lastActiveAt 会被更新（lastErr 清空），返回 nil
	//   - 失败：lastErr 会被设置，返回错误
	// 用于 "is the peer still reachable?"，比 IsConnected 准；调用方应节流。
	ProbeActive(ctx context.Context) error

	// LastActiveAt 返回最近一次成功操作（Read/Write/HealthCheck/ProbeActive）的本地时间；未使用过返回 time.Time{}。
	LastActiveAt() time.Time

	// LastError 返回最近一次失败的错误信息；空字符串表示无最近错误。
	// 调用方应自行清空（如周期性清空）以避免长期陈旧。
	LastError() string

	// LastErrorAt 返回 LastError 最近一次被设置为非空的时间戳。
	// 用于：registry 探活节流——避免 lastUsed 在读失败时也被 touch() 更新而无法区分。
	LastErrorAt() time.Time

	// Stats 返回协议特定运行时数据（用于 web 仪表盘诊断 / 通道详情页）。
	//
	// 推荐字段（key 建议 kebab-case）：
	//   - "connection_state": "Connected" / "Disconnected" / "Reconnecting"
	//   - "<protocol>_read_count" / "<protocol>_write_count" / "<protocol>_error_count"
	//   - "last_probe_ok_ms":  最近一次 ProbeActive 成功时间（unix ms）
	//   - 协议特定：如 s7 的 "rack"/"slot"、opcua 的 "session_id"、modbus 的 "unit_id"
	//
	// 实现要求：
	//   - 必须并发安全（registry.Snapshot 在 mu 外并发读多个 cli）
	//   - 返回值必须可 json.Marshal（不要返回 func / chan）
	//   - 失败/未初始化时应返回 "connection_state": "Disconnected" + error 信息
	Stats() map[string]any
}

// StringLengthWriter 是**可选**接口：让协议层在写字符串时拿到点位声明的长度。
//
// 为什么需要这个接口：
//   - S7 STRING 的内容区容量由点位配置（platform.Point.StringLength）决定，
//     协议层的 Write(addr, value, dataType) 签名里没有这个信息。
//   - 没有它，S7 无头布局（string_layout=0）写字符串时不知道该写多长，
//     只能猜 —— 结果是 PLC 侧读到残留旧数据或半截内容。
//
// 设计取舍（为什么不用改 Write 签名）：
//   - 改 ProtocolChannel.Write 签名会波及全部协议（S7/Modbus/OPCUA）、
//     全部调用点（write worker、batch compensation、heartbeat）与测试。
//   - 用可选接口：调用方用类型断言探测，不支持就退化到默认行为，
//     对其他协议零影响。
//
// 实现要求：
//   - WriteStringLen(ctx, address, value, dataType, length) 写字符串，
//     length 是内容区**字节数**上限（0 = 用协议默认值）。
//   - 只有 string 类型点位需要关心，其他 dataType 应返回不支持错误。
type StringLengthWriter interface {
	// WriteStringLen 写入字符串，length 为内容区字节数上限。
	//
	// length <= 0 表示"由协议自行决定"（S7 会用 header 里的 maxLen 或默认值）。
	WriteStringLen(ctx context.Context, address string, value any, dataType string, length int) error
}

// Value 是统一的点位值类型。
//
// 协议层将原始字节解析为 Value，业务层按 DataType 使用。
type Value struct {
	DataType string
	Bool     bool
	Int64    int64
	Uint64   uint64
	Float64  float64
	String   string
	Bytes    []byte // bytes / raw
	Quality  string // "good" / "bad"
}

// AsGoNative 把 Value 转换为对应的 Go 原生类型（Plan2 主题 C 5.1）。
//
// 用途：应用层（collect / forward worker）拿到 protocol.Value 后，**不再写 switch**，
//
//	直接 .AsGoNative() 喂给 target.PointValue.Value（接口 any）。
//
// 映射表（与 platform.AllDataTypes() 严格对齐）：
//
//	bool            → bool
//	int8..int64     → int64
//	uint8..uint64   → uint64
//	float32/float64 → float64
//	string          → string
//	bytes           → []byte
//	其它（未知）    → nil（让业务层做降级 / 丢日志）
//
// 关键不变量：Int64 / Uint64 / Float64 / String 字段是冗余存储（同一 Value 任一时刻
// 只对应一种 DataType），由协议层 ParseConfig 时保证；应用层只信 AsGoNative 的输出。
func (v Value) AsGoNative() any {
	switch v.DataType {
	case "bool":
		return v.Bool
	case "int8", "int16", "int32", "int64":
		return v.Int64
	case "uint8", "uint16", "uint32", "uint64":
		return v.Uint64
	case "float32", "float64":
		return v.Float64
	case "string":
		return v.String
	case "bytes":
		return v.Bytes
	default:
		return nil
	}
}

// WriteOp 是批量写的一行。
type WriteOp struct {
	Address  string
	Value    any
	DataType string
}

// === 全局注册表 ===

var (
	registry   = map[string]ProtocolPlugin{}
	registryMu sync.RWMutex
)

// Register 注册一个协议插件。
func Register(p ProtocolPlugin) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[p.Name()]; exists {
		return fmt.Errorf("协议 %s 已注册", p.Name())
	}
	registry[p.Name()] = p
	return nil
}

// MustRegister 注册失败 panic。
func MustRegister(p ProtocolPlugin) {
	if err := Register(p); err != nil {
		panic(err)
	}
}

// Get 按名称获取协议插件。
func Get(name string) (ProtocolPlugin, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("协议 %s 未注册", name)
	}
	return p, nil
}

// Unregister 注销协议插件（仅供测试用）。
func Unregister(name string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, name)
}

// ProtocolInfo 是单个协议插件对外暴露的元信息（Plan2 主题 A 3.3）。
//
// 用途：GET /api/v1/protocols 接口；Web 端启动时拉取用于下拉框 + 配置表单生成。
// 与 ProtocolPlugin 接口的关系：Name/Version/ConfigSchema/Capabilities 直接转发；role 字段
// 由调用方决定（collect / forward），protocol 包的插件都属 collect 角色。
type ProtocolInfo struct {
	Name         string          `json:"name"`
	Version      string          `json:"version"`
	Role         string          `json:"role"`          // 固定 "collect"
	ConfigSchema json.RawMessage `json:"config_schema"` // JSON Schema（驱动 Web UI 表单生成）
	Capabilities Capability      `json:"capabilities"`
}

// AllInfo 返回所有已注册协议插件的元信息（按 name 字典序）。
//
// 线程安全：读持有 registryMu.RLock；plugin 内部方法（Name/Version/ConfigSchema/Capabilities）
// 必须是无状态读，调用方不能假设并发安全。
func AllInfo() []ProtocolInfo {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]ProtocolInfo, 0, len(registry))
	for _, p := range registry {
		out = append(out, ProtocolInfo{
			Name:         p.Name(),
			Version:      p.Version(),
			Role:         "collect",
			ConfigSchema: p.ConfigSchema(),
			Capabilities: p.Capabilities(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// 注意：MVP 内置协议的注册已从 MustRegisterDefaults 改为各 plugin 文件末尾的 init() 自注册（Plan2 主题 A 3.2）。
// 加新协议：新建 plugin 文件 → 实现 ProtocolPlugin 接口 → 文件末尾加 init() 调 Register 即可。
