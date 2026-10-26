# plcbridge 架构图（Plan2 主题 F 8.2）

本文档描述 plcbridge 在 Plan2 演进后的整体架构。MVP 阶段的旧架构图见 [Plan.md §5.2](../md/Plan.md)。

## 1. 进程拓扑

```
┌──────────────────┐       ┌─────────────────────┐
│  plcbridge-web   │ HTTP  │     plcbridge        │
│   :8080 (UI/API) │◀─────▶│  :8082 (REST)        │
│  Vue 3 + Element │       │  :9091 (Prometheus)  │
└──────────────────┘       └─────────────────────┘
        │                            │
        │ 读写 SQLite (config)       │ 读 SQLite (config snapshot)
        │ 写 Prometheus               │ 写 Prometheus
        ▼                            ▼
   config.json ◀──── fsnotify ────▶ memory config
```

两个进程通过 `config.json` 文件 + SQLite 解耦，**不直接通信**：
- `plcbridge-web` 接受用户配置 → 写 SQLite → 导出 config.json
- `plcbridge` 监听 config.json 变化 → 重新加载配置 → 触发 worker 池重建

## 2. 4 个 worker 池（物理隔离）

```
                     ┌──────────────┐
                     │  configPool  │  4 goroutines
                     │  监听 config │
                     └──────┬───────┘
                            │ (file change)
                            ▼
   ┌────────────┐  ┌────────────┐  ┌────────────┐  ┌────────────┐
   │ collectPool│  │ forwardPool│  │  writePool │  │heartbeatPool│
   │   200      │  │   100      │  │     50     │  │   (n=1/chan)│
   └──────┬─────┘  └──────┬─────┘  └──────┬─────┘  └──────┬──────┘
          │               ▲               ▲                │
          │               │               │                │
          ▼               │               │                ▼
   ProtocolChannel    Target.Queue    ProtocolChannel   ProtocolChannel
   (S7/Modbus/OPC UA) (MQTT/Influx)  (S7/Modbus/OPC UA) (S7 心跳翻转)
```

- 每个池**独立 goroutine + 独立 context**
- ChannelRegistry 在所有池之间共享（引用计数 + Acquire/Release）
- 4 个池通过 chan 通信，避免共享内存

## 3. 协议 / Target 插件化（Plan2 主题 A）

```
                ┌─────────────────────────────────────────┐
                │  protocol  /  target 注册表              │
                │  (init() 自注册，零配置)                 │
                │  - s7          (collect)               │
                │  - modbus_tcp  (collect, modbus alias) │
                │  - opcua       (collect)               │
                │  - mqtt        (forward)               │
                │  - influxdb    (forward)               │
                │  - fake        (collect, 仅测试)        │
                └────────────────┬────────────────────────┘
                                 │
                                 ▼
   ┌─────────────────────────────────────────────────────────────┐
   │  handler / 前端 / config validator 全部查表                  │
   │  - isKnownChannelType(role, type) → protocol.Get(type)      │
   │  - GET /api/v1/protocols → protocol.AllInfo()               │
   │  - config.Validate → protocol.CanonicalizeChannelType()     │
   └─────────────────────────────────────────────────────────────┘
```

**加新协议 = 新建子包 + 1 行 import _，零改动其他任何文件。**

## 4. 数据流（采集 → 转发 → 回写）

### 4.1 采集 (Collect)

```
   ProtocolChannel.BatchRead(points)
            │
            ▼
   map[point_biz_id]protocol.Value   (含 DataType 字段)
            │
            ▼
   v.AsGoNative()  (Plan2 5.1：协议层提供，应用层无 switch)
            │
            ▼
   target.PointValue{Value: any, ...}   (any = bool/int64/uint64/float64/string/[]byte)
            │
            ▼
   Target.Queue.Enqueue()  (有界队列，drop-oldest)
            │
            ▼
   forwardPool worker → TargetClient.Send()
```

### 4.2 回写 (Write)

```
   HTTP POST /api/v1/write  (body: {device_biz_id, point_biz_id, value})
            │
            ▼
   api.server → writePool.Enqueue(writeOp)
            │
            ▼
   writePool worker → ProtocolChannel.Write()
            │
            ▼
   metrics.WriteSuccessTotal.Inc() / WriteFailTotal.Inc()
```

### 4.3 心跳 (Heartbeat)

```
   heartbeatPool (per-channel) ──周期翻转──▶ ProtocolChannel.Write(heartbeat_addr, !current)
                                                       │
                                                       ▼
                                              LastActiveAt 更新
                                              LastError 更新（如果失败）
                                                       │
                                                       ▼
                                              ChannelRegistry.ProbeActive
                                              (lazy 探活 + handler death 旁路)
```

## 5. 错误体系（Plan2 主题 B）

```
   handler 显式错误           Store 错误
       │                        │
       ▼                        ▼
   *web.APIError  ───┐    sqlstore.ErrNotFound / ErrConflict / FK
                      │            │
                      ▼            ▼
                 web.WriteError(c, err)
                      │
                      ▼
              {code, msg, [i18n_key]}  (Plan2 4.1/4.2/4.3 全部统一)
                      │
                      ▼
              axios interceptor: data.error || data.msg || err.message
```

## 6. 配置热加载（Plan §6.6）

```
   config.json (on disk)
        │
        │  fsnotify (L1) + mtime 轮询 (L2, 60s) + 500ms debounce
        ▼
   config.Store.TryLoad() → atomic.Pointer[platform.Config]
        │
        │  Validate() 失败 → log error + 不更新内存
        │  Validate() 成功 → 替换 + gen++ → 触发 worker 重建
        ▼
   cfg.Version 变化 → supervisor.SpawnAll() (或增量 reconcile)
```

## 7. 关键不变式

1. **4 个 worker 池物理隔离**（Plan §6.2.2）：一个池阻塞不影响其他池
2. **ChannelRegistry 引用计数**：Acquire/Release 必须配对（defer 释放）
3. **Target 队列 drop-oldest**：满时丢旧数据保新（Plan §6.4.3）
4. **失败不重试、不写 WAL**：丢 + ERROR 日志 + Prometheus 计数（Plan §6.4）
5. **错误响应统一 `{code, msg}`**（Plan2 主题 B）：前端 axios interceptor 兼容旧格式
6. **协议 / Target 自注册**：plugin init() 由 Go runtime 在启动时调，main.go 无显式注册

## 8. 关联文档

- [Plan.md](../md/Plan.md) MVP 原始架构
- [Plan2.md](../md/Plan2.md) 演进背景
- [plugin-guide.md](./dev/plugin-guide.md) 加新协议步骤
- [SLO.md](./SLO.md) 监控告警阈值
- [openapi.md](./openapi.md) REST API 契约
