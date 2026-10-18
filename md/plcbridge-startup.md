# plcbridge 启动备忘

## 启动方式（正确）

```powershell
cd F:\Project\PlcBridge

# 只启动 web，由 web 拉起 core 作为子进程
$root = (Get-Location).Path
$binDir = Join-Path $root 'bin'
$logDir = Join-Path $root 'logs'

$webProc = Start-Process -FilePath (Join-Path $binDir 'plcbridge-web.exe') `
    -ArgumentList '--config', '.\config', '--addr', ':8080' `
    -WorkingDirectory $root `
    -RedirectStandardOutput (Join-Path $logDir 'plcbridge-web.out.log') `
    -RedirectStandardError  (Join-Path $logDir 'plcbridge-web.err.log') `
    -PassThru -NoNewWindow
```

> 关键：**`--config .\config`**，不是 `--config .`
>
> 根目录 `F:\Project\PlcBridge\` 是 Go 源码仓库；运行时数据（配置 / HMAC / DB / 种子）全部在 `F:\Project\PlcBridge\config\` 下。

## 停止

```powershell
Get-Process -Name plcbridge,plcbridge-web -ErrorAction SilentlyContinue | Stop-Process -Force
```

## config dir 怎么指定

**核心：plcbridge / plcbridge-web 没有配置文件指定 config dir。config dir 由"代码 + 启动参数 + 环境变量"显式决定。**

### 三种显式方式（按优先级）

| 优先级 | 方式 | 适用场景 | 示例 |
|---|---|---|---|
| 1 | `-config` 命令行参数 | 临时调试、supervisor 启动 | `plcbridge.exe -config=F:\Project\PlcBridge\config` |
| 2 | `PLCBRIDGE_CONFIG` 环境变量 | 当前 shell 范围；supervisor 启动前预设 | `$env:PLCBRIDGE_CONFIG = "F:\Project\PlcBridge\config"` |
| 3 | 默认探测 | 无人显式指定时的降级 | Linux `/etc/plcbridge`；Windows `%PROGRAMDATA%\plcbridge` |

**优先级在代码里写死**（`cmd/plcbridge/main.go` 的 `defaultConfigDir()` 与 `cmd/plcbridge-web/main.go` 的 `defaultConfigDirWeb()`）：

```go
// 优先级链路（自上而下，命中即返回）
1. flag.String("config", ...)                // -config 参数
2. os.Getenv("PLCBRIDGE_CONFIG")             // 环境变量
3. os.Stat("/etc/plcbridge") == nil          // Linux 约定
4. os.Getenv("PROGRAMDATA") + "\\plcbridge"  // Windows 默认
   // 备选：硬编码 C:\ProgramData\plcbridge（PROGRAMDATA 未设置时）
```

### 关键约束（2026-10-12 修复 dual-dir 漂移 bug）

- **`-config` / `PLCBRIDGE_CONFIG` 指定的目录必须存在**，否则进程**直接 `os.Exit(1)` 拒启动**，不做兜底降级
- **不静默降级**：即使 PROGRAMDATA 里有完整 DB，指定 F 盘但 F 盘目录不存在，进程就拒绝启动
- **不存在 ≠ 自动创建**（web 是初始化器会 MkdirAll，但仅针对"首次安装"场景；明确指定 F 盘但 F 盘没建，**直接拒启动**让运维人工处理）

### 运维操作范式

**场景 1：项目级开发机（最常用 — 显式指向 F 盘 config）**

```powershell
# 方式 A：命令行参数（推荐，每次都明示）
F:\Project\PlcBridge\bin\plcbridge-web.exe -config=F:\Project\PlcBridge\config
F:\Project\PlcBridge\bin\plcbridge.exe     -config=F:\Project\PlcBridge\config

# 方式 B：环境变量（脚本中预先 export）
$env:PLCBRIDGE_CONFIG = "F:\Project\PlcBridge\config"
F:\Project\PlcBridge\bin\plcbridge-web.exe
```

**场景 2：supervisor 托管（看 `scripts/supervisor.ps1`）**

```powershell
# supervisor 第 145 / 170 行：已经写死 --config $ConfigDir
# $ConfigDir 默认 <Root>/config，可被 PLCBRIDGE_CONFIG_DIR env 覆盖
$env:PLCBRIDGE_CONFIG_DIR = "F:\Project\PlcBridge\config"
.\scripts\supervisor.ps1 -Action start
```

**场景 3：生产机器（裸 PROGRAMDATA，无项目目录）**

```powershell
# 直接裸跑，走默认降级链
plcbridge-web.exe
# → 没设 PLCBRIDGE_CONFIG → 找不到 /etc → 用 %PROGRAMDATA%\plcbridge
```

### 排查"我的 config 被谁读走了"？

```powershell
# 1. 看进程命令行
Get-Process plcbridge,plcbridge-web | ForEach-Object {
  $cmd = (Get-CimInstance Win32_Process -Filter "ProcessId=$($_.Id)").CommandLine
  Write-Host "PID=$($_.Id) $cmd"
}
#   期望输出形如:
#   PID=9164 "F:\Project\PlcBridge\bin\plcbridge.exe" -config=F:\Project\PlcBridge\config

# 2. 看启动日志第一条
Get-Content F:\Project\PlcBridge\logs\plcbridge.out.log -Tail 3
#   找 "config_dir":"..." 字段，这就是当前进程用的 config dir

# 3. 看 web 启动决策日志
Get-Content C:\ProgramData\plcbridge\web.password.log -Tail 5
#   找 "event":"BOOT_BEFORE_ENSURE"，里面 details.config_dir 字段
```

### 历史坑：dual-dir 漂移

- **症状**：今天上午（2026-10-12）出现的 bug —— `C:\ProgramData\plcbridge\data\plcbridge.db`（10/09 旧配置）和 `F:\Project\PlcBridge\config\data\plcbridge.db`（10/11 最新工作区）**两份 DB 漂移**，admin 密码 / channel 配置 / 设备 / 点位都不一致，登录后看到错位数据。
- **根因**：某些命令裸跑（不传 `-config` 也不设 env）默认落到 PROGRAMDATA，与 supervisor 显式指向 F 盘并存；同一台机器跑两套 config 目录。
- **修法**：现在所有进程**必须**显式 `-config` 或 `PLCBRIDGE_CONFIG`，不存在就拒启动，**绝不降级兜底**。



## 各文件的作用

| 文件 | 作用 | 能不能删 |
|---|---|---|
| `config\config.json` | 真正的运行时配置（通道、设备、点位） | 否 |
| `config\web.hmac` | session HMAC 签名密钥（持久化） | **否**（删了会换 key，旧 cookie 全部失效） |
| `config\web.bootstrap` | 首次启动时写出的初始明文密码（一次性提示） | 是（只用于首次登录） |
| `config\data\plcbridge.db` | SQLite（用户、配置历史） | 否 |
| `config\runtime\` | 运行时临时文件 | 否（删了下次启动会重建） |

## 常见坑

1. **`--config .` 写错路径** → 根目录会冒出 `config.json` / `web.hmac`，且读不到 `config\config.json`，导致空配置无采集。
2. **保留 `web.bootstrap`** → 下次启动 web 会再次走"首次初始化"流程，把空模板写回根目录或 config 目录。建议已登录后立刻删。
3. **同时手动起 plcbridge** → 跟 web 拉起的子进程抢 8082/9091 端口；只起 web，让它拉 core。

## 通道运行时状态（PR1 扩展）

**2026-10-12 升级**：通道列表的"状态"列（绿/黄/红）现在同时覆盖 **collect** 和 **forward** 通道；每个通道还暴露"协议特定诊断数据"。

### 通用字段（所有通道都有）

| 字段 | 含义 | 示例 |
|---|---|---|
| `connected` | 通道是否处于"已连接且无错误"状态 | `true` |
| `last_active_ms` | 最近一次成功操作 unix ms | `1762567800000` |
| `last_error` | 最近一次错误（空=无错） | `"dial tcp ... refused"` |
| `protocol` | 协议名 | `"s7"` / `"mqtt"` / `"influxdb"` |
| `stats` | 协议特定数据（key-value） | 见下表 |

### 协议特定 stats 字段

| 协议 | 关键字段 | 含义 |
|---|---|---|
| **s7** | `connection_state` | `"Connected"` / `"Disconnected"` |
| | `address` / `rack` / `slot` / `connect_type` | 静态配置 |
| | **`read_count` / `write_count` / `error_count`** | **PR2：累计操作计数** |
| | **`last_read_ok_ms` / `last_write_ok_ms` / `last_error_ms`** | **PR2：unix ms 时间戳** |
| | `last_probe_ok_ms` | 最近一次探活成功时间 |
| | `string_layout` | STRING 头字节数（4/2/1/0） |
| **modbus_tcp** | `connection_state` / `address` / `unit_id` | 同上 |
| | **`read_count` / `write_count` / `error_count`** | **PR2：累计计数** |
| | **`last_read_ok_ms` / `last_write_ok_ms` / `last_error_ms`** | **PR2：时间戳** |
| | `last_probe_ok_ms` | 最近一次探活成功时间 |
| **opcua** | `connection_state` | gopcua 内部 state（`Connected`/`Disconnected`/`Connecting`/`Reconnecting`） |
| | `endpoint` / `security_mode` / `security_policy` / `auth_type` | 静态配置 |
| | `subscription_active` / `subscription_count` | 订阅状态 |
| | `publish_interval_ms` | 订阅发布间隔 |
| | **`read_count` / `write_count` / `error_count`** | **PR2：累计计数** |
| | **`last_read_ok_ms` / `last_write_ok_ms` / `last_error_ms`** | **PR2：时间戳** |
| **mqtt** | `connection_state` / `protocol_version` | `v5` / `v3.1.1` |
| | `broker_url` / `topic` | 目标 broker + topic |
| | `queue_depth` | 暂未实现（paho 无公开 API，autopaho 内部管）|
| | **`sent_count` / `sent_points` / `error_count`** | **PR2：batch 数 / point 数 / 失败数** |
| | **`connect_count` / `last_connect_ms`** | **PR2：连接成功次数** |
| | **`health_check_count` / `last_health_check_ms`** | **PR2：探活计数** |
| | `last_send_ok_ms` / `last_error_ms` | 最近发送 / 失败时间 |
| **influxdb** | `connection_state` / `version` | `2.0` / `1.x` / `1.8-flux` / `3.x` |
| | `address` / `org` / `bucket` / `db` | 目标地址 + V2/V1 差异 |
| | **`sent_count` / `sent_points` / `error_count`** | **PR2：batch 数 / point 数 / 失败数** |
| | **`connect_count` / `last_connect_ms` / `health_check_count` / `last_health_check_ms`** | **PR2：连接/探活计数** |
| | `last_send_ok_ms` / `last_error_ms` | 最近发送 / 失败时间 |

### PR3：forward 通道启动时主动探测连接（2026-10-12）

**问题**：PR1+PR2 上线后，发现 forward 通道（mqtt / influxdb）在上游 PLC 离线时，runtime 永远显示 `connected=false` + `connect_count=0`，与"服务已起来"的实际状态不一致。

**根因**：forwarder 只在 `Send()` 被调用时才建立连接（MQTT 用 `started.Load()` 懒加载、InfluxDB `ensureClient()` 在首次 Send 里执行）。当 PLC 离线时：

- collect 队列空 → target 队列空
- `Send()` 从不被调用 → `ensureClient()` / `Connect()` 不跑
- `connect_count` 始终 0、`last_connect_ms` 始终 0、`IsConnected()` 反映的是 "曾否 connect 过" 而非 "现在能否连上"

**修复**（2026-10-12 下午）：

1. `internal/infra/target/target.go` — `TargetClient` 接口加 `ProbeConnect(ctx) error`（新方法）
2. `internal/infra/target/mqtt.go` — `mqttTarget.ProbeConnect()` 复用 `Connect()`，让 `connect_count` 计数正确
3. `internal/infra/target/influxdb.go` — V1/V2 的 `ProbeConnect()` 复用 `HealthCheck()`，触发 `ensureClient` + 真正去 `/ping`
4. `internal/app/forward/workers.go` — `runForwarder()` 启动后立即调 `ProbeConnect(5s timeout)`
   - 成功 → 记 `forwarder 启动时探测连接成功` 日志
   - 失败 → 记 WARN 日志（**不影响数据流**；首次 Send 时仍会重试）

**行为**：

- 上游 PLC 离线 + InfluxDB 服务在跑 → 通道 status 立即变 ON（探测后 5s 内）
- 上游 PLC 在线 + 正常数据流 → 行为完全等价（probe 一次 + 后续 Send 触发）
- broker / InfluxDB 不在 → 启动时 probe 失败，WARN 日志，但不影响 forwarder 跑；后续 Send 会按原有逻辑重连

**可观测性**：

```powershell
# 看启动时是否 probe 成功
Get-Content F:\Project\PlcBridge\logs\plcbridge.out.log | Select-String "forwarder 启动时|ProbeConnect"

# 期望：每条 forward 通道一行成功日志
# {"event":"forwarder 启动时探测连接成功","target":"InfluxDB", ...}
# {"event":"forwarder 启动时探测连接成功","target":"2", ...}   # MQTT 的 target_biz_id
```

### 排查"通道显示 OFF 但程序没报错"？

```powershell
# 1. 直接看 8082 端点（带 Bearer token）
$tok = (Get-Content F:\Project\PlcBridge\config\web.bootstrap -ErrorAction SilentlyContinue)
$headers = @{Authorization="Bearer $tok"}
Invoke-RestMethod -Uri "http://127.0.0.1:8082/api/v1/channels/runtime" -Headers $headers | ConvertTo-Json -Depth 5

#   期望看到形如：
#   {
#     "ok": true,
#     "fetched": "2026-10-12T...",
#     "channels": {
#       "s7_plc_main":      { "connected": true,  "last_error": "", "protocol": "s7", "stats": {"connection_state": "Connected", "rack": 0, "slot": 1, ...} },
#       "mqtt_broker_main": { "connected": true,  "last_error": "", "protocol": "mqtt", "stats": {"connection_state": "Connected", "broker_url": "tcp://broker:1883", "sent_count": 12345, ...} },
#       "influx_main":      { "connected": false, "last_error": "InfluxDB V1 返回 401: ...", "protocol": "influxdb", "stats": {"connection_state": "Disconnected", ...} }
#     }
#   }

# 2. 看具体 driver 日志（按 protocol 类型 grep）
Get-Content F:\Project\PlcBridge\logs\plcbridge.out.log -Tail 50 | Select-String "snapshot probe failed|snapshot reconnect|s7|influxdb|mqtt"
```

### 历史变化

- **2026-10-12 之前**：forward 通道（mqtt / influxdb）**没**纳入 runtime，前端显示 "—"（"转发通道暂无运行时状态"）。
- **2026-10-12 之后（PR1）**：forward 也纳入，key = `channel_biz_id`（不是 `target_biz_id`），多个 target 共享同一 channel 时取 OR 聚合。
- **2026-10-12 之后（PR2）**：每个 driver 暴露详细统计（read_count/write_count/error_count/last_*_ms），用于问题诊断和性能监控。
  - mqtt/influxdb 加 sent_points（区别于 sent_count 的 batch 数）+ connect_count + health_check_count
  - 所有 driver 共享同一 `Stats()` 形状（ProtocolChannel.Stats 契约）
- **2026-10-12 之后（PR3：forward 启动时主动探测）**：forwarder 启动立即 ProbeConnect，让 forward 通道在 PLC 离线时也能正确显示连接状态。详见上一节。
