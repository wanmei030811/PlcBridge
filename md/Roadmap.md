# PlcBridge 内核-外层优化路线图

> **背景**：用户洞察——"采集/转发/转发"是**内核恒久**，S7/Modbus/MQTT 等协议是**外层可变**。
> **目标**：在保留内核稳定的前提下，让外层扩展零成本（加新协议不改动 handler/前端/服务）。
> **节奏**：4 阶段，~2.5 个月；当前 2026-10-12 开始。

---

## 0. 验收标准

完成全部 4 阶段后，用以下 3 个验收用例确认优化目标达成：

### 验收 1：加新协议零改动
```
新增一个 OPCUA 协议插件
   ↓
期望：handler/*.go、web/src/views/*、sqlstore.go 零修改
   ↓
实际：仅在 protocol/opcua/opcua.go 写 NewPlugin()，init() 自动注册
```

### 验收 2：内核测试覆盖
```
app/collect/workers.go 调度逻辑：100% 单测覆盖
protocol/plugin 契约测试：所有插件 100% 通过
   ↓
go test ./internal/... 通过率 100%
```

### 验收 3：故障可观测
```
制造一次采集失败（人为注入错误）
   ↓
期望：
  - 3 秒内日志出现结构化记录（含 trace_id + device_biz_id）
  - Prometheus 指标更新
  - 错误响应包含国际化 code
```

---

## Phase 1：基础设施（Week 1~2）

> **目标**：补齐 3 个**横切关注点**（cross-cutting concerns），为后续优化铺路。
> **依据**：Plan.md §10 日志规范、§8.4 监控指标、§6.4 错误处理。
> **完成度定义**：所有 `go test` 通过、日志可在 jq 检索、指标可在 /metrics 抓取、错误响应统一格式。

### 1.1 Logger 接入（slog）
**优先级**：⭐⭐⭐ | **预估**：1 天

**问题**：当前用 `fmt.Println` / `log.Println`，无级别、无结构化字段、无 trace_id。

**实施**：
```go
// internal/log/logger.go
package log

import (
    "log/slog"
    "os"
)

var Default *slog.Logger

func Init(level string) {
    var lvl slog.Level
    switch level {
    case "DEBUG": lvl = slog.LevelDebug
    case "WARN":  lvl = slog.LevelWarn
    case "ERROR": lvl = slog.LevelError
    default:      lvl = slog.LevelInfo
    }
    Default = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
        Level: lvl,
    }))
}
```

**event 字段对齐 Plan.md §10.1.2 枚举表**：
```go
// 业务事件强枚举
const (
    EventServiceStart     = "service_start"
    EventDeviceConnectOK  = "device_connect_ok"
    EventPollBatchEnd     = "poll_batch_end"
    EventPointReadFail    = "point_read_fail"
    EventConfigReload     = "config_reload"
    // ... 完整 17 个 event
)
```

**替换点**：
- `app/collect/workers.go`：`log.Println(...)` → `log.Default.Info("...", "event", "poll_batch_end", ...)`
- `protocol/s7/connect.go`：连接成功/失败
- `sqlstore.go`：查询错误
- `handlers_crud.go`：请求处理结果

**验收**：
- `cat logs/plcbridge.log | jq 'select(.event=="device_connect_fail")'` 返回正确记录
- 包含 `trace_id`、`device_biz_id`、`source` 三个字段

---

### 1.2 错误体系（APIError + i18n 准备）
**优先级**：⭐⭐⭐ | **预估**：1.5 天

**问题**：当前错误响应混乱，英文/中文混杂，无法国际化。

**实施**：
```go
// web/errors.go
package web

type APIError struct {
    Code       string // "channel_not_found"
    Message    string // 默认文案
    HTTPStatus int    // 404
}

// 业务错误码枚举
const (
    ErrCodeChannelNotFound   = "channel_not_found"
    ErrCodeChannelRoleMismatch = "channel_role_mismatch"
    ErrCodeSourceNotFound    = "source_not_found"
    ErrCodePointNotFound     = "point_not_found"
    ErrCodeDeviceDisabled    = "device_disabled"
    ErrCodeInvalidParam      = "invalid_param"
    ErrCodeInternal          = "internal_error"
)

// 预定义错误实例
var (
    ErrChannelNotFound = &APIError{
        Code: ErrCodeChannelNotFound, Message: "channel 不存在", HTTPStatus: 404,
    }
    ErrChannelRoleMismatch = &APIError{
        Code: ErrCodeChannelRoleMismatch, Message: "channel role 不匹配", HTTPStatus: 400,
    }
    // ...
)

// 统一响应
func WriteError(c *gin.Context, err error) {
    var apiErr *APIError
    if errors.As(err, &apiErr) {
        c.JSON(apiErr.HTTPStatus, gin.H{
            "code": apiErr.Code,
            "msg":  apiErr.Message,
        })
        return
    }
    c.JSON(500, gin.H{
        "code": "internal_error",
        "msg":  "服务器内部错误",
    })
}
```

**改造 handler**：
```go
// Before:
if !exists {
    c.JSON(404, gin.H{"error": "channel not found"})
    return
}

// After:
if !exists {
    web.WriteError(c, web.ErrChannelNotFound)
    return
}
```

**国际化预留**（不做完整 i18n，但留接口）：
```go
// 将来要做 i18n 时，只需替换 Message 字段
type APIError struct {
    Code       string
    Message    string  // 临时默认文案
    HTTPStatus int
    // i18nKey string  // 预留
}
```

**验收**：
- 所有 handler 错误响应统一为 `{code, msg}` 格式
- 错误码在 web/src/api/errors.ts 集中定义（前端可对照处理）

---

### 1.3 核心测试覆盖
**优先级**：⭐⭐⭐ | **预估**：3 天

**目标**：核心调度逻辑 100% 单测覆盖。

#### 1.3.1 protocol/contract_test.go
**目的**：所有协议插件必须通过的契约测试，**新协议实现的验收门槛**。

```go
// protocol/contract_test.go
func TestProtocolContract(t *testing.T) {
    plugins := []ProtocolPlugin{
        s7.NewPlugin(),
        modbus.NewPlugin(),
        opcua.NewPlugin(),
    }
    for _, p := range plugins {
        t.Run(p.Name(), func(t *testing.T) {
            // 1. 基础元信息
            assert.NotEmpty(t, p.Name())
            assert.NotEmpty(t, p.Version())
            // 2. ConfigSchema 是合法 JSON
            var schema map[string]any
            err := json.Unmarshal(p.ConfigSchema(), &schema)
            assert.NoError(t, err)
            // 3. ParseConfig 能解析自己的 schema
            _, err = p.ParseConfig(p.ConfigSchema())
            assert.NoError(t, err)
            // 4. Init 能初始化（无真实连接）
            err = p.Init(context.Background(), minimalConfig)
            assert.NoError(t, err)
            defer p.Close()
            // 5. CreateChannel 返回非 nil
            ch, err := p.CreateChannel(&platform.Channel{...})
            assert.NoError(t, err)
            assert.NotNil(t, ch)
        })
    }
}
```

#### 1.3.2 app/collect/workers_test.go
**目的**：采集调度逻辑——**业务内核的核心**。

```go
func TestWorker_CollectTick(t *testing.T) {
    // Mock 协议插件：返回预设值
    mockPlugin := &MockPlugin{
        BatchReadFunc: func(ctx context.Context, points []*platform.Point) (map[string]protocol.Value, error) {
            return map[string]protocol.Value{
                "p_001": {DataType: "int16", Int64: 100},
                "p_002": {DataType: "int16", Int64: 200},
            }, nil
        },
    }
    
    // 验证：
    // 1. 1 个 tick 后，2 个点位都进入目标队列
    // 2. 周期时间准确
    // 3. 超时后正确处理
    // 4. 错误时不阻塞下一周期
}
```

#### 1.3.3 sqlstore_test.go
**目的**：CRUD + 级联删除的回归测试。

```go
func TestDeleteSource_CascadePoints(t *testing.T) {
    // 1. 创建 source + 3 points
    // 2. 删除 source
    // 3. 验证 points 全部删除（FK CASCADE）
}

func TestDeleteDevice_RestrictIfHasChildren(t *testing.T) {
    // 1. 创建 device + source
    // 2. 尝试删除 device
    // 3. 验证返回错误（FK RESTRICT）
}
```

**验收**：
```bash
go test ./internal/protocol/... -v      # 契约测试
go test ./internal/app/collect/... -v   # 调度测试
go test ./internal/store/... -v          # 持久化测试
# 全部通过
```

---

### Phase 1 交付清单
- [ ] `internal/log/logger.go` 接入
- [ ] `web/errors.go` 错误体系
- [ ] 17 个 event 枚举
- [ ] 所有 `log.Println` 替换为结构化日志
- [ ] 所有 handler 错误响应统一格式
- [ ] `protocol/contract_test.go`
- [ ] `app/collect/workers_test.go`
- [ ] `sqlstore_test.go`
- [ ] `go test ./...` 100% 通过

---

## Phase 2：架构验收（Week 3~4）

> **目标**：**加新协议时，handler / 前端 / service 零修改**——这是「内核-外层」架构的**验收门槛**。
> **当前问题**：协议名出现在 3 个地方（protocol 注册、handler 校验白名单、Vue 组件常量），是典型的"重复事实源"。
> **完成度定义**：fake 协议开发验证。

### 2.1 协议/Target 类型集中
**优先级**：⭐⭐⭐ | **预估**：1 天

**问题**：
```go
// handlers_crud.go (现状)
func isKnownChannelType(role, typ string) bool {
    switch role {
    case "collect":
        switch typ {
        case "s7", "opcua", "modbus":  // ← 硬编码
            return true
        }
    }
    // ...
}
```

**改造**：
```go
// protocol/registry.go
type Registry struct {
    mu              sync.RWMutex
    collectPlugins  map[string]ProtocolPlugin  // "s7" → plugin
    forwardPlugins  map[string]TargetPlugin
}

var Default = NewRegistry()

func (r *Registry) RegisterCollectPlugin(name string, p ProtocolPlugin) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.collectPlugins[name] = p
}

func (r *Registry) CollectTypes() []string {
    r.mu.RLock()
    defer r.mu.RUnlock()
    types := make([]string, 0, len(r.collectPlugins))
    for k := range r.collectPlugins {
        types = append(types, k)
    }
    sort.Strings(types)
    return types
}

func (r *Registry) GetCollectPlugin(name string) (ProtocolPlugin, bool) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    p, ok := r.collectPlugins[name]
    return p, ok
}
```

**handler 改造**：
```go
// Before
if !isKnownChannelType(role, typ) { ... }

// After
if _, ok := protocol.Default.GetCollectPlugin(typ); !ok && role == "collect" {
    web.WriteError(c, web.ErrInvalidChannelType)
}
```

---

### 2.2 自动注册机制（init()）
**优先级**：⭐⭐⭐ | **预估**：0.5 天

**问题**：每次加协议要改 main.go 的注册表。

**改造**：
```go
// protocol/s7/init.go
package s7

import "plcbridge/internal/protocol"

func init() {
    protocol.Default.RegisterCollectPlugin("s7", NewPlugin())
}

func NewPlugin() ProtocolPlugin { ... }
```

```go
// protocol/modbus/init.go
package modbus

import _ "plcbridge/internal/protocol/s7"      // 触发 s7 init
import _ "plcbridge/internal/protocol/modbus"   // 触发 modbus init
import _ "plcbridge/internal/protocol/opcua"    // 触发 opcua init
// 未来加 BACnet：import _ "plcbridge/internal/protocol/bacnet"
```

**main.go 简化**：
```go
// Before
app.RegisterProtocol("s7", s7.NewPlugin())
app.RegisterProtocol("modbus", modbus.NewPlugin())
app.RegisterProtocol("opcua", opcua.NewPlugin())
// ...

// After
import _ "plcbridge/internal/protocol/_all"  // 一行
```

---

### 2.3 元信息接口：协议上报能力
**优先级**：⭐⭐⭐ | **预估**：0.5 天

**问题**：Web 端 `List.vue` 硬编码协议列表。

**改造**：
```go
// protocol/registry.go
type ProtocolInfo struct {
    Name        string          `json:"name"`
    Version     string          `json:"version"`
    Role        string          `json:"role"`         // "collect" | "forward"
    ConfigSchema json.RawMessage `json:"config_schema"` // JSON Schema
}

// HTTP 接口
// GET /api/v1/protocols → 返回所有已注册协议的元信息
func listProtocolsHandler(c *gin.Context) {
    infos := protocol.Default.AllInfo()
    c.JSON(200, gin.H{"protocols": infos})
}
```

**Vue 改造**：
```typescript
// Before
const TYPES_COLLECT = ['s7', 'modbus', 'opcua']  // 硬编码

// After
const { data: protocols } = await api.get('/api/v1/protocols')
const collectTypes = protocols
    .filter(p => p.role === 'collect')
    .map(p => p.name)
```

---

### 2.4 验收：Fake 协议开发
**优先级**：⭐⭐⭐ | **预估**：1 天

**目的**：用 fake 协议验证"零改动"承诺。

**步骤**：
1. 新建 `protocol/fake/init.go`，实现 `ProtocolPlugin` 接口（Mock 行为）
2. 在 `protocol/_all.go` 加 `import _ ".../fake"`
3. 启动服务，验证：
   - `/api/v1/protocols` 返回 fake
   - Web 端协议下拉框自动出现 "fake"
   - 添加一个 fake 协议的 Channel，零代码修改

**如果发现还需要改代码**：
- 改的地方就是 Phase 2 没做完的
- 继续完善直到 fake 协议真正零改动

---

### Phase 2 交付清单
- [ ] `protocol/registry.go` 注册表
- [ ] `protocol/s7/init.go` 等每个插件的 init
- [ ] `protocol/_all.go` 一行式导入
- [ ] `GET /api/v1/protocols` 接口
- [ ] handler 协议白名单改为查表
- [ ] Vue `List.vue` 改为启动时拉取
- [ ] **fake 协议开发验证通过**

---

## Phase 3：边界清理（Week 5~6）

> **目标**：把"业务规则"和"持久化细节"从外层（HTTP/SQL）抽离到内核（service/domain）。
> **完成度定义**：HTTP handler 纯调用、SQL store 纯 CRUD、业务规则在 service 层。

### 3.1 Value.AsGoNative()
**优先级**：⭐⭐ | **预估**：0.5 天

```go
// protocol/value.go
func (v Value) AsGoNative() any {
    switch v.DataType {
    case "bool":     return v.Bool
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
```

```go
// app/collect/workers.go (改造后)
queue.Enqueue(target.PointValue{
    Value: values[pointBizID].AsGoNative(),  // 应用层只调方法
})
```

---

### 3.2 事务抽象
**优先级**：⭐⭐ | **预估**：0.5 天

```go
// store/transaction.go
type Store interface {
    WithTx(ctx context.Context, fn func(Tx) error) error
}

func (s *SQLStore) WithTx(ctx context.Context, fn func(Tx) error) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil { return err }
    defer func() { _ = tx.Rollback() }()
    if err := fn(tx); err != nil { return err }
    return tx.Commit()
}
```

```go
// store/source.go
func (s *SQLStore) DeleteSource(ctx context.Context, id int64) error {
    return s.WithTx(ctx, func(tx Tx) error {
        if _, err := tx.Exec(`DELETE FROM points WHERE source_id=?`, id); err != nil {
            return err
        }
        res, err := tx.Exec(`DELETE FROM sources WHERE id=?`, id)
        if err != nil { return err }
        n, _ := res.RowsAffected()
        if n == 0 { return ErrNotFound }
        return nil
    })
}
```

---

### 3.3 校验下沉（service 层）
**优先级**：⭐⭐ | **预估**：1.5 天

```go
// service/validation.go
type ValidationResult struct {
    Code    string
    Message string
}

func (r ValidationResult) Error() string { return r.Message }

func ValidateSourceForCreate(src *platform.Source, ch *platform.Channel) error {
    if ch == nil {
        return ValidationResult{Code: "channel_not_found", Message: "channel 不存在"}
    }
    if ch.Role != "collect" {
        return ValidationResult{Code: "channel_role_mismatch", Message: "source 只能引用 collect 通道"}
    }
    return nil
}

func ValidatePointForCreate(p *platform.Point, src *platform.Source) error {
    if src == nil {
        return ValidationResult{Code: "source_not_found", Message: "source 不存在"}
    }
    // data_type 白名单也可下沉
    if !isKnownDataType(p.DataType) {
        return ValidationResult{Code: "invalid_data_type", Message: "data_type 不支持"}
    }
    return nil
}
```

```go
// handlers_crud.go (改造后)
func (h *Handler) CreateSource(c *gin.Context) {
    var req web.SourceDTO
    if err := c.ShouldBindJSON(&req); err != nil {
        web.WriteError(c, web.ErrInvalidParam)
        return
    }
    
    src := &platform.Source{...}
    ch, _ := h.store.GetChannelByBizID(src.ChannelBizID)
    if err := service.ValidateSourceForCreate(src, ch); err != nil {
        web.WriteError(c, web.WrapAPIError(err))  // ValidationResult → APIError
        return
    }
    
    if err := h.store.CreateSource(ctx, src); err != nil { ... }
}
```

---

### 3.4 实体/DTO 分离（可选）
**优先级**：⭐ | **预估**：2 天

**只在以下情况做**：
- 需要给另一个端（如 CLI、gRPC）暴露 API
- 或当前 DTO 与实体混杂已造成维护问题

**否则跳过**，等真有需求再做。

---

### Phase 3 交付清单
- [ ] `protocol/value.go` AsGoNative()
- [ ] `store/transaction.go` WithTx
- [ ] `service/validation.go` 校验函数
- [ ] handler 改为纯调用 + 错误翻译
- [ ] 既有测试仍通过
- [ ] 新增 service 层测试

---

## Phase 4：健壮性（Week 7~8）

> **目标**：配置错误早失败、运行时可观测、HTTP 有保护。
> **完成度定义**：配置错误启动时立即报错；Prometheus 指标齐全；恶意请求不会拖垮服务。

### 4.1 Fail-Fast 配置校验
**优先级**：⭐⭐ | **预估**：1.5 天

**问题**：当前 `Channel.Config` 是 `json.RawMessage`，**直到采集时才报错**。

**改造**：
```go
// app/startup.go
func (a *App) validateAllChannelConfigs() error {
    var errs []string
    for _, ch := range a.config.Channels {
        plugin, ok := protocol.Default.GetCollectPlugin(ch.Type)
        if !ok {
            plugin, ok = protocol.Default.GetForwardPlugin(ch.Type)
            if !ok {
                errs = append(errs, fmt.Sprintf("channel %s: unknown type %q", ch.BizID, ch.Type))
                continue
            }
        }
        if _, err := plugin.ParseConfig(ch.Config); err != nil {
            errs = append(errs, fmt.Sprintf("channel %s: config invalid: %v", ch.BizID, err))
        }
    }
    if len(errs) > 0 {
        return fmt.Errorf("config validation failed:\n  - %s", strings.Join(errs, "\n  - "))
    }
    return nil
}

func (a *App) Start() error {
    if err := a.validateAllChannelConfigs(); err != nil {
        return err  // 启动失败，立即退出
    }
    // ...
}
```

**验收**：
- 故意写错 config（缺字段、类型错）→ 启动立即报错，错误信息含 channel_biz_id
- 不再等到采集时才发现

---

### 4.2 Prometheus 指标
**优先级**：⭐ | **预估**：1.5 天

**对齐 Plan.md §8.4 监控指标**：
```go
// metrics/metrics.go
var (
    CollectDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "plcbridge_collect_duration_seconds",
            Buckets: []float64{0.01, 0.05, 0.1, 0.3, 0.5, 1.0},
        },
        []string{"device_biz_id"},
    )
    CollectTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "plcbridge_collect_total"},
        []string{"device_biz_id", "result"},
    )
    TargetQueueLength = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "plcbridge_target_queue_length"},
        []string{"target_biz_id"},
    )
    TargetDropped = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "plcbridge_target_dropped_total"},
        []string{"target_biz_id", "reason"},
    )
    WriteTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "plcbridge_write_total"},
        []string{"result"},
    )
    WriteDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "plcbridge_write_duration_seconds",
            Buckets: []float64{0.05, 0.1, 0.3, 0.5, 1.0},
        },
        []string{"device_biz_id"},
    )
)

func init() {
    prometheus.MustRegister(
        CollectDuration, CollectTotal, TargetQueueLength, TargetDropped,
        WriteTotal, WriteDuration,
    )
}
```

**埋点位置**：
- `app/collect/workers.go`：`CollectDuration.Observe(...)` + `CollectTotal.Inc(...)`
- `app/forward/dispatcher.go`：`TargetQueueLength.Set(...)` + `TargetDropped.Inc(...)`
- `app/write/pool.go`：`WriteDuration.Observe(...)` + `WriteTotal.Inc(...)`

---

### 4.3 HTTP 中间件
**优先级**：⭐ | **预估**：1 天

```go
// web/middleware.go
func TimeoutMiddleware(timeout time.Duration) gin.HandlerFunc {
    return func(c *gin.Context) {
        ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
        defer cancel()
        c.Request = c.Request.WithContext(ctx)
        c.Next()
    }
}

func RateLimitMiddleware(rps int) gin.HandlerFunc {
    limiter := rate.NewLimiter(rate.Limit(rps), rps*2)
    return func(c *gin.Context) {
        if !limiter.Allow() {
            c.JSON(429, gin.H{"code": "rate_limited", "msg": "请求过于频繁"})
            c.Abort()
            return
        }
        c.Next()
    }
}

func RecoveryMiddleware(logger *slog.Logger) gin.HandlerFunc {
    return func(c *gin.Context) {
        defer func() {
            if r := recover(); r != nil {
                logger.Error("panic recovered",
                    "event", "http_panic",
                    "path", c.Request.URL.Path,
                    "panic", r,
                )
                c.JSON(500, gin.H{"code": "internal_error", "msg": "服务器内部错误"})
            }
        }()
        c.Next()
    }
}
```

---

### Phase 4 交付清单
- [ ] `app/startup.go` validateAllChannelConfigs
- [ ] `metrics/metrics.go` 6 个核心指标
- [ ] 关键路径埋点
- [ ] `web/middleware.go` 超时/限流/恢复
- [ ] 启动校验测试

---

## 时间线汇总

```
Week 1~2   [Phase 1: 基础设施]
   Day 1-2    Logger 接入 + 17 个 event 枚举
   Day 3-4    错误体系 + handler 改造
   Day 5-7    核心测试覆盖
   Day 8-10   Phase 1 收尾 + 文档

Week 3~4   [Phase 2: 架构验收]
   Day 11-12  protocol/registry + 协议类型集中
   Day 13     init() 自动注册
   Day 14     GET /api/v1/protocols 元信息接口
   Day 15     Vue List.vue 改造
   Day 16-17  fake 协议验证 + 收尾

Week 5~6   [Phase 3: 边界清理]
   Day 18     Value.AsGoNative()
   Day 19     WithTx 事务抽象
   Day 20-22  校验下沉到 service
   Day 23-24  Phase 3 收尾

Week 7~8   [Phase 4: 健壮性]
   Day 25-26  Fail-Fast 配置校验
   Day 27-28  Prometheus 指标
   Day 29     HTTP 中间件
   Day 30     整体验收 + 文档更新
```

---

## 风险与回滚

| 风险 | 影响 | 缓解 |
|------|------|------|
| slog 性能不如 zap | 日志延迟 | 第一阶段对比 benchmark，> 5% 则换 zap |
| 自动注册隐藏 main.go 启动流程 | 调试困难 | 启动时打印注册表日志 |
| Fail-Fast 太严格 | 旧配置无法启动 | 提供 `--skip-config-validation` 兜底 |
| Phase 1 改动面太大 | 影响其他开发 | 单独分支，feature flag 灰度 |

---

## 关联文档

- **Plan.md**：产品方案、性能指标、数据模型
- **Roadmap.md**（本文）：优化路线图
- **Architecture.md**（待写）：架构图、依赖关系

---

**最后更新**：2026-10-12
**负责**：plcbridge 后端
**当前阶段**：Phase 1 待启动
