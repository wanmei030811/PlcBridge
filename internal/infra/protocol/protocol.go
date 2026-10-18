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

	// ParseConfig 解析 Channel.config JSON 字段为协议私有配置。
	ParseConfig(raw []byte) (any, error)

	// Init 初始化插件（一次性，如建立协议客户端单例）。
	Init(ctx context.Context) error
	Close() error

	// CreateChannel 为一个 Channel 配置创建一个连接通道。
	CreateChannel(cfg *platform.Channel) (ProtocolChannel, error)
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
	Quality  string // "good" / "bad"
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

// MustRegisterDefaults 注册 MVP 内置协议。
func MustRegisterDefaults() {
	MustRegister(newS7Plugin())
	MustRegister(newModbusTCPPlugin())
	MustRegister(newOPCUAPlugin())
}
