# PlcBridge

工业 PLC 数据采集与指令回写平台。基于 **Go + 单进程多 worker 池隔离** 架构。

> 详细产品方案：[md/Plan.md](md/Plan.md)

## MVP 核心特性

| # | 特性 | 说明 |
|---|---|---|
| 1 | **单进程多 worker 池隔离** | collectPool / forwardPool / writePool / configPool（§6.2.2）|
| 2 | 多协议采集 | S7 / ModbusTCP / OPCUA（均为完整实现）|
| 3 | 数据转发 | MQTT（v5 + v3.1.1 双协议）/ InfluxDB（支持 V1.x / V1.8+ / V2.x / V3.x）|
| 4 | 指令回写 | `POST /api/v1/write` 单点位 + `/api/v1/write/batch` 批量 |
| 5 | 配置同步 | `fsnotify`（L1）+ mtime 轮询（L2）|
| 6 | 基础监控 | `/health` + `/metrics`（Prometheus 格式）|

## 目录结构

```
plcbridge/
├── cmd/
│   ├── plcbridge/      # 主进程二进制（采集+转发+回写一体化）
│   └── plcbridge-web/  # Web 后台二进制（最小骨架）
├── internal/
│   ├── pool/           # 4 个 worker 池实现
│   ├── config/         # 配置加载（atomic.Pointer + fsnotify + mtime）
│   ├── protocol/       # 协议插件接口 + S7 / ModbusTCP / OPCUA 实现
│   ├── target/         # Target 插件接口 + MQTT / InfluxDB 实现
│   ├── collect/        # 采集 worker + 批量读策略 + 熔断
│   ├── forward/        # 转发 worker + 有界队列 + drop-oldest
│   ├── write/          # 回写 worker + 心跳
│   ├── api/            # HTTP API（/health / /metrics / /api/v1/write）
│   ├── auth/           # Bearer Token 鉴权
│   ├── log/            # 结构化日志（zerolog）
│   └── metrics/        # Prometheus 指标
├── configs/
│   └── config.example.json
├── scripts/
│   ├── install-systemd.sh
│   └── install-windows.ps1
├── go.mod
├── README.md
└── md/Plan.md
```

## 快速开始

```bash
# 编译
go build -o bin/plcbridge ./cmd/plcbridge
go build -o bin/plcbridge-web ./cmd/plcbridge-web

# 运行（需先准备 config.json）
./bin/plcbridge --config /etc/plcbridge/
./bin/plcbridge-web --config /etc/plcbridge/
```

## 跨平台

- Linux（生产）：systemd unit（参见 `scripts/install-systemd.sh`）
- Windows（开发）：NSSM 注册为 Windows Service（参见 `scripts/install-windows.ps1`）

## MQTT 协议版本

MQTT Target 同时支持 **MQTT v5** 和 **MQTT v3.1.1**，由 `channel.config.protocol_version` 字段控制：

```json
{
  "type": "mqtt",
  "address": "tcp://broker:1883",
  "config": {
    "protocol_version": "v5",        // 默认；mosquitto ≥2.1 / emqx 默认支持
    "client_id": "plcbridge-01",
    "qos": 1,
    "lwt_topic": "plcbridge/01/status",
    "lwt_payload": "offline",
    ...
  }
}
```

```json
{
  "type": "mqtt",
  "address": "tcp://legacy-broker:1883",
  "config": {
    "protocol_version": "v3.1.1",    // 兼容老 broker（仅 v3.x listener）
    "client_id": "plcbridge-legacy",
    ...
  }
}
```

| 取值 | 客户端库 | 适用场景 |
|---|---|---|
| `v5`（默认） | `eclipse/paho.golang` + `autopaho` | mosquitto ≥2.1 / emqx 默认；推荐 |
| `v3.1.1` | `eclipse/paho.mqtt.golang` | 旧 broker / 仅接受 v3.x 的 listener |

Eclipse Paho 团队故意将 v3.x 和 v5 拆为两个独立仓库，单一 client 库无法跨协议版本。
不同 MQTT forward channel 可独立设置 `protocol_version`，互不干扰。

集成测试（连本机 broker 真实收发）：

```bash
PLCB_INTEGRATION=1 go test -timeout 60s -run "TestMQTTTarget_Integration_RealBroker" ./internal/infra/target/...
# 会同时跑 v5 和 v3.1.1 两个子测试
```

调试探针 `cmd/mqttprobe` 也支持 `-protocol-version` 参数：

```bash
go run ./cmd/mqttprobe -broker tcp://127.0.0.1:1883 -protocol-version v3.1.1 -sub
go run ./cmd/mqttprobe -broker tcp://127.0.0.1:1883 -protocol-version v5
```
