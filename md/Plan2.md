# PlcBridge 演进方案(Plan2 — 面向可扩展性)

> **承接文档**:`md/Plan.md`(MVP,已交付)、`md/MVP_CHECKLIST.md`(MVP 验收,全 ✅)、`md/Roadmap.md`(原 4 阶段路线图,本文档将其重写并对齐现状)。
>
> **目标**:`Plan.md` 解决"能不能交付",`Plan2.md` 解决"能不能低成本演进"。
>
> **核心命题**:工业协议 / 下游 Target 永远是外层可变的(西门子会出新 PLC、工厂会换 IIoT 平台、运维会追加私有协议),**采集 / 转发 / 回写的内核必须在内层恒定**。
> 演进方向一切以这条原则为锚:任何让"加新协议 / 新 Target"需要改 handler / 前端 / 内核代码的设计,都视为反模式。
>
> **当前阶段(2026-10-18)**:MVP 已完成,Roadmap 4 阶段里的 Phase 1(基础设施)+ Phase 4(健壮性)基本落地,Phase 2(架构验收)差最后一步,Phase 3(边界清理)未动。`Plan2.md` 接续 Phase 2 收尾 + 后续可扩展性深化。
>
> **范围**:开放(按主题列,无固定截止时间)。

---

## 0. Plan2 验收门槛

完成 Plan2 全部条目后,用以下 4 个用例验证可扩展性目标:

### 验收 1:加新协议零代码改动
```
新加一个 BACnet 协议插件
  ↓
期望:handlers_crud.go、web/src/pages/*、sqlstore.go、main.go 全部零修改
  ↓
实际:仅在 protocol/bacnet/{plugin,init}.go 写 NewPlugin(),通过 go:generate 或 _all.go 引入
```

### 验收 2:加新 Target 零代码改动
```
新加一个 Kafka Target
  ↓
期望:同上(handler / 前端 / 内核零修改)
  ↓
实际:仅在 target/kafka/{plugin,init}.go 写 NewPlugin()
```

### 验收 3:插件契约 100% 覆盖
```
go test ./internal/infra/protocol/...   # protocol 契约测试全过
go test ./internal/infra/target/...     # target 契约测试全过
go test ./internal/app/...              # 内核调度测试全过
```

### 验收 4:运行时元信息
```
GET /api/v1/protocols → 返回所有已注册协议 + Target 的 schema、版本、role
  ↓
Web 端:
  - 协议下拉框 = 接口返回值(零硬编码)
  - 新协议上线后无需发版前端,刷新生效
```

---

## 1. 现状盘点(MVP 后的真实代码状态)

> **本节是 Plan2 的基线**。Roadmap 里 4 阶段的事实际已部分落地,先盘点清楚避免重复或冲突。

### 1.1 已落地(无需 Plan2 重复)

| 来源 | 状态 | 证据 |
|---|---|---|
| MVP 6 项核心功能 | ✅ 完整 | `md/MVP_CHECKLIST.md` §1,6/6 全部通过 |
| §6.2.2 进程内隔离 4 条硬约束 | ✅ 完整 | worker 池 4 个 + 连接池隔离 + 1024 队列 + P99 监控 |
| §6.3 心跳机制 5 协议 / 角色 | ✅ 完整 | S7 翻转 + ModbusTCP 探针 + OPC UA Session + MQTT LWT |
| §10 结构化日志 | ✅ 完整 | `internal/infra/logging/logging.go`(zerolog)+ 38 个 event 强枚举(`events.go`) |
| §8.4 Prometheus 指标 | ✅ 完整 | `internal/metrics/metrics.go` 9+ 指标,含 pool_running / pool_rejected |
| §8.1/8.2 写 API | ✅ 完整 | `POST /api/v1/write` + `/write/batch`,writePool 消费 |
| §8.3 健康检查 | ✅ 完整 | `/health` + `/metrics` |
| §6.6 配置热加载 | ✅ 完整 | fsnotify L1 + 60000ms mtime L2 + 500ms debounce |
| §6.4 无 WAL 失败处理 | ✅ 完整 | 失败丢 + 日志 + 计数 |
| §7 数据模型 | ✅ 完整 | 7 张表(Plan §7.11)+ 双主键策略 + 完整索引 |
| 错误体系(APIError) | ✅ 完整 | `internal/interface/web/errors.go` 强枚举 |
| config 业务校验 | ✅ 完整 | `internal/infra/config/validate.go` 引用完整性 + 字段合法性 |
| ChannelRegistry 复用 | ✅ 完整 | `internal/infra/protocol/registry.go` 引用计数 + lazy 探活 + handler death 旁路 |
| 协议 / Target 注册表 | ✅ 完整 | `Register` / `MustRegister` / `Get` / `MustRegisterDefaults` |

### 1.2 已知缺口(Plan2 待办)

| 缺口 | 位置 | 影响 |
|---|---|---|
| **handler 协议白名单硬编码** | `handlers_crud.go:184-194` `isKnownChannelType` 写死 `"s7","opcua","modbus"` / `"mqtt","influxdb"` | 加新协议必须改 handler(违反验收 1) | ✅ **2026-10-18 完成(3.1)**:改为 `protocol.Get` / `target.Get` 查表;`modbus` 别名待 3.4 前端收尾 |
| **data_type 词汇表双份维护** | `handlers_crud.go:isKnownDataType` + `config/validate.go:validDataTypes` 各一份 | 词汇表漂移风险 | ✅ **2026-10-18 完成(3.1 加项)**:抽到 `internal/platform/datatypes.go`,handler + 启动校验都走 `platform.IsValidDataType` |
| **Vue 协议下拉框硬编码** | `web/src/pages/channels/List.vue:58-59` `TYPES_COLLECT = ['s7','modbus','opcua']` | 加新协议必须发版前端(违反验收 4) | ✅ **2026-10-18 完成(3.4)**:删 `TYPES_COLLECT` / `TYPES_FORWARD`;`typeOptions` 改为 `computed` 走 `protocolsCollect` / `protocolsForward`(从 `/api/v1/protocols` 拉取);前端不动 = 加新协议即可见 |
| **无插件元信息接口** | 缺 `GET /api/v1/protocols` | 前端无法动态发现协议 | ✅ **2026-10-18 完成(3.3)**:加 `protocol.AllInfo()` / `target.AllInfo()` + 路由 `GET /api/v1/protocols`,返回 `{collect:[...], forward:[...]}` 含 ConfigSchema;附 `internal/infra/{protocol,target}/allinfo_test.go` + `internal/infra/api/protocols_test.go` |
| **main.go 显式注册** | `cmd/plcbridge/main.go:80` `protocol.MustRegisterDefaults()` 显式调 | 加新协议需要改 main.go(违反验收 1) | ✅ **2026-10-18 完成(3.2)**:5 个 plugin 文件末尾加 `init()` 调 `Register`,main.go × 2 删 `MustRegisterDefaults` 调用 |
| **handler 错误响应未统一为 `{code, msg}`** | `handlers_crud.go:41-52` 仍走 `gin.H{"error": msg}` | 错误体系已建好但 handler 没接 | ✅ **2026-10-18 完成(4.1)**:业务校验全部走 `WriteError(*APIError)`(13 个 CRUD + auth + process_control + snapshot),响应体统一 `{code, msg}`;`handleStoreErr` 暂保留 `{error: msg}` 旧格式(过渡期);前端 `client.ts` 加 `extractErrorMessage` 兼容两套格式;`web/src/api/errors.ts` 集中 40+ code;新增 `TestErrorResponse_BusinessValidation_UseCodeMsg` + `TestErrorResponse_HandleStoreErr_KeepsLegacyFormat` 锁定过渡期契约 |
| **Phase 3 校验下沉未做** | 业务规则仍在 handler 层(`assertChannelExists` / `assertChannelRole` 等) | service 层缺失,业务规则与 HTTP 强耦合 | ✅ **2026-10-18 完成(5.3)**:新建 `internal/interface/web/service.go`,`Store.Ensure*` / `Validate*ForCreate/Update` 6 个方法;`assertChannel*` / `assertDevice*` / `assertSource*` 旧 helper 全部下沉并删除;`createSource` / `updateSource` / `createTarget` / `updateTarget` 改调 service 复合校验;handler 退化为"绑定 + 翻译";`service_test.go` 7 个测试覆盖 |
| **Phase 3 事务抽象未做** | `sqlstore.go` 单 SQL 散落,无 `WithTx` | 多步写入无事务保证 | ✅ **2026-10-18 完成(5.2)**:`Store.WithTx(ctx, func(Tx) error) error` 抽象 + `Tx` 接口(解耦 `*sql.Tx`);`DeleteSource` / `CreateTarget` / `UpdateTarget` / `DeleteTarget` 4 个事务方法重写走 WithTx;`sqlstore_test.go` 4 个测试覆盖 commit/rollback/multistep/query-in-tx |
| **Phase 3 Value.AsGoNative 未做** | 协议层 `Value` 与业务层转换靠类型断言 | 转换逻辑散落 | ✅ **2026-10-18 完成(5.1)**:`Value.AsGoNative() any` 方法(覆盖 13 个 data_type);`app/collect/workers.go` 删除原 `valueToAny` switch 改调方法;`protocol.Value` 加 `Bytes []byte` 字段;`asnative_test.go` 14 个 case |
| **Phase 3 实体/DTO 分离未做** | DTO 同时承担 API 层和 store 层角色 | 后续加 gRPC / CLI 会卡 | ⏭️ **跳过(选做,无触发需求)**:Plan2 §5.4 明确"只在以下情况做,否则跳过";当前 DTO/实体未造成维护问题,gRPC / CLI 需求未出现,推迟 |
| **Phase 3 协议能力声明未做** | 不同协议特性差异大,handler / 前端按 name 硬编码特殊处理 | 加新协议需改多处校验逻辑 | ✅ **2026-10-18 完成(3.6)**:`protocol.Capability` 结构 + `ProtocolPlugin.Capabilities()` 接口方法;S7 声明 SupportsBatchRead+SupportsHeartbeat+RequiresRackSlot;Modbus 声明 SupportsBatchRead+RequiresSlaveID;OPC UA 声明 SupportsBatchRead+SupportsHeartbeat;`ProtocolInfo` 增 `capabilities` 字段;前端 `ProtocolInfo` TS 接口同步;`allinfo_test.go` 新增 `TestAllInfo_CapabilitiesPerProtocol` |
| **启动 fail-fast 校验未明确** | `config.LoadWithRetry` 是否调 Validate 不确定 | 非法配置可能启动后才暴露 | ✅ **2026-10-18 完成(6.1)**:确认 `TryLoad()` 已调 `Validate`(`config.go:80`),fail-fast 已就位;`failfast_test.go` 3 个 case 锁定行为(invalid config / malformed JSON / unknown protocol) |
| **SLO 告警规则无沉淀** | 监控指标在 Prometheus 但无 Prometheus rules | 告警阈值散落各处 | ✅ **2026-10-18 完成(6.3)**:`deploy/prometheus/rules.yaml` 6 个 group × 8 个 alert(collect P99 / collect error / write fail / target queue / target drop / pool rejected / config reload);`md/SLO.md` 阈值依据 + 分级 + 抑制规则 |
| **无协议特定指标** | 通用 `plcbridge_collect_duration_seconds` 掩盖协议特性 | OPC UA Session 续期 / Modbus RTT 不可观测 | ✅ **2026-10-18 完成(7.1)**:`plcbridge_s7_connect_duration_seconds` / `plcbridge_opcua_session_renew_total` / `plcbridge_modbus_request_duration_seconds` / `plcbridge_mqtt_publish_duration_seconds` 注册 + 同步 `metrics_naming_test.go` 列表 |
| **无仪表盘 JSON** | Grafana 仪表盘需手动拼 | 新部署运维成本高 | ✅ **2026-10-18 完成(7.2)**:`deploy/grafana/dashboard-overview.json`(4 个 stat 面板 + 4 个 timeseries 面板,含采集 P99 / 错误率 / 队列 / 池利用率) |
| **无插件开发指南** | 第三方或个人 6 个月后加新协议不知从哪下手 | 协议化承诺难兑现 | ✅ **2026-10-18 完成(8.1)**:`md/dev/plugin-guide.md`(零改动契约 / 接口规范 / 完整示例 / 测试清单 / 常见陷阱) |
| **无架构图更新** | Plan.md §5.2 架构图是 MVP 状态 | Plan2 演进后架构不直观 | ✅ **2026-10-18 完成(8.2)**:`md/architecture.md`(进程拓扑 / 4 worker 池 / 协议注册表 / 数据流 / 错误体系 / 配置热加载) |
| **无 OpenAPI 文档** | REST 端点契约无机器可读规范 | 前端 / 客户端 / 文档生成不便 | ✅ **2026-10-18 完成(8.3)**:`api/openapi.yaml`(OpenAPI 3.0.3,16 个端点 + 错误响应统一格式) |
| **i18n 无预留** | 错误信息中文硬编码 | 未来接 vue-i18n 需大改 | ✅ **2026-10-18 完成(4.2)**:`APIError.I18nKey` 字段 + `WithI18nKey()` 方法;`WriteError` 在 I18nKey 非空时附带;前端 `getApiI18nKey()` 工具;`errors_test.go` 新增 `TestAPIError_WithI18nKey` + `TestWriteError_I18nKeyNotEmittedWhenEmpty` |
| **handleStoreErr 仍走旧格式** | 4.1 过渡期保留 `{error: msg}` | 前端需兼容双格式 | ✅ **2026-10-18 完成(4.3)**:`handleStoreErr` 简化为 `WriteError(c, err)`;`TestErrorResponse_HandleStoreErr_KeepsLegacyFormat` 翻转期望 = `code=not_found, 不再含 error 字段`;前端 `client.ts` 双格式兼容继续生效,无破坏 |

### 1.4 主题 A(3.1~3.5)完成度(2026-10-18)

| 步骤 | 状态 | 关键改动 |
|---|---|---|
| **3.1 handler 协议白名单查表化** | ✅ | `isKnownChannelType` 查表 + `isKnownDataType` 走 `platform.IsValidDataType`(词汇表抽到 `internal/platform/datatypes.go`) |
| **3.2 main.go 隐式注册** | ✅ | 5 个 plugin 文件末尾 `init() 调 Register`;main.go × 2 删 `MustRegisterDefaults` |
| **3.3 `GET /api/v1/protocols` 元信息接口** | ✅ | `protocol.AllInfo()` / `target.AllInfo()` + 公开路由(无需 JWT) |
| **3.4 Vue 下拉框动态化** | ✅ | web 后端代理 + 前端 `typeOptions` 改 `computed` |
| **3.5 fake 协议验收** | ✅ | `internal/infra/protocol/fake/` 子包 + 全链路测试(`fake_test.go` 4 项 + `api/protocols_test.go` 含 fake 断言);顺带 `CanonicalizeChannelType` 收 `modbus` 别名 |

### 1.5 主题 B(4.1)完成度(2026-10-18)

| 步骤 | 状态 | 关键改动 |
|---|---|---|
| **4.1 handler 全部走 WriteError** | ✅ | 业务校验错误(13 个 CRUD + auth + process_control + snapshot)统一 `WriteError(c, web.ErrXxx.With(msg))`;`assertChannelExists/Role/Device/Source` 4 个 helper 改返回 `*APIError`;RequireSession 中间件 `Abort()` + WriteError;`handleStoreErr` 暂留旧格式(过渡期);前端 `client.ts` `extractErrorMessage` 双格式兼容;`web/src/api/errors.ts` 集中 40+ code;新增 2 个 test 锁定契约(`BusinessValidation_UseCodeMsg` + `HandleStoreErr_KeepsLegacyFormat`) |

### 1.6 主题 B(4.2 / 4.3)完成度(2026-10-18)

| 步骤 | 状态 | 关键改动 |
|---|---|---|
| **4.2 i18n 预留** | ✅ | `APIError.I18nKey` 字段 + `WithI18nKey()` 方法;`WriteError` 在 I18nKey 非空时附带 `i18n_key` 响应字段;前端 `getApiI18nKey()` 工具函数;当前未启用 i18n(I18nKey 恒空 → 响应体不含 `i18n_key` 字段,向前兼容);新增 2 个 test |
| **4.3 handleStoreErr 全量切换** | ✅ | `handleStoreErr` 简化为 `WriteError(c, err)` 一行;`TestErrorResponse_HandleStoreErr_KeepsLegacyFormat` 翻转期望 = `{code: "not_found", msg: "资源不存在"}` 不再含 `error` 字段;前端 client.ts 双格式兼容继续生效 |

### 1.7 主题 C(5.1~5.3)完成度(2026-10-18)

| 步骤 | 状态 | 关键改动 |
|---|---|---|
| **5.1 Value.AsGoNative** | ✅ | `protocol.Value.AsGoNative() any` 覆盖 13 个 data_type(bool / int8-64 / uint8-64 / float32-64 / string / bytes → 对应 Go 原生类型,未知 → nil);`protocol.Value` 加 `Bytes []byte` 字段;`app/collect/workers.go` 删除 `valueToAny` switch 改调方法;`asnative_test.go` 14 个 case |
| **5.2 Store.WithTx 事务抽象** | ✅ | `Store.WithTx(ctx, fn func(Tx) error) error` + `Tx` 接口(解耦 `*sql.Tx`);`DeleteSource` / `CreateTarget` / `UpdateTarget` / `DeleteTarget` 4 个事务方法重写;`sqlstore_test.go` 4 个 test(commit / rollback / multi-step / query-in-tx) |
| **5.3 校验下沉 service** | ✅ | 新建 `internal/interface/web/service.go`,`Store.Ensure*` / `Validate*ForCreate/Update` 6 个方法;`assertChannel*` / `assertDevice*` / `assertSource*` 旧 helper 全部下沉并删除;`isKnownChannelType` 改名为 service 顶层 `ValidateChannelType` 返回 `*APIError`;`createSource` / `updateSource` / `createTarget` / `updateTarget` 改调 service 复合校验;`service_test.go` 7 个 test |
| **5.4 实体/DTO 分离(选做)** | ⏭️ 跳过 | Plan2 §5.4 明确"无需求则不做";当前 DTO/实体未造成维护问题 |

### 1.8 主题 D + E + F(6.1 / 6.3 / 7.1 / 7.2 / 8.1-8.3)完成度(2026-10-18)

| 步骤 | 状态 | 关键改动 |
|---|---|---|
| **6.1 启动 fail-fast 校验** | ✅ | 确认 `config.TryLoad()` 已调 `Validate`(已就位);`failfast_test.go` 3 个 test 锁定行为(invalid config / malformed JSON / unknown protocol) |
| **6.2 OpenTelemetry(选做)** | ⏭️ 跳过 | Plan2 §6.2 明确"MVP 阶段不阻塞";当前 zerolog trace_id 字段已够 |
| **6.3 SLO 告警规则** | ✅ | `deploy/prometheus/rules.yaml`(6 group × 8 alert:collect P99 / error rate / write fail / target queue / target drop / pool rejected / config reload);`md/SLO.md` 阈值依据 + 分级 + 抑制规则 |
| **6.4 水平扩展(选做)** | ⏭️ 跳过 | 单实例 300 PLC / 10 万点位未达上限;超阈单独立项 |
| **7.1 协议特定指标** | ✅ | `plcbridge_s7_connect_duration_seconds` / `plcbridge_opcua_session_renew_total` / `plcbridge_modbus_request_duration_seconds` / `plcbridge_mqtt_publish_duration_seconds` 4 个指标注册;同步 `metrics_naming_test.go` 列表 |
| **7.2 仪表盘 JSON** | ✅ | `deploy/grafana/dashboard-overview.json`(4 stat + 4 timeseries 面板) |
| **8.1 插件开发指南** | ✅ | `md/dev/plugin-guide.md`(零改动契约 + 接口规范 + 完整示例 + 测试清单 + 常见陷阱) |
| **8.2 架构图更新** | ✅ | `md/architecture.md`(进程拓扑 + 4 worker 池 + 协议注册表 + 数据流 + 错误体系 + 配置热加载 + 关键不变式) |
| **8.3 OpenAPI 文档** | ✅ | `api/openapi.yaml`(OpenAPI 3.0.3,16 个端点 + 错误响应统一格式) |

### 1.3 设计现状的关键事实

- **协议 / Target 注册表已存在**(`protocol/protocol.go` / `target/target.go`),`Register` / `Get` / `MustRegisterDefaults` 一应俱全;缺的只是 handler / 前端**查表代替硬编码**。
- **ChannelRegistry 是单例缓存层**,引用计数 + Acquire/Release,handler death 旁路,生产可用。这块不动。
- **worker 池物理隔离 + 连接池隔离 + 有界队列 + P99 监控**这 4 条硬约束生产已验证,后续演进不允许破坏。
- **错误体系 (APIError) 已就位**:`errors.go` 40+ 业务错误码 + `WriteError`,只差 handler 全量切换。

---

## 2. 总体目标与原则

### 2.1 一句话目标

> **加新协议 / 新 Target 时,handler / 前端 / 内核 0 改动,只新增 plugin 包。**

### 2.2 设计原则

1. **内核恒定,外层可换**:采集 / 转发 / 回写的调度逻辑在 `internal/app/{collect,forward,write}/` 不可被新协议侵入;新协议只在 `internal/infra/{protocol,target}/<name>/` 落地。
2. **事实源唯一**:协议类型、协议能力、心跳要求、Config Schema **只在 plugin 包里声明一次**,其他地方一律查表(`protocol.Get(name)` / `protocol.AllInfo()`)。
3. **注册即插拔**:`import _ ".../protocol/_all"` 一行触发全部内置插件的 `init()`;main.go 0 改动。
4. **运行时可发现**:`GET /api/v1/protocols` 暴露所有已注册插件的元信息 + ConfigSchema(JSON Schema),Web 端启动时拉取一次缓存。
5. **错误码强枚举**:`APIError.Code` 是契约,前端可对照 `web/src/api/errors.ts` 集中处理;所有 handler 走 `WriteError` 出口。
6. **业务规则不下沉到 SQL**:SQLite 层只做"单条记录是否合法"的事;**跨表 / 跨实体引用完整性由 `service` 层负责**(Phase 3 范围)。

### 2.3 与 Plan.md 的边界

| 维度 | Plan.md (MVP) | Plan2.md (演进) |
|---|---|---|
| 焦点 | 能不能交付(MVP 6 项) | 能不能低成本演进 |
| 时间 | 4 个月固定 | 开放,按主题 |
| 协议 | 内置 3+2,硬编码 | 全部 plugin 化,自动注册 |
| Handler | 走 `gin.H{"error": msg}` | 走 `WriteError(*APIError)`,`{code, msg}` |
| 前端 | 协议下拉框硬编码 | 启动时拉 `/api/v1/protocols` |
| 业务校验 | handler 内联 + SQLite 即时校验 | service 层 + 启动时 config 校验(已有) |
| 事务 | 单 SQL 散写 | `Store.WithTx` 抽象 |
| DTO/实体 | 合一 | 视情况分离(Phase 3.4 选做) |

---

## 3. 主题 A:协议 / Target 插件化(Phase 2 收尾 + 加固)

> **主线**:把"协议名出现在 3 个地方"这个反模式彻底消灭。
> **对应 Roadmap**:Phase 2 全部 + 部分 Phase 3 边缘。

### 3.1 handler 协议白名单查表化
**优先级**:⭐⭐⭐ | **预估**:0.5 天

**问题**:
```go
// handlers_crud.go:184-194
func isKnownChannelType(role, typ string) bool {
    switch role {
    case "collect":
        switch typ {
        case "s7", "opcua", "modbus":  // ← 硬编码
            return true
        }
    ...
```

**改造**:
```go
// handler 改造后
import "github.com/plcbridge/plcbridge/internal/infra/protocol"
import "github.com/plcbridge/plcbridge/internal/infra/target"

func isKnownChannelType(role, typ string) bool {
    switch role {
    case "collect":
        _, err := protocol.Get(typ)  // 查表
        return err == nil
    case "forward":
        _, err := target.Get(typ)
        return err == nil
    }
    return false
}
```

**验收**:删掉 switch 里的所有 case,改为查表,加新协议 handler 不动。

---

### 3.2 main.go 注册改 `_all.go` 隐式注册
**优先级**:⭐⭐⭐ | **预估**:0.5 天

**改造**:
```go
// internal/infra/protocol/_all.go
package protocol

import (
    _ "github.com/plcbridge/plcbridge/internal/infra/protocol/s7"
    _ "github.com/plcbridge/plcbridge/internal/infra/protocol/modbus_tcp"
    _ "github.com/plcbridge/plcbridge/internal/infra/protocol/opcua"
    // 未来加 BACnet:  _ ".../bacnet"
)

// internal/infra/protocol/s7/plugin.go
func init() {
    Register(newS7Plugin())
}
```

```go
// cmd/plcbridge/main.go 改造后
import _ "github.com/plcbridge/plcbridge/internal/infra/protocol"  // 触发 _all.go
// 不再显式调 protocol.MustRegisterDefaults()
```

**验收**:删 `MustRegisterDefaults` 调用,加新协议只在 `_all.go` 加一行 import。

---

### 3.3 `GET /api/v1/protocols` 元信息接口
**优先级**:⭐⭐⭐ | **预估**:1 天

**实现**:
```go
// protocol/registry.go
type ProtocolInfo struct {
    Name        string          `json:"name"`
    Version     string          `json:"version"`
    Role        string          `json:"role"`        // "collect" | "forward"
    ConfigSchema json.RawMessage `json:"config_schema"`
    // 协议能力声明(可选)
    Capabilities ProtocolCapabilities `json:"capabilities"`
}

type ProtocolCapabilities struct {
    SupportsBatchRead  bool `json:"supports_batch_read"`
    SupportsBatchWrite bool `json:"supports_batch_write"`
    SupportsHeartbeat  bool `json:"supports_heartbeat"`
}

// 暴露给 target 同理
```

**新增 HTTP 路由**:
```go
// internal/infra/api/server.go
api.GET("/protocols", s.listProtocols)
func (s *Server) listProtocols(c *gin.Context) {
    collect := protocol.AllInfo()  // []ProtocolInfo
    forward := target.AllInfo()
    c.JSON(200, gin.H{
        "collect": collect,
        "forward": forward,
    })
}
```

**验收**:
```bash
curl -H "Authorization: Bearer $T" http://localhost:8082/api/v1/protocols
# 返回 3 个 collect + 2 个 forward,每个含 config_schema(JSON Schema)
```

---

### 3.4 Vue 协议下拉框改为启动时拉取
**优先级**:⭐⭐⭐ | **预估**:1 天

**改造**:
```typescript
// web/src/api/protocols.ts
import { apiGet } from './client'

export interface ProtocolInfo {
  name: string
  version: string
  role: 'collect' | 'forward'
  config_schema: object
  capabilities: {
    supports_batch_read: boolean
    supports_batch_write: boolean
    supports_heartbeat: boolean
  }
}

export async function listProtocols() {
  return apiGet<{ collect: ProtocolInfo[]; forward: ProtocolInfo[] }>('/api/v1/protocols')
}
```

```typescript
// web/src/pages/channels/List.vue
import { listProtocols } from '@/api/protocols'

const protocols = ref<ProtocolInfo[]>([])
const collectTypes = computed(() => protocols.value.filter(p => p.role === 'collect').map(p => p.name))
const forwardTypes = computed(() => protocols.value.filter(p => p.role === 'forward').map(p => p.name))

onMounted(async () => {
  const { collect, forward } = await listProtocols()
  protocols.value = [...collect, ...forward]
})
```

**验收**:
- 删 `List.vue:58-59` 的 `TYPES_COLLECT` / `TYPES_FORWARD` 硬编码
- 新协议上线后,刷新页面下拉框自动出现
- 协议特定 UI(如 S7 的 STRING_LAYOUT_OPTIONS)按 `capabilities.supports_heartbeat` 等能力字段条件渲染

---

### 3.5 Fake 协议验收用例
**优先级**:⭐⭐⭐ | **预估**:1 天

**目的**:用 fake 协议验证"零改动"承诺真的成立。

**步骤**:
1. 新建 `internal/infra/protocol/fake/plugin.go`,实现 `ProtocolPlugin` 接口(Mock 行为,BatchRead 返回固定值)
2. 在 `_all.go` 加 `import _ ".../fake"`
3. **不动 handler / 前端 / main.go**
4. 启动服务,验证:
   - `/api/v1/protocols` 返回 fake
   - Web 端协议下拉框自动出现 "fake"
   - 添加一个 fake 协议的 Channel,数据按 Mock 返回

**如果发现还需要改代码**:
- 改的地方就是 Phase 2 没做完的
- 继续完善直到 fake 协议真正零改动

---

### 3.6 协议能力声明(Capability)
**优先级**:⭐⭐ | **预估**:0.5 天

**目的**:让前端和 handler 根据协议能力做条件渲染 / 校验,而不是按协议名硬编码。

```go
// 协议能力(声明式)
type ProtocolCapabilities struct {
    SupportsBatchRead  bool
    SupportsBatchWrite bool
    SupportsHeartbeat  bool
    RequiresRackSlot   bool  // S7
    RequiresSlaveID    bool  // Modbus
    // ...
}

// ProtocolPlugin 接口扩展
type ProtocolPlugin interface {
    // ...已有方法
    Capabilities() ProtocolCapabilities
}
```

**应用场景**:
- Web 端:根据 `capabilities.requires_rack_slot` 决定是否显示 rack/slot 输入框
- handler 校验:BatchWrite 失败时检查 `capabilities.supports_batch_write`,不支持则降级为单点写
- 文档生成:自动产出协议能力矩阵

---

## 4. 主题 B:handler 错误体系统一(Phase 1.2 收尾)

> **对应 Roadmap**:Phase 1.2 错误体系(`/web/errors.go` 已实现,只差 handler 全量切换)。

### 4.1 handler 全部走 `WriteError`
**优先级**:⭐⭐⭐ | **预估**:1.5 天

**问题**:`handlers_crud.go` 大量 `c.JSON(400, gin.H{"error": "..."})`,新错误体系接不上。

**改造**:
```go
// 改前
c.JSON(http.StatusBadRequest, gin.H{"error": "channel_biz_id/role/type/address 必填"})

// 改后
web.WriteError(c, web.ErrInvalidParam.With("channel_biz_id/role/type/address 必填"))
```

**范围**:
- `handlers_crud.go`:全部 13 个 handler(createXxx / updateXxx / deleteXxx)
- `auth.go`:登录失败
- `process_control.go`:进程控制错误
- `snapshot.go`:快照获取错误
- 其他 handler 散落点

**关键约束**(保留兼容):
- `handleStoreErr` 维持 `gin.H{"error": msg}`(前端 client.ts 依赖)
- 业务校验错误(400/404/409)统一用 `WriteError` 走 `{code, msg}` 格式
- 过渡期:新格式 + 旧格式共存(用 X-Error-Migration 头标记)

**验收**:
- 所有 400/404/409 业务错误走 `WriteError`
- 前端 `client.ts` 加 `code` 字段处理(集中处理,handler 不感知)
- 文档:`web/src/api/errors.ts` 列出所有 Code

---

### 4.2 i18n 预留(不实现,只留接口)
**优先级**:⭐ | **预估**:0.5 天

```go
type APIError struct {
    Code       string
    Message    string  // 当前中文文案
    HTTPStatus int
    // I18nKey string  // 预留,未来接 vue-i18n / go-i18n
}

// 未来做 i18n 时,只替换 Message 字段,Code 不动
```

---

## 5. 主题 C:边界清理(Phase 3)

> **对应 Roadmap**:Phase 3 全部。
> **原则**:业务规则从 handler / SQL 抽到 service / domain。

### 5.1 `Value.AsGoNative()`
**优先级**:⭐⭐ | **预估**:0.5 天

```go
// internal/infra/protocol/value.go
func (v Value) AsGoNative() any {
    switch v.DataType {
    case "bool":     return v.Bool
    case "int8", "int16", "int32", "int64":
        return v.Int64
    case "uint8", "uint16", "uint32", "uint64":
        return v.Uint64
    case "float32", "float64":
        return v.Float64
    case "string":   return v.String
    case "bytes":    return v.Bytes
    default:         return nil
    }
}
```

**应用**:
```go
// app/collect/workers.go (改造后)
queue.Enqueue(target.PointValue{
    Value: values[pointBizID].AsGoNative(),  // 应用层只调方法
})
```

---

### 5.2 `Store.WithTx` 事务抽象
**优先级**:⭐⭐ | **预估**:0.5 天

```go
// internal/interface/web/sqlstore.go (或 store/transaction.go)
type Tx interface {
    Exec(query string, args ...any) (sql.Result, error)
    QueryRow(query string, args ...any) *sql.Row
    Query(query string, args ...any) (*sql.Rows, error)
}

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

**应用场景**:
- `DeleteSource` 删 source + 删 points(已有 CASCADE,但显式事务更可控)
- `DeleteDevice` 删 device + 清理关联 source/target/point
- `SetTargetPoints` 批量替换点位关联

---

### 5.3 校验下沉到 service 层
**优先级**:⭐⭐ | **预估**:1.5 天

**目标**:`handlers_crud.go` 退化为"绑定 + 翻译"。

```go
// service/validation.go(新建)
func ValidateSourceForCreate(src *platform.Source, ch *platform.Channel) error {
    if ch == nil {
        return web.ErrChannelNotFound
    }
    if ch.Role != "collect" {
        return web.ErrChannelRoleMismatch.With("source 只能引用 collect 通道")
    }
    return nil
}

func ValidatePointForCreate(p *platform.Point, src *platform.Source) error {
    if src == nil {
        return web.ErrSourceNotFound
    }
    if !isKnownDataType(p.DataType) {
        return web.ErrInvalidDataType
    }
    return nil
}
```

```go
// handlers_crud.go 改造后
func (h *Handler) CreateSource(c *gin.Context) {
    var req web.SourceDTO
    if err := c.ShouldBindJSON(&req); err != nil {
        web.WriteError(c, web.ErrInvalidParam.With(err.Error()))
        return
    }
    
    src := req.ToEntity()  // DTO → entity
    ch, _ := h.store.GetChannelByBizID(src.ChannelBizID)
    if err := service.ValidateSourceForCreate(src, ch); err != nil {
        web.WriteError(c, web.WrapAPIError(err))
        return
    }
    
    if err := h.store.CreateSource(ctx, src); err != nil {
        web.WriteError(c, web.WrapAPIError(err))
        return
    }
    c.JSON(http.StatusCreated, src.ToDTO())
}
```

**保留**:
- `assertChannelExists` / `assertChannelRole` 等 helper 不动(handler 内部仍用,但 service 层用更纯函数)
- 过渡期:handler 调用 service,service 暂时内联在 handler 包,后续可独立成包

---

### 5.4 实体 / DTO 分离(选做)
**优先级**:⭐ | **预估**:2 天

**只在以下情况做**:
- 需要给另一个端(CLI / gRPC)暴露 API
- 或当前 DTO 与实体混杂已造成维护问题

**否则跳过**,等真有需求再做。

---

## 6. 主题 D:健壮性深化(Phase 4 之后)

> **对应 Roadmap**:Phase 4 部分 + 新增。

### 6.1 启动时配置 fail-fast 校验
**优先级**:⭐⭐ | **预估**:1 天

**问题**:目前 `config/validate.go` 已有完整业务校验,但**调用时机**待确认是否启动时全量校验。

**验收**:
- 故意写错 config(缺字段、类型错、跨表引用错)→ 启动立即报错
- 错误信息含 channel_biz_id / device_biz_id / 路径下标
- 错误聚合(`*ValidationError` 已支持),不首个失败就返回

**实现**(已大部分就位,需确认):
```go
// main.go 启动时
cfg, err := config.LoadWithRetry(*configDir, 5*time.Second)
if err != nil { os.Exit(1) }
if err := config.Validate(cfg); err != nil {
    logging.Error(logging.EventConfigReloadFail).Err(err).Send()
    os.Exit(1)
}
```

---

### 6.2 OpenTelemetry 接入(选做)
**优先级**:⭐ | **预估**:3 天

**目的**:把 `trace_id`(Plan §10.2)从日志字段升级为完整分布式追踪。

**范围**:
- otel SDK 初始化
- HTTP middleware:`otelgin`
- DB:`otelsql`
- 协议层:Read/Write span
- 导出:OTLP(对接 Jaeger / Tempo / SigNoz)

**MVP 阶段不阻塞**;有运维需求再做。

---

### 6.3 SLO 告警规则
**优先级**:⭐ | **预估**:1.5 天(文档为主)

**目的**:把 §2.5 监控指标的告警阈值写成 Prometheus 规则或 Grafana 仪表盘 JSON。

**交付**:
- `deploy/prometheus/rules.yaml`:`plcbridge_collect_p99 > 300ms for 5m` 等
- `deploy/grafana/dashboard.json`:核心面板(采集 P99 / 回写 P99 / 队列长度 / 池利用率)
- 文档:`md/SLO.md`:阈值依据 + 告警分级

---

### 6.4 水平扩展(多 plcbridge 实例)
**优先级**:⭐ | **预估**:取决于场景

**触发条件**:
- 单实例 300 PLC / 10 万点位达到上限(Plan §2.1.2 高密度档)
- 单台服务器 CPU / 内存 / 网络达到瓶颈

**设计要点**:
- plcbridge 实例无状态化(配置从同一共享目录拉)
- Target 投递用 `device_biz_id % N` 哈希分片,避免重复投递
- Channel 复用需要协调(etcd / Redis 分布式锁 / 或干脆每实例独立连接)
- 回写路由:外部系统按 device_biz_id 哈希到目标实例

**MVP 不动**;超过高密度档时单独立项。

---

## 7. 主题 E:可观测性深化

### 7.1 协议特定指标
**优先级**:⭐ | **预估**:1 天

**目的**:不同协议的运行特征差异大(OPC UA 有 Session 续期、ModbusTCP 有 RTT),通用指标掩盖问题。

**候选指标**:
- `plcbridge_s7_connect_duration_seconds{rack, slot}`
- `plcbridge_opcua_session_renew_total{channel_biz_id}`
- `plcbridge_modbus_request_duration_seconds{func_code}`
- `plcbridge_mqtt_publish_duration_seconds{topic}`

**实现**:`internal/metrics/metrics.go` 加 HistogramVec,各协议 driver 内部埋点。

---

### 7.2 仪表盘 JSON
**优先级**:⭐ | **预估**:1.5 天

**交付**:
- `deploy/grafana/dashboard-overview.json`:总览(在线率、P99、队列)
- `deploy/grafana/dashboard-channel.json`:单 channel 详情
- `deploy/grafana/dashboard-write.json`:回写链路

---

## 8. 主题 F:文档与可发现性

### 8.1 协议插件开发指南
**优先级**:⭐⭐ | **预估**:1 天

**目的**:让第三方(或个人 6 个月后)能照着文档加新协议。

**交付**:`md/dev/plugin-guide.md`:
- `ProtocolPlugin` / `ProtocolChannel` 接口说明
- 完整示例(S7 简化版)
- 契约测试如何写
- 注册到 `_all.go` 的步骤
- 加新协议后端到端测试流程

---

### 8.2 架构图更新
**优先级**:⭐ | **预估**:0.5 天

**目的**:Plan.md §5.2 数据流图是 MVP 时的状态,Plan2 演进后架构图需更新。

**交付**:`md/architecture.md`:
- 4 个 worker 池 + ChannelRegistry + Router 数据流
- 协议 / Target 注册表与 handler / 前端的查询关系
- 配置热加载链路
- 写链路(外部 → API → writePool → PLC)

---

### 8.3 公开 API 文档
**优先级**:⭐ | **预估**:1 天

**目的**:`/api/v1/*` 接口契约(Plan §8)已有,但没有机器可读的 OpenAPI。

**交付**:
- `api/openapi.yaml`:从 handler 注释生成(或手写)
- `web/src/api/types.ts`:从前端类型自动生成(或反向)
- Swagger UI(可选)

---

## 9. 风险与回滚

| 风险 | 影响 | 缓解 |
|---|---|---|
| handler 错误体系切换破坏前端 | 客户端 `error` 字段读不到 | 过渡期 `gin.H{"error": msg, "code": "...", "msg": "..."}` 三字段共存;前端按阶段切 |
| `_all.go` 隐式注册让调试变难 | 看不到协议从哪来 | 启动日志打印 `Registered protocols: [s7, modbus_tcp, opcua]`,`Registered targets: [mqtt, influxdb]` |
| Fake 协议验收发现"零改动"做不到 | 暴露 Phase 2 漏点 | 接受继续收尾;不掩盖 |
| 业务规则下沉改动面大 | 影响其他开发 | 独立分支 + feature flag;先 service 层内联,后续独立包 |
| `WithTx` 引入破坏现有 SQL | 性能 / 死锁 | 谨慎评估,先用在真正需要事务的路径(级联删除),单 SQL 保持原样 |
| OpenTelemetry 性能开销 | 请求延迟增加 | 采样率默认 10%;核心路径 100% |

---

## 10. 关联文档

- **`md/Plan.md`**:MVP 方案(MVP 已交付)
- **`md/MVP_CHECKLIST.md`**:MVP 实施核对清单(6/6 通过)
- **`md/Roadmap.md`**:原 4 阶段路线图(**本文档已重写并对齐现状,Roadmap.md 后续可作废**)
- **`md/Plan2.md`**:本文档(演进方案)
- **`md/architecture.md`**:架构图(主题 F.2 产出)
- **`md/dev/plugin-guide.md`**:协议插件开发指南(主题 F.1 产出)
- **`md/SLO.md`**:SLO 告警规则(主题 D.3 产出)
- **`md/IndustryBenchmark.md`**:性能基准依据(Plan §2 引用)

---

## 11. 实施顺序(推荐)

```
Step 1 (主题 A 收尾)        — 1 周
  ├─ 3.1 handler 协议白名单查表化 (0.5d)
  ├─ 3.2 main.go 隐式注册 (0.5d)
  ├─ 3.3 GET /api/v1/protocols (1d)
  ├─ 3.4 Vue 下拉框动态化 (1d)
  ├─ 3.5 Fake 协议验收 (1d)        ← Phase 2 收尾标志
  └─ 3.6 协议能力声明 (0.5d)

Step 2 (主题 B)              — 0.5 周
  └─ 4.1 handler 全部走 WriteError (1.5d)

Step 3 (主题 C)              — 1 周(可与 Step 2 并行)
  ├─ 5.1 Value.AsGoNative (0.5d)
  ├─ 5.2 Store.WithTx (0.5d)
  └─ 5.3 校验下沉 service (1.5d)

Step 4 (主题 D + E)          — 按需
  ├─ 6.1 启动 fail-fast 校验 (1d)
  ├─ 6.3 SLO 告警规则 (1.5d)
  └─ 7.2 仪表盘 JSON (1.5d)

Step 5 (主题 F)              — 文档集中产出
  ├─ 8.1 插件开发指南 (1d)
  ├─ 8.2 架构图 (0.5d)
  └─ 8.3 OpenAPI (1d)
```

**关键里程碑**:
- **Step 1 结束**(主题 A 完成)→ 加新协议 / Target 真正零改动,Phase 2 全部收尾
- **Step 2 结束** → 错误响应全统一,i18n 准备就绪
- **Step 3 结束** → 业务规则与持久化解耦,handler 纯翻译
- **Step 4 结束** → 生产可观测性 + 告警闭环
- **Step 5 结束** → 文档齐备,新成员可自助加协议

---

**最后更新**:2026-10-18
**负责**:plcbridge 后端
**当前阶段**:MVP 已交付(2026-10-18),Plan2 Step 1 待启动
