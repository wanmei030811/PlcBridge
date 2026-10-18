// Package timeconst 提供跨包统一的时间常量（Phase 1.10）。
//
// 原则：
//   - 业务中"魔法时间"必须用这里的常量或域内 const
//   - 避免散落的 time.Second * 5 / time.Millisecond * 100 等
//   - 这里只放**跨包共享**的时间值；包内独占的（如 shutdown timeout）放包内 const
package timeconst

import "time"

// === 网络 / IO 超时 ===

// DefaultIOReadTimeout 单次 IO 读超时（PLC 读 / HTTP 读）。
const DefaultIOReadTimeout = 5 * time.Second

// DefaultProbeTimeout 健康探活超时（registry.Snapshot 探活 / ProbeActive）。
const DefaultProbeTimeout = 1500 * time.Millisecond

// DefaultReconnectTimeout 强制重连超时（registry.Snapshot 重连）。
const DefaultReconnectTimeout = 3 * time.Second

// DefaultChannelConnectTimeout channel 初次 Connect 超时。
const DefaultChannelConnectTimeout = 5 * time.Second

// === 重试 / 退避 ===

// AcquireRetryInterval Acquire 失败后重试间隔。
const AcquireRetryInterval = 1 * time.Second

// MaxAcquireWaitTime Acquire 等待最长时间。
const MaxAcquireWaitTime = 60 * time.Second

// === Watcher / Debounce ===

// ConfigReloadDebounce fsnotify 事件去抖（防止短时间内多次触发 reload）。
const ConfigReloadDebounce = 500 * time.Millisecond

// === Pool / Shutdown ===

// DefaultShutdownTimeout 单个 worker / 资源优雅停止超时。
const DefaultShutdownTimeout = 5 * time.Second

// PoolStopForceTimeout pool 强制停止（worker 卡死兜底）。
const PoolStopForceTimeout = 5 * time.Second

// === Scheduler ===

// DefaultCollectInterval 默认采集间隔。
const DefaultCollectInterval = 1 * time.Second

// MinCollectInterval 最小采集间隔（防止打爆 PLC）。
const MinCollectInterval = 100 * time.Millisecond

// === Probe / Snapshot ===

// ProbeIdleThreshold 多久没活动触发 ProbeActive。
const ProbeIdleThreshold = 10 * time.Second

// ReconnectThrottle 普通错误下重连节流。
const ReconnectThrottle = 500 * time.Millisecond

// ReconnectThrottleFast handler 死亡错误下重连节流。
const ReconnectThrottleFast = 200 * time.Millisecond

// FailedRetryInterval failed 通道重试 Connect 间隔。
const FailedRetryInterval = 100 * time.Millisecond

// ChannelDisconnectGrace channel 引用归零后延迟断连（避免抖动）。
const ChannelDisconnectGrace = 5 * time.Second

// === Watcher / Health ===

// HealthCheckInterval 健康检查主动周期（预留）。
const HealthCheckInterval = 3 * time.Second
