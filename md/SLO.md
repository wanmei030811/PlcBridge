# plcbridge SLO 与告警规则（Plan2 主题 D 6.3）

本文档定义 plcbridge 生产环境的 SLO 指标、告警阈值与分级。
配套 Prometheus 规则：[`deploy/prometheus/rules.yaml`](../deploy/prometheus/rules.yaml)。

## 1. 核心 SLO 指标

| 指标 | SLO 目标 | 告警阈值 | 等级 |
|---|---|---|---|
| 采集 P99 延迟 | < 100ms | > 300ms / 5min | warning |
| 采集错误率 | < 0.1% | > 5% / 5min | critical |
| 回写成功率 | > 99.9% | < 90% / 5min | critical |
| Target 投递成功率 | > 99% | 队列满 / 丢消息 / 1min | critical |
| 池拒绝率 | 0 | > 0.1/s / 5min | warning |
| 配置热加载成功率 | 100% | 失败 1 次 | critical |

## 2. 阈值依据

### 2.1 采集 P99 = 300ms
- Plan §2.5 监控指标定义 P99 < 100ms 为绿区
- 300ms 是黄区上限（CPU 抖动 / 网络轻微拥塞）
- 5min 持续 → 已是稳态问题，不是抖动
- 红色区在 1s（worker 池可能已堆积）

### 2.2 队列容量 80% 告警
- 队列默认 1000
- 80% = 800 触发警告：业务侧有 20% 缓冲应对突发
- 持续打满意味着下游 Target 消费慢或宕机
- 红色区 = 队列满 + 出现 dropped 计数

### 2.3 回写失败率 10%
- 写链路是外部主动操作，失败 = 用户指令丢失
- 10% 是黄色区（网络偶发）
- 超过 50% 持续 1min 应该考虑 PLC 离线

## 3. 告警分级

| 等级 | 含义 | 响应时间 | 通知渠道 |
|---|---|---|---|
| **critical** | 业务功能受损 | 15min | 钉钉/电话 |
| **warning** | 性能/容量告警 | 4h | 邮件/IM |

## 4. 抑制规则（推荐）

```yaml
# 减少告警风暴：当 PLC 离线时，与其相关的 collect/write 告警不重复触发
inhibit_rules:
  - source_matchers: [alertname="PlcbridgeWriteFailRateHigh"]
    target_matchers: [component="write"]
    equal: [device_biz_id]
  - source_matchers: [alertname="PlcbridgeCollectErrorRateHigh"]
    target_matchers: [component="collect"]
    equal: [device_biz_id]
```

## 5. 关联文档

- [`md/Plan.md` §2.5](../md/Plan.md) 监控指标原始定义
- [`md/Plan2.md`](../md/Plan2.md) 主题 D 演进背景
- [`deploy/prometheus/rules.yaml`](../deploy/prometheus/rules.yaml) Prometheus 规则
- [`deploy/grafana/dashboard-overview.json`](../deploy/grafana/dashboard-overview.json) 仪表盘（Plan2 7.2 产出）
