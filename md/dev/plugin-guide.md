# plcbridge 协议插件开发指南（Plan2 主题 F 8.1）

本文档面向"加新协议 / Target"的开发者。读完可独立完成 `internal/infra/protocol/<your_proto>/` 子包的开发、测试、提交。

> 配套资源：
> - [Plan2 主题 A §3](../md/Plan2.md) 加新协议的总设计背景
> - [`internal/infra/protocol/fake/`](../internal/infra/protocol/fake) 最小可运行示例（30 行 init 协议）
> - [8.3 公开 API 文档](./openapi.md) 配套 REST 端点契约

## 1. 零改动契约

加新协议 = **新建 1 个子包**（不修改其他任何文件）：

```
internal/infra/protocol/<your_proto>/
  ├─ plugin.go        # 实现 ProtocolPlugin 接口
  └─ plugin_test.go   # 单元测试
```

main.go / handlers_crud.go / 前端 / 内核 dispatch 全部不需要改。

## 2. ProtocolPlugin 接口（必读）

接口定义在 [`internal/infra/protocol/protocol.go`](../internal/infra/protocol/protocol.go)：

```go
type ProtocolPlugin interface {
    Name() string                                          // 协议唯一标识
    Version() string                                       // 语义化版本
    ConfigSchema() json.RawMessage                         // JSON Schema（驱动 Web 表单）
    Capabilities() protocol.Capability                     // 协议能力声明（Plan2 3.6）
    ParseConfig(raw []byte) (any, error)                   // 解析 Channel.config 字段
    Init(ctx context.Context) error                        // 启动时一次性初始化
    Close() error                                          // 关闭时清理
    CreateChannel(cfg *platform.Channel) (ProtocolChannel, error) // 创建连接实例
}
```

### 2.1 各方法的实现约定

| 方法 | 实现约定 |
|---|---|
| `Name()` | 小写 snake_case，全局唯一。建议 `<protocol>` 或 `<protocol>_<variant>`（如 `modbus_tcp`）。 |
| `Version()` | 语义化版本 `MAJOR.MINOR.PATCH`。改协议私有协议格式要 bump MAJOR。 |
| `ConfigSchema()` | JSON Schema 顶层 `type: object`，含 `properties` 字段。Web 前端用它生成表单（v0.1 暂未自动渲染，但已就位）。 |
| `Capabilities()` | 返回 `protocol.Capability{}` 结构：标记 `SupportsBatchRead` / `RequiresRackSlot` 等。前端按它决定是否显示 rack/slot 等必填项。 |
| `ParseConfig(raw)` | 把 Channel.config JSON 字节流解析为协议私有结构体。空字节流要返回合理默认值，不要 panic。 |
| `Init()` | 启动时调用一次。**只做一次性的全局初始化**（如连接池、客户端工厂单例）。不要在这里建 per-Channel 的资源。 |
| `Close()` | 关停时清理全局资源。 |
| `CreateChannel(cfg)` | 给一个 `platform.Channel` 配置创建一个连接实例。**每个 Channel 一个新实例**（不同 channel 间不共享连接）。 |

## 3. ProtocolChannel 接口（连接实例）

`CreateChannel` 返回的连接实例需实现 [`ProtocolChannel`](../internal/infra/protocol/protocol.go)：

```go
type ProtocolChannel interface {
    // === 数据 I/O ===
    Read(ctx, address, dataType) (Value, error)
    BatchRead(ctx, points) (map[string]Value, error)
    Write(ctx, address, value, dataType) error
    BatchWrite(ctx, writes) error

    // === 生命周期 ===
    Connect(ctx) error
    Reconnect(ctx) error  // 强制重建（registry 探活失败时调用）
    Disconnect() error
    HealthCheck(ctx) error

    // === 运行时状态（供仪表盘） ===
    IsConnected() bool
    ProbeActive(ctx) error
    LastActiveAt() time.Time
    LastError() string
    LastErrorAt() time.Time
    Stats() map[string]any  // JSON-marshalable
}
```

### 3.1 关键不变量

- **`IsConnected()` ≠ 远端真活**：只表示"句柄非空"。判断"peer 还在不在"用 `ProbeActive(ctx)`（发一个轻量无副作用请求）。
- **`Read` / `Write` 失败时必须更新 `LastError` / `LastErrorAt`**，让 ChannelRegistry 知道要触发探活。
- **`Stats()` 必须并发安全**（web 端多线程读）。返回 `map[string]any`，值必须能 json.Marshal。
- **`BatchRead` 要尽量优化**：同 fc + 连续地址的点位合并成 1 次请求（参考 modbus_tcp.go 的实现）。

## 4. 自注册（关键）

在 plugin 文件末尾加 `init()` 调 `protocol.Register`：

```go
package yourproto

import "github.com/plcbridge/plcbridge/internal/infra/protocol"

type plugin struct{}

func newPlugin() protocol.ProtocolPlugin { return &plugin{} }

func (p *plugin) Name() string    { return "yourproto" }
func (p *plugin) Version() string { return "0.1.0" }
// ... 其他方法

func init() {
    if err := protocol.Register(newPlugin()); err != nil {
        panic(err) // 启动时必须暴露注册冲突
    }
}
```

### 4.1 谁 import 这个子包？

| 进程 | 是否 import |
|---|---|
| `cmd/plcbridge` (主进程) | ✅ 必须 import 才能用上 |
| `cmd/plcbridge-web` (Web 后台) | ✅ 必须 import（Web 后台有 handler 校验） |
| `cmd/bench` (基准测试) | ✅ 推荐 import |
| 测试代码 | 用 `import _ "..."` 触发 init 即可 |

如果忘记 import，协议不会出现在 `GET /api/v1/protocols` 中——这是最常见的"我加了协议但前端看不到"bug。

## 5. 完整示例：照抄 fake 协议

最小可工作示例在 [`internal/infra/protocol/fake/fake.go`](../internal/infra/protocol/fake)：

- 30 行 plugin 实现
- 纯内存，不发网络包
- 适合做端到端测试

把它拷贝改名 → 改 Name / ParseConfig / CreateChannel → 5 分钟出一个新协议 stub。

## 6. 测试清单

新增协议前**必须**跑过：

- [ ] `go test ./internal/infra/protocol/yourproto/...` 通过
- [ ] `go test ./internal/infra/protocol/...` 通过（allinfo_test 等）
- [ ] `go test ./internal/infra/api/...` 通过（HTTP 端到端：GET /api/v1/protocols 含新协议）
- [ ] 手动：起 plcbridge 进程，浏览器访问 http://localhost:8082/api/v1/protocols，看到你的协议
- [ ] 手动：Web 端 channels 列表页下拉框出现你的协议类型
- [ ] 手动：添加一个 channel 选你的协议 + 设备 + source + point，**实际数据按 Mock 返回**（或真实协议读 PLC）

## 7. 常见陷阱

| 症状 | 原因 | 修复 |
|---|---|---|
| 协议不出现在 `/api/v1/protocols` | 没人 import 这个子包 | main.go 加 `_ "internal/infra/protocol/yourproto"` |
| 前端下拉框没新协议 | 同上 | 同上 |
| `Register` panic "协议 X 已注册" | Name() 重复 | 改 Name()，或检查是否有旧的 _all.go 隐式 import |
| 创建 channel 报 "type 与 role 不匹配" | role 选错（protocol 是 collect, target 是 forward） | 创建 collect 角色 channel |
| `CreateChannel(role=forward)` 成功 | 你的 protocol 被 forward 误用了 | ProtocolChannel 应在 role != "collect" 时返回 error |

## 8. 升级路径

协议升级时（bump Version）需要：

1. 改 `Version()` 字符串
2. 若 ConfigSchema 字段有变化（不兼容），bump MAJOR，并在 `md/Plan2.md` 加 changelog
3. 若仅新增可选字段（兼容），bump MINOR
4. 数据库不需要迁移——`channels.config_json` 是 TEXT，自动兼容

## 9. 关联文档

- [Plan2 §3 协议 / Target 插件化](../md/Plan2.md)
- [Plan2 §3.6 协议能力声明（Capabilities）](../md/Plan2.md)
- [openapi.md](./openapi.md) 公开 REST API
- [architecture.md](./architecture.md) 整体架构
