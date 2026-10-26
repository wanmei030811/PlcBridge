# PlcBridge 功能扩展计划（Plan3）

> **承接文档**:`md/Plan2.md`(演进方案,已交付)、`md/Plan.md`(MVP,已交付)、`md/MVP_CHECKLIST.md`(MVP 验收,全 ✅)。
>
> **目标**:在 MVP + Plan2 基础上,追加用户可感知的高价值功能,同时保持架构稳定。
>
> **核心原则**:
> 1. 新功能不破坏 Plan2 已建立的不变式(4 worker 池隔离、插件化注册表、错误体系统一、`{code,msg}` 响应格式)
> 2. 新功能优先复用现有基础设施(复用 Store/service 层、复用 `protocol.AllInfo()`、复用 `platform` 工具函数)
> 3. 每个功能独立测试、独立验收;不做"大一揽子"发布

---

## 0. Plan3 验收门槛

完成 Plan3 全部条目后,用以下用例验证整体目标:

### 验收 A:运维效率提升
```
运维人员:
  1. 从 Excel 批量导入 500 个点位 → 1 分钟内完成
  2. 将现有 Channel/Source/Point 配置导出为 CSV → 10 秒内完成
  3. 在设备详情页直接看到所有点位的实时值 → 无需跳转到其他页面
  4. Target 连接断开时 → 设备详情页直接看到红色告警标记 → 无需查 Prometheus
```

### 验收 B:可观测性深化
```
运维人员:
  1. 点击任意 Target → 看到连接状态、sent/error 计数实时刷新
  2. 在 InfluxDB 配置页面 → 直接预览历史趋势图
  3. 打开 Grafana → 仪表盘自动导入 → 立刻看到采集 P99 / 队列 / 池利用率
```

### 验收 C:数据安全与合规
```
管理员:
  1. 每次修改配置 → 系统记录变更前后的 diff → 可随时审计
  2. 任意时间点配置 → 一键回滚 → plcbridge 立即热加载新配置
  3. 多角色账号 → admin/operator/viewer 各司其职 → 防止误操作
```

---

## 1. 现状盘点(Plan2 后的真实代码状态)

### 1.1 Plan2 已交付(无需 Plan3 重复)

| 来源 | 状态 | 证据 |
|---|---|---|
| 协议/Targe 插件化 | ✅ 完整 | `_all.go` 隐式注册、`protocol.Get`/`target.Get` 查表、`GET /api/v1/protocols` |
| Vue 协议下拉动态化 | ✅ 完整 | 前端 `computed` 走 `/api/v1/protocols` |
| handler 错误体系统一 | ✅ 完整 | 全部走 `WriteError`,响应 `{code, msg}` |
| `Value.AsGoNative()` | ✅ 完整 | 13 个 data_type 覆盖 |
| `Store.WithTx` 事务 | ✅ 完整 | 4 个事务方法 |
| 校验下沉 service 层 | ✅ 完整 | `service.go` 6 个校验方法 |
| SLO 告警规则 | ✅ 完整 | `deploy/prometheus/rules.yaml` + `md/SLO.md` |
| 协议特定指标 | ✅ 完整 | S7/OPC UA/Modbus/MQTT 专项指标 |
| 仪表盘 JSON | ✅ 完整 | `deploy/grafana/dashboard-overview.json` |
| 插件开发指南 | ✅ 完整 | `md/dev/plugin-guide.md` |
| 架构图 | ✅ 完整 | `md/architecture.md` |
| OpenAPI 文档 | ✅ 完整 | `api/openapi.yaml` |
| i18n 预留 | ✅ 完整 | `APIError.I18nKey` 字段 |
| 启动 fail-fast 校验 | ✅ 完整 | `failfast_test.go` 3 个 case |
| Dark Mode 预留 | ⏭️ 跳过 | Element Plus 支持但无需求触发 |

### 1.2 Plan3 待实现的功能缺口

| 缺口 | 位置 | 影响 | 优先级 |
|---|---|---|---|
| **CSV 导入/导出 Points** | 缺 handler + 前端 UI | 批量配置效率低 | ⭐⭐⭐ |
| **设备详情页实时数据预览** | 缺 runtime 状态聚合 | 查看点位状态需跳转 | ⭐⭐⭐ |
| **配置变更历史与回滚** | 缺 audit_log 表 + 回滚 API | 配置变更无法审计/回滚 | ⭐⭐⭐ |
| **配置对比(Diff)** | 缺 diff 逻辑 | 变更审查困难 | ⭐⭐ |
| **Target 诊断面板** | 缺 Target runtime 状态 API | Target 状态不可观测 | ⭐⭐ |
| **InfluxDB 历史趋势图** | 缺 InfluxDB Query API 调用 | 无法预览历史数据 | ⭐⭐ |
| **Dark Mode** | 缺 CSS 变量切换 | 无 | ⭐ |
| **设备克隆** | 缺克隆 handler | 批量创建同类型设备效率低 | ⭐ |
| **键盘快捷键** | 缺 `useMagicKeys` | 无 | ⭐ |
| **Swagger UI** | 缺 swagger 静态资源 | 无 | ⭐ |
| **规则告警** | 缺告警规则 CRUD + Prometheus AlertManager | 无 | ⏭️ 未来 |
| **多账号/RBAC** | 缺用户表 + 角色系统 | 无 | ⏭️ 未来 |
| **插件化协议扩展** | 缺 plugin 注册表单 + 下载 | 无 | ⏭️ 未来 |

---

## 2. 总体目标与原则

### 2.1 一句话目标

> **在 MVP + Plan2 基础上,追加"运维效率 + 可观测性 + 配置安全"三类功能,让运维人员无需跳转多个工具即可完成日常操作。**

### 2.2 设计原则

1. **复用不重复**:所有新功能优先复用现有 Store/service 层、`protocol.AllInfo()`、`platform` 工具函数,不在新功能里重复业务逻辑。
2. **渐进增强**:CSV 导入/导出、实时预览等属于"渐进增强"——现有 HTTP API 完全不变,前端按需追加。
3. **可独立验收**:每个功能独立开发、独立测试、独立验收;不做"大一揽子"发布。
4. **不破坏 Plan2 不变式**:新功能不允许破坏 4 worker 池隔离、插件化注册表、错误体系统一。

---

## 3. 功能详述

### 3.1 CSV 导入/导出 Points

**优先级**:⭐⭐⭐ | **预估**:2 天 | **依赖**:Plan2 全部

**问题**:当前配置点位只能一个一个手动添加,100+ 点位场景下效率极低。

**改造**:

#### 后端

**新增 SQLite 表**:
```sql
CREATE TABLE config_snapshots (
    id              BIGINT UNSIGNED PRIMARY KEY AUTO_INCREMENT,
    snapshot_biz_id VARCHAR(64) NOT NULL,
    description     VARCHAR(256),
    config_json     MEDIUMTEXT  NOT NULL,
    created_at      DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by      VARCHAR(64),
    UNIQUE KEY uk_snapshot_biz_id (snapshot_biz_id),
    KEY idx_created_at (created_at)
);

CREATE TABLE config_diff_log (
    id              BIGINT UNSIGNED PRIMARY KEY AUTO_INCREMENT,
    snapshot_id     BIGINT UNSIGNED NOT NULL,
    entity_type     VARCHAR(32) NOT NULL,  -- channel/device/source/point/target
    entity_biz_id   VARCHAR(64) NOT NULL,
    action          VARCHAR(16) NOT NULL,  -- create/update/delete
    before_json     MEDIUMTEXT,
    after_json      MEDIUMTEXT,
    changed_by      VARCHAR(64),
    changed_at      DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (snapshot_id) REFERENCES config_snapshots(id) ON DELETE CASCADE
);
```

**新增 Handler**:

```go
// internal/interface/web/handlers_crud.go

// POST /api/v1/points/import
// 请求体: multipart/form-data, field="file"(CSV) 或 field="text"(粘贴内容)
// CSV 格式: point_biz_id,name,address,data_type,collect_enabled,write_enabled
// 响应: {imported: N, failed: M, errors: [{row, field, message}]}

type ImportResult struct {
    Imported int           `json:"imported"`
    Failed   int           `json:"failed"`
    Errors   []ImportError `json:"errors,omitempty"`
}
type ImportError struct {
    Row     int    `json:"row"`
    Field   string `json:"field"`
    Message string `json:"message"`
}

// GET /api/v1/points/export
// Query: ?channel_biz_id=xxx 或 ?device_biz_id=xxx 或 ?source_biz_id=xxx
// 响应: CSV 文件下载或 JSON {points: [...]}

// GET /api/v1/config/snapshots        → 列出所有快照
// GET /api/v1/config/snapshots/:id    → 获取指定快照详情
// POST /api/v1/config/snapshots       → 创建快照(含描述)
// POST /api/v1/config/snapshots/:id/rollback → 回滚到指定快照
// GET /api/v1/config/diff            → 对比两个快照: ?from=id1&to=id2
```

#### 前端

**导入页面**:
```
/points/import
├─ 上传区域(拖拽 CSV 或粘贴内容)
├─ 预览表格(前 10 行,错误行标红)
├─ 字段映射(自动识别,支持手动调整)
├─ 目标 Source 选择器
└─ 导入进度条 + 结果统计
```

**导出页面**:
```
/points/export
├─ 筛选条件(Source/Channel/Device 下拉)
├─ 预览表格(可选)
└─ 下载 CSV / 复制到剪贴板
```

**配置历史页面**:
```
/config/history
├─ 时间线列表(快照卡片)
│   ├─ 快照描述 + 创建时间 + 创建人
│   └─ 操作: 对比 / 回滚
├─ Diff 弹窗
│   ├─ 左侧: 旧配置
│   └─ 右侧: 新配置
│       ├─ 新增(绿色)
│       ├─ 删除(红色)
│       └─ 修改(黄色)
└─ 回滚确认弹窗
```

**验收**:
```bash
# 导入
curl -X POST /api/v1/points/import \
  -F "source_biz_id=s7_main" \
  -F "file=@500_points.csv"

# 导出
curl "/api/v1/points/export?source_biz_id=s7_main" > points.csv

# 快照
curl -X POST /api/v1/config/snapshots \
  -d '{"description":"修改前备份"}'

# 回滚
curl -X POST /api/v1/config/snapshots/123/rollback

# Diff
curl "/api/v1/config/diff?from=122&to=123"
```

---

### 3.2 设备详情页实时数据预览

**优先级**:⭐⭐⭐ | **预估**:2 天 | **依赖**:无

**问题**:当前设备详情页只显示静态配置,无法看到点位实时值;需跳转到 Prometheus 才能看到采集状态。

**改造**:

#### 后端

**新增 Runtime 聚合接口**:

```go
// internal/interface/web/handlers_crud.go

// GET /api/v1/channels/runtime
// 返回所有 Channel 的运行时状态
// 响应:
type ChannelRuntime struct {
    ChannelBizID string     `json:"channel_biz_id"`
    Role         string     `json:"role"`  // collect | forward
    Type         string     `json:"type"`  // s7 | modbus | opcua | mqtt | influxdb
    Status       string     `json:"status"` // online | offline | degraded
    ConnectedAt  *time.Time `json:"connected_at,omitempty"`
    LastError    string     `json:"last_error,omitempty"`
    // Protocol 特定
    S7Status     *S7RuntimeStatus     `json:"s7_status,omitempty"`
    ModbusStatus *ModbusRuntimeStatus `json:"modbus_status,omitempty"`
    OpcuaStatus  *OpcuaRuntimeStatus  `json:"opcua_status,omitempty"`
    MqttStatus   *MqttRuntimeStatus    `json:"mqtt_status,omitempty"`
    InfluxStatus *InfluxRuntimeStatus  `json:"influx_status,omitempty"`
}

// GET /api/v1/devices/:biz_id/runtime
// 返回设备下所有 Source 的实时采集状态
type DeviceRuntime struct {
    DeviceBizID   string           `json:"device_biz_id"`
    Sources       []SourceRuntime  `json:"sources"`
    Targets       []TargetRuntime  `json:"targets"`
    LastCollectAt *time.Time       `json:"last_collect_at,omitempty"`
    CollectStatus string           `json:"collect_status"` // running | stopped | error
}

// GET /api/v1/sources/:biz_id/values
// 返回 Source 下所有点位的当前值(从内存缓存拉取)
// 响应:
type SourceValues struct {
    SourceBizID string                   `json:"source_biz_id"`
    Values     map[string]PointValueDTO `json:"values"`
    UpdatedAt  time.Time                `json:"updated_at"`
}
type PointValueDTO struct {
    PointBizID string `json:"point_biz_id"`
    Address    string `json:"address"`
    Value      any    `json:"value"`      // AsGoNative() 的结果
    Quality    string `json:"quality"`    // good | bad | uncertain
    Timestamp  int64  `json:"timestamp"`  // Unix ms
    CollectOK  bool   `json:"collect_ok"` // 本周期是否成功
}

// GET /api/v1/targets/:biz_id/diag
// 返回 Target 诊断信息
type TargetDiag struct {
    TargetBizID    string  `json:"target_biz_id"`
    Status         string  `json:"status"`   // connected | disconnected | error
    SentTotal      int64   `json:"sent_total"`
    ErrorTotal     int64   `json:"error_total"`
    QueueLength    int     `json:"queue_length"`
    QueueCapacity  int     `json:"queue_capacity"`
    DroppedTotal   int64   `json:"dropped_total"`
    LastSentAt     *int64  `json:"last_sent_at,omitempty"`  // Unix ms
    LastError      string  `json:"last_error,omitempty"`
    ConnectLatency float64 `json:"connect_latency_ms,omitempty"`
}
```

#### 前端

**设备详情页改造**(`/devices/:biz_id`):

```
┌──────────────────────────────────────────────────────────────┐
│  设备详情: plc_01                          [编辑] [克隆] [删除] │
├──────────────────────────────────────────────────────────────┤
│  [基本信息]  [Sources]  [Targets]  [实时数据]  [诊断]       │
├──────────────────────────────────────────────────────────────┤
│  ▶ 实时数据 (自动刷新 5s)                                      │
│  ┌────┬──────────┬────────┬──────────┬────────┬─────────┐  │
│  │ #  │ Point    │ Address│ 当前值    │ 质量   │ 最后更新  │  │
│  ├────┼──────────┼────────┼──────────┼────────┼─────────┤  │
│  │ 1  │ p_001    │DB1.DBD0│ 25.6      │ ✅ good │ 10:30:01│  │
│  │ 2  │ p_002    │DB1.DBD4│ 1.02      │ ✅ good │ 10:30:01│  │
│  │ 3  │ p_003    │DB1.DBD8│ [连接断开] │ ❌ bad  │ 10:29:55│  │
│  └────┴──────────┴────────┴──────────┴────────┴─────────┘  │
│  ▶ 诊断                                                       │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ Source: s7_main   [在线] P99: 42ms   最后采集: 10:30:01 │  │
│  │ Target: t_mqtt    [在线] Sent: 1.2M  Error: 12        │  │
│  └────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────┘
```

**Channel 列表页改造**:
- 每行追加状态列(在线/离线/降级),图标+颜色标识
- 点击状态列展开协议特定信息

**Target 详情页改造**:
```
/targets/:biz_id/diag
├─ 连接状态卡片(大图标 + 状态)
├─ 实时指标(Sent/Error/Dropped 计数)
├─ 队列使用率进度条
├─ 最近错误列表(最新 10 条)
└─ [重新连接] 按钮
```

**验收**:
```bash
# Channel 运行时状态
curl -H "Authorization: Bearer $T" http://localhost:8080/api/v1/channels/runtime

# 设备运行时状态
curl -H "Authorization: Bearer $T" http://localhost:8080/api/v1/devices/plc_01/runtime

# Source 点位实时值
curl -H "Authorization: Bearer $T" http://localhost:8080/api/v1/sources/s7_main/values

# Target 诊断
curl -H "Authorization: Bearer $T" http://localhost:8080/api/v1/targets/t_mqtt/diag
```

---

### 3.3 配置变更历史与回滚

**优先级**:⭐⭐⭐ | **预估**:3 天 | **依赖**:3.1 的快照表

**问题**:配置变更无法审计,出错后无法快速回滚。

**改造**:

#### 后端

**快照触发时机**:
- 用户在 Web UI 点击"保存"时,后端自动创建快照
- API 调用 `UpdateChannel/UpdateDevice/UpdateSource/UpdatePoint/UpdateTarget` 时,自动创建快照
- 快照描述格式: `{user} 修改了 {entity_type} {entity_biz_id}`

**快照自动清理**:
- 保留最近 50 个快照(或最近 30 天,取较早者)
- 后台 goroutine 每天清理一次

**回滚实现**:
```go
// 回滚流程:
// 1. 获取目标快照的 config_json
// 2. 解析 JSON → 重新构建所有实体
// 3. 在事务中: 先删除当前所有配置 → 再插入快照配置
// 4. 生成新快照(描述="回滚到快照 {id}")
// 5. 触发 plcbridge 配置热加载(config.json 变更)

func (h *Handler) RollbackSnapshot(c *gin.Context) {
    id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
    snapshot, err := h.store.GetSnapshot(id)
    if err != nil {
        web.WriteError(c, web.ErrSnapshotNotFound)
        return
    }
    
    // 解析快照
    var cfg platform.Config
    if err := json.Unmarshal([]byte(snapshot.ConfigJSON), &cfg); err != nil {
        web.WriteError(c, web.ErrInvalidConfig.With(err.Error()))
        return
    }
    
    // 事务中重建配置
    if err := h.store.RollbackToSnapshot(ctx, &cfg); err != nil {
        web.WriteError(c, web.ErrInternal.With(err.Error()))
        return
    }
    
    // 触发 plcbridge 热加载(写 config.json → fsnotify 触发)
    if err := h.configExporter.Export(ctx, &cfg); err != nil {
        logging.Warn("snapshot rollback config export fail", "err", err)
    }
    
    c.JSON(200, gin.H{"rollback": true, "snapshot_id": id})
}
```

#### 前端

**历史页面**:
```
/config/history
├─ 时间线视图
│   ├─ [自动快照] admin 修改了 channel ch_s7_main  @ 10:30:01 [对比] [回滚]
│   ├─ [手动快照] admin 修改前备份 @ 10:00:00      [对比] [回滚]
│   └─ ...
├─ Diff 弹窗
│   ├─ 变更实体列表(点击展开详情)
│   ├─ JSON Diff 视图(语法高亮)
│   └─ [回滚到此版本] 按钮
└─ 自动快照说明(保存时自动创建,无需手动)
```

**Diff 实现**:
```typescript
// 使用 diff 库对比两个 config JSON
import { diffJson } from 'diff'

function computeDiff(from: Config, to: Config) {
  const fromJson = JSON.stringify(from, null, 2)
  const toJson = JSON.stringify(to, null, 2)
  return diffJson(fromJson, toJson)
}
```

**验收**:
```bash
# 列出快照
curl /api/v1/config/snapshots

# 对比两个快照
curl "/api/v1/config/diff?from=10&to=11"

# 回滚
curl -X POST /api/v1/config/snapshots/10/rollback
# 期望: 返回 200,plcbridge 2s 内加载新配置
```

---

### 3.4 Target 诊断面板

**优先级**:⭐⭐ | **预估**:1.5 天 | **依赖**:3.2 的 runtime 接口

**问题**:Target 连接状态、sent/error 计数不在 Web UI 里,需查 Prometheus。

**改造**:

#### 后端

**复用 3.2 的 `GET /api/v1/targets/:biz_id/diag` 接口**,补充以下细节:

```go
type TargetDiagDetail struct {
    TargetBizID   string  `json:"target_biz_id"`
    Type          string  `json:"type"`  // mqtt | influxdb
    Status        string  `json:"status"`
    
    // MQTT 特有
    BrokerURL     string  `json:"broker_url,omitempty"`
    ClientID      string  `json:"client_id,omitempty"`
    ConnectedAt   *int64  `json:"connected_at,omitempty"`
    QoS           int     `json:"qos,omitempty"`
    
    // InfluxDB 特有
    ServerURL     string  `json:"server_url,omitempty"`
    Database      string  `json:"database,omitempty"`
    WriteURL      string  `json:"write_url,omitempty"`
    
    // 通用指标(从 Prometheus metrics 拉取或内存维护)
    Metrics       TargetMetrics `json:"metrics"`
}
type TargetMetrics struct {
    SentTotal     int64   `json:"sent_total"`
    ErrorTotal    int64   `json:"error_total"`
    DroppedTotal  int64   `json:"dropped_total"`
    QueueLen      int     `json:"queue_len"`
    QueueCap      int     `json:"queue_cap"`
    LastSentAt    *int64  `json:"last_sent_at,omitempty"`
    LastError     string  `json:"last_error,omitempty"`
    LatencyP99    float64 `json:"latency_p99_ms,omitempty"`
}
```

#### 前端

**Target 详情页**:
```
/targets/:biz_id
├─ [基本信息]  [诊断]
├─ 诊断 Tab
│   ├─ 连接状态卡片
│   │   ├─ MQTT: Broker / ClientID / QoS / Connected Duration
│   │   └─ InfluxDB: Server / Database / Write URL / Retention
│   ├─ 实时指标
│   │   ├─ Sent: 1,234,567  [计数]
│   │   ├─ Error: 12         [红色告警]
│   │   ├─ Dropped: 0        [绿色]
│   │   └─ 队列: 12/10000    [进度条]
│   ├─ 延迟 P99: 45ms        [趋势图 mini]
│   └─ [重新连接] [刷新指标]
└─ 最近错误列表
    ├─ 10:30:01 send fail: connection reset by peer
    ├─ 10:28:55 send fail: timeout
    └─ ...
```

**验收**:
```bash
# Target 诊断
curl /api/v1/targets/t_mqtt/diag
# 期望:
{
  "target_biz_id": "t_mqtt",
  "type": "mqtt",
  "status": "connected",
  "metrics": {
    "sent_total": 1234567,
    "error_total": 12,
    "queue_len": 12,
    "queue_cap": 10000
  }
}
```

---

### 3.5 InfluxDB 历史趋势图

**优先级**:⭐⭐ | **预估**:2 天 | **依赖**:InfluxDB v2 API

**问题**:用户无法在 Web UI 里预览 InfluxDB 里存储的历史数据。

**改造**:

#### 后端

**新增 InfluxDB Query API**:

```go
// internal/interface/web/handlers_crud.go

// GET /api/v1/influxdb/query
// Query: ?database=plcbridge&q=SELECT * FROM ... (Flux 或 InfluxQL)
// Response: JSON 格式的查询结果
// 注意: 只允许查询已配置的 InfluxDB 实例,禁止跨库查询

// GET /api/v1/channels/:biz_id/preview
// Query: ?from=1699999999000&to=1700000099000&interval=10s
// Response: 趋势数据,用于前端 ECharts 渲染
type PreviewData struct {
    ChannelBizID string          `json:"channel_biz_id"`
    Series       []PreviewSeries  `json:"series"`
    From         int64            `json:"from"`
    To           int64            `json:"to"`
    Interval     string           `json:"interval"`
}
type PreviewSeries struct {
    PointBizID string    `json:"point_biz_id"`
    Address    string    `json:"address"`
    DataPoints []DataPoint `json:"data_points"`
}
type DataPoint struct {
    Timestamp int64   `json:"timestamp"` // Unix ms
    Value     float64 `json:"value"`
    Quality   string  `json:"quality"`
}
```

**安全约束**:
- 只能查询 `channels.config` 中已注册的 InfluxDB Channel
- 限制查询时间范围(最大 7 天)
- 限制返回点数(最大 10000 点)
- SQL 注入防护(参数化查询)

#### 前端

**Channel 详情页 → 历史趋势 Tab**:
```
/channels/:biz_id  [历史趋势]

┌────────────────────────────────────────────────────────────┐
│  时间范围: [最近1小时 ▼]  [自定义范围选择器]                   │
│  指标选择: [☑ p_001] [☑ p_002] [+ 添加指标]                │
├────────────────────────────────────────────────────────────┤
│  趋势图(ECharts)                                           │
│  ┌──────────────────────────────────────────────────────┐ │
│  │          ╱╲    ╱╲                                      │ │
│  │    ────╱──╲──╱──╲──── p_001 (温度)                    │ │
│  │        ╱      ╲    ╱╲    ╱╲                            │ │
│  │   ────╱───────╲──╱──╲──╱──╲─ p_002 (压力)            │ │
│  │                                                      │ │
│  └──────────────────────────────────────────────────────┘ │
│  数据点: 1,234  |  查询耗时: 245ms  |  [导出 CSV]           │
└────────────────────────────────────────────────────────────┘
```

**验收**:
```bash
# 趋势预览
curl "/api/v1/channels/ch_influxdb/preview?from=1699999999000&to=1700000099000&interval=10s"

# 自定义查询
curl "/api/v1/influxdb/query?database=plcbridge&q=SELECT+*+FROM+points+WHERE+time+%3E+now()-1h"
```

---

### 3.6 Dark Mode

**优先级**:⭐ | **预估**:0.5 天 | **依赖**:无

**改造**:

```typescript
// web/src/composables/useTheme.ts
import { useDark, useToggle } from '@vueuse/core'

export const isDark = useDark({
  storageKey: 'plcbridge-theme',
  valueDark: 'dark',
  valueLight: 'light',
})

export const toggleTheme = useToggle(isDark)

// Element Plus 自动跟随
watch(isDark, (dark) => {
  document.documentElement.classList.toggle('dark', dark)
})
```

**验收**:
- 刷新页面后主题保持
- 右上角主题切换按钮工作正常

---

### 3.7 设备克隆

**优先级**:⭐ | **预估**:1 天 | **依赖**:无

**问题**:批量创建同类型设备时,每个都手动配置效率低。

**改造**:

#### 后端

```go
// POST /api/v1/devices/:biz_id/clone
// 请求体: {new_device_biz_id, new_device_name, prefix_for_biz_ids}
// 行为: 深拷贝 Device + 所有 Source + 所有 Point + 所有 Target
//       所有 _biz_id 加上前缀,保证唯一性
//       Channel 不拷贝(引用现有 Channel)

type CloneRequest struct {
    NewDeviceBizID     string `json:"new_device_biz_id" binding:"required"`
    NewDeviceName      string `json:"new_device_name"`
    BizIDPrefix        string `json:"biz_id_prefix"`  // 如 "copy_of_", 默认 "clone_"
}
```

#### 前端

**设备列表页**:
```
[克隆] 按钮 → 弹出克隆向导
├─ 新设备 ID
├─ 新设备名称
├─ biz_id 前缀(如 clone_s7_main, clone_p_001)
└─ [预览克隆结果] [确认克隆]
```

**验收**:
```bash
curl -X POST /api/v1/devices/plc_01/clone \
  -d '{"new_device_biz_id":"plc_02","new_device_name":"二号车间"}'
# 期望: 创建 plc_02,包含所有 Source/Point/Target,biz_id 自动加前缀
```

---

### 3.8 键盘快捷键

**优先级**:⭐ | **预估**:0.5 天 | **依赖**:无

**改造**:

```typescript
// web/src/composables/useGlobalShortcuts.ts
import { useMagicKeys } from '@vueuse/core'

export function useGlobalShortcuts() {
  const { Ctrl_N, Ctrl_S, Ctrl_R, Escape } = useMagicKeys()
  
  watch(Ctrl_N, (pressed) => {
    if (pressed) router.push('/devices/new')
  })
  
  watch(Ctrl_S, (pressed) => {
    if (pressed) {
      // 触发当前表单保存
      const event = new KeyboardEvent('shortcut-save')
      window.dispatchEvent(event)
    }
  })
  
  watch(Ctrl_R, (pressed) => {
    if (pressed) location.reload()
  })
  
  watch(Escape, (pressed) => {
    if (pressed) router.back()
  })
}
```

**验收**:
- Ctrl+N / Ctrl+S / Ctrl+R / Escape 全局生效
- 输入框获得焦点时不触发

---

### 3.9 Swagger UI

**优先级**:⭐ | **预估**:0.5 天 | **依赖**:`api/openapi.yaml` 已存在

**改造**:

```go
// internal/infra/api/server.go
import (
    "github.com/swaggo/files"
    swagger "github.com/swaggo/files"
    ginSwagger "github.com/swaggo/gin-swagger"
)

// 挂载 swagger 静态资源
api.GET("/swagger/*any", ginSwagger.WrapHandler(swagger.Handler))
```

**验收**:
- 访问 `/api/v1/swagger/index.html` 显示 Swagger UI
- 所有 API 文档可交互测试

---

## 4. 实施顺序(推荐)

```
Phase 1: 运维效率(高价值,低依赖)
  ├─ 3.1 CSV 导入/导出 Points (2d)
  └─ 3.7 设备克隆 (1d)

Phase 2: 可观测性深化(高价值)
  ├─ 3.2 设备详情页实时数据预览 (2d)
  ├─ 3.4 Target 诊断面板 (1.5d)
  └─ 3.5 InfluxDB 历史趋势图 (2d)

Phase 3: 配置安全
  ├─ 3.3 配置变更历史与回滚 (3d)
  └─ 3.6 Dark Mode (0.5d)

Phase 4: UX 优化(低优先级)
  ├─ 3.8 键盘快捷键 (0.5d)
  └─ 3.9 Swagger UI (0.5d)
```

**关键里程碑**:
- **Phase 1 结束** → 批量配置效率提升 10 倍
- **Phase 2 结束** → 设备状态可观测,无需跳转 Prometheus
- **Phase 3 结束** → 配置变更可审计/可回滚
- **Phase 4 结束** → UX 完善

---

## 5. 风险与回滚

| 风险 | 影响 | 缓解 |
|---|---|---|
| CSV 导入破坏现有数据 | 误操作导致已有点位丢失 | 导入前自动创建快照;导入在事务中执行,失败自动回滚 |
| 实时数据预览导致性能问题 | 频繁轮询拉高后端负载 | 前端轮询间隔可配置(默认 5s);后端限制最大查询点数 |
| 回滚到旧快照破坏配置 | 旧快照本身有 bug | 回滚前自动创建快照(描述="回滚前备份");回滚在事务中执行 |
| InfluxDB 查询超时 | 大范围查询阻塞 API | 限制查询时间范围(7 天)和返回点数(10000);超时快速失败 |
| 设备克隆 biz_id 冲突 | 克隆后 biz_id 已存在 | 前端校验 biz_id 唯一性;后端事务中原子创建,冲突返回错误 |

---

## 6. 关联文档

- **`md/Plan.md`**:MVP 方案(已交付)
- **`md/Plan2.md`**:演进方案(已交付)
- **`md/architecture.md`**:架构图
- **`md/SLO.md`**:SLO 告警规则
- **`md/dev/plugin-guide.md`**:协议插件开发指南
- **`api/openapi.yaml`**:REST API 契约

---

**最后更新**:2026-10-21
**负责**:plcbridge 后端 + 前端
**当前阶段**:Plan3 已全部交付 ✅

### 交付状态一览

| 编号 | 功能 | 状态 | 主要交付物 |
|------|------|------|-----------|
| 3.1 | Points CSV 导入/导出 | ✅ | `points_csv.go` + `points_csv.ts` + `Detail.vue` |
| 3.7 | 设备克隆 | ✅ | `device_clone.go` + `Detail.vue` / `List.vue` |
| 3.2 | 实时点位值预览 | ✅ | `collect.Workers.PointValueCache()` + `GET /api/v1/sources/:biz_id/values` |
| 3.4 | Target 诊断面板 | ✅ | `forward.Workers.TargetSnapshot()` + `GET /api/v1/targets/:biz_id/diag` |
| 3.5 | InfluxDB 历史趋势图 | ✅ | `target.HistoryQueryer` + `GET /api/v1/targets/:biz_id/history` + `TrendChart.vue` (ECharts) |
| 3.3 | 配置变更历史与回滚 | ✅ | `config_snapshots` 表 + `snapshots.go` + `History.vue` |
| 3.6 | Dark Mode | ✅ | `useDark` + `assets/global.css` 深色变量 |
| 3.8 | 键盘快捷键 | ✅ | `composables/useGlobalShortcuts.ts` |
| 3.9 | Swagger UI | ✅ | `internal/infra/swagger` + `GET /swagger/` + `GET /api/openapi.yaml` |
