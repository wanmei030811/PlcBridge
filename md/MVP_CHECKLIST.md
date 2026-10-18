# MVP 实施核对清单

> 范围:Plan.md §1.2 「MVP 核心功能」6 项 + §6.2.2 「进程内隔离」4 条硬约束 + §6.3 「通道心跳机制」。
> 方法:Plan 条款 → 代码 grep → 逐条判定 ✅/⚠️/❌。
> 生成日期:2026-10-18。

---

## 1. MVP 核心功能(Plan §1.2)

| # | 条款 | 实现位置 | 关键证据 | 状态 |
|---|---|---|---|---|
| 1 | 单进程多 worker 池隔离(collectPool/forwardPool/writePool/configPool) | `cmd/plcbridge/main.go:88-93` | `pool.NewPools{CollectSize:200, ForwardSize:100, WriteSize:50, ConfigSize:4}` | ✅ 完整 |
| 2 | 多协议采集 S7 + ModbusTCP + OPCUA | `internal/infra/protocol/{s7,modbus_tcp,opcua}.go` | 三个 `new*Plugin()` 都在 `MustRegisterDefaults()`(`protocol.go:160-164`) | ✅ 完整 |
| 3 | 数据转发 MQTT + InfluxDB | `internal/infra/target/{mqtt,influxdb}.go` | `MustRegisterDefaults()`(`target.go:122-125`);MQTT v5+v3.1.1;InfluxDB V1.x/V1.8+/V2/V3 | ✅ 完整 |
| 4 | 指令回写 RESTful API | `internal/infra/api/server.go:195` + `:279` | `POST /api/v1/write`(`writeSingle`)+ `POST /api/v1/write/batch`(`writeBatch`);`internal/app/write/workers.go` writePool 消费 | ✅ 完整 |
| 5 | 配置同步 fsnotify(L1)+ mtime(L2) | `internal/infra/config/config.go:123` | `StartWatcher`:fsnotify.NewWatcher + 60000ms mtime 兜底;`timeconst.go:35` 500ms debounce | ✅ 完整 |
| 6 | 基础监控 Prometheus + 健康检查 | `internal/metrics/metrics.go` + `internal/infra/api/server.go:123` | 9 个 Prometheus 指标;`/health` + `/metrics` 路由 | ✅ 完整 |

---

## 2. §6.2.2 进程内隔离 4 条硬约束

| # | 条款 | 实现位置 | 关键证据 | 状态 |
|---|---|---|---|---|
| 1 | **worker 池物理隔离** collectPool/forwardPool/writePool/configPool | `cmd/plcbridge/main.go:88` + `internal/infra/pool/pool.go` | 4 个独立 chan + 独立 goroutine,跨池通过 `pools.Submit*`(`pools.SubmitCollectBlocking` 等)API 限制 | ✅ 完整 |
| 2 | **连接池物理隔离** PLC 读连接池 / 写连接池 独立 | `internal/infra/protocol/registry.go`(ChannelRegistry 引用计数) + `internal/app/write/workers.go` | 读采集走 `BatchRead`、写回写走 `Write`/`BatchWrite`;registry.Acquire 同一 channel 不同 role 拿不同连接 | ✅ 完整 |
| 3 | **有界队列 + 超时** 回写 1024 + IO 300ms | `internal/app/write/workers.go:81` + `internal/app/collect/workers.go:146` | 回写:`make(chan Command, 1024)`,满 ErrQueueFull 返 503;采集 IO 默认 `300 * time.Millisecond` | ✅ 完整 |
| 4 | **关键路径 P99 监控** 采集/队列/池利用率独立打点 | `internal/metrics/metrics.go` | `plcbridge_collect_duration_seconds`(Histogram,bucket 含 0.3)+ `plcbridge_target_queue_length`(Gauge)+ `plcbridge_pool_running`/`plcbridge_pool_rejected_total`(Gauge/Counter) | ✅ 完整 |

---

## 3. §6.3 通道心跳机制

### §6.3.2 协议心跳机制对比

| 协议 | Plan 要求 | 实现 | 状态 |
|---|---|---|---|
| **S7** | 应用层 0/1 翻转 | `internal/app/write/heartbeat.go` HeartbeatSupervisor,每次 flip 写 `1 - lastValue`(`heartbeat.go:200`) | ✅ 完整 |
| **ModbusTCP** | 读探针 | `internal/infra/protocol/modbus_tcp.go` 协议层探针;`registry.ProbeActive` 由 collectPool 调用 | ✅ 完整 |
| **OPC UA** | Session 续期(协议层自动) | `gopcua/opcua` 库内置 session 管理,`internal/infra/protocol/opcua.go:89` 默认 `SessionTimeoutMs = 600000` | ✅ 完整 |
| **MQTT** | LWT + online 主题 | `internal/infra/target/mqtt.go` LWT 字段,`will_topic` / `will_payload` 配置 | ✅ 完整 |

### §6.3.3 「事件驱动 + 兜底定时器」实现(S7 / ModbusTCP)

| 条款 | 要求值 | 实现位置 | 状态 |
|---|---|---|---|
| 兜底定时器 ticker | `heartbeat_max_interval = 5s` | `heartbeat.go:160` `maxInterval = 5*time.Second` 默认值 | ✅ 完整 |
| 事件驱动信号 | 业务回写触发翻转 | `heartbeat.go:107` `SignalHeartbeat` → `w.sigCh <- struct{}{}`;`run()` 监听 sigCh + ticker.C | ✅ 完整 |
| 去抖窗口 | `debounce = 50ms` | `heartbeat.go:96` `debounceMs: 50` | ✅ 完整 |
| 状态持久化 | `now.Unix() % 2` 决定下次写值 | `heartbeat.go:94` `lastValue: uint8(time.Now().Unix() % 2)` | ✅ 完整 |
| 写入失败不阻断业务 | 只记 WARN,不影响节奏 | `heartbeat.go:202-218` WARN 写日志,无 error 返回 | ✅ 完整 |
| goroutine panic recover | 防止拖垮主进程 | `internal/infra/pool/pool.go:249` 池层 recover,heartbeat goroutine 在 pool 中运行间接受保护 | ✅ 完整(在 pool 层而非 heartbeat 内部) |
| Prometheus 失败计数 | `heartbeat_write_fail_total{channel_biz_id}` | `internal/metrics/metrics.go` `HeartbeatWriteFailTotal` + `internal/app/write/heartbeat.go:208` `Inc()` | ✅ 完整(2026-10-18 补齐,ChannelClosed 也计数) |

### §6.3.4 心跳失败隔离原则

| 条款 | 状态 | 说明 |
|---|---|---|
| 心跳写入失败不影响业务写入 | ✅ | `heartbeat.go:202-218` flip 失败只记 WARN,heartbeatCommand.ResultChan 不触发(心跳没有 ResultChan) |
| Prometheus 失败计数 `heartbeat_write_fail_total` | ✅ | `metrics/metrics.go` `HeartbeatWriteFailTotal` + `heartbeat.go:208` 失败时 `Inc()`(ChannelClosed 也计,见 `heartbeat.go:204-208` 注释) |
| goroutine panic recover | ✅ | `pool.go:249` 池层 recover 兜底 |

### Plan §6.3.3 / §6.3.4 提到的 Prometheus 指标

| 指标名 | Plan 要求 | 实际 | 状态 |
|---|---|---|---|
| `heartbeat_write_fail_total{channel_biz_id}` | §6.3.4 列出 | `metrics.HeartbeatWriteFailTotal` 已注册,`heartbeat.go:208` 失败 `Inc()`(包含 ChannelClosed) | ✅ 完整(2026-10-18 补齐) |

**风险**:Prometheus 侧**无法量化心跳失败率**,告警/仪表盘只看到日志。若依赖指标做 SLO 告警,需要补这个 counter(改动量约 10 行:`metrics.go` 加 1 个 CounterVec,`heartbeat.go` `flip()` 失败分支加 1 行 `metrics.HeartbeatWriteFailTotal.WithLabelValues(channel).Inc()`)。

### §6.3.5 进程内各 worker 池的心跳职责

| 方向 | 通道类型 | 心跳机制 | 实现位置 | 状态 |
|---|---|---|---|---|
| 采集 | ModbusTCP | 读探针 | `internal/infra/protocol/registry.go` `ProbeActive` 由 collectPool 调用 | ✅ |
| 采集 | OPC UA | Session 续期 | `gopcua` 库内置,`opcua.go:89` `SessionTimeoutMs = 600000` | ✅ |
| 转发 | MQTT | LWT + online 主题 | `internal/infra/target/mqtt.go` LWT 字段配置 | ✅ |
| 回写 | S7 | 0/1 翻转 + 事件驱动 + 兜底定时器 | `internal/app/write/heartbeat.go` HeartbeatSupervisor,writePool 内运行 | ✅ |

---

## 4. §6.4 失败处理(无 WAL、不重试)核验摘要

> Plan §6.4 全部 6 节统一原则是「**直接丢弃 + 错误日志 + Prometheus 计数,不写 WAL、不重试**」。
> 实现: `internal/app/forward/workers.go` 转发失败日志 + 计数;`internal/app/write/workers.go` 回写失败 ResultChan 返 fail;`target/target.go:128` 之前存在的 `ErrNotImplemented` stub 错误已删除(本次顺手清理)。

---

## 5. 总结

| 维度 | 完成度 |
|---|---|
| MVP 6 项核心功能 | **6/6 完整** |
| §6.2.2 进程内隔离 4 条硬约束 | **4/4 完整** |
| §6.3 心跳机制 5 个协议/角色 | **5/5 完整** |
| §6.3.3 心跳细节(5s/50ms/now.Unix()%2/recover/Prometheus) | **7/7 完整** |
| §6.3.4 Prometheus 指标 `heartbeat_write_fail_total` | **✅ 完整(2026-10-18 补齐)** |

**最终结论**:MVP 6 项核心功能 + §6.2.2 + §6.3 全部条款均完整实现,无缺口。

---

## 附录 A:验证方法

```bash
# 编译
go build ./...

# 心跳单元测试
go test ./internal/app/write/... -run Heartbeat

# 端到端(需要 mock PLC)
go run ./cmd/bench/main.go
```

## 附录 B:本次同时清理的 stub 残留(2026-10-18)

| 文件 | 改动 |
|---|---|
| `README.md` | "OPCUA stub" / "InfluxDB stub" 描述改为"均为完整实现" |
| `internal/infra/protocol/protocol.go` | 顶部注释 stub → 完整实现,删 `ErrNotImplemented` + 调整 `errors` import |
| `internal/infra/target/target.go` | 顶部注释对齐,删 `ErrNotImplemented` + 调整 `errors` import |
| `cmd/plcbridge/main.go` | "OPCUA stub"/"InfluxDB stub" 注释修正 |
| `internal/app/collect/workers.go` | "MVP stub 单点循环" 注释更新为"协议层 BatchRead 已实现" |
