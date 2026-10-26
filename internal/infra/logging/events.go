package logging

// Event 枚举 —— plcbridge 所有日志事件的强类型常量。
//
// 来源：Plan.md §10.1.2 事件枚举表（17 个业务事件 + 1 个心跳特有事件）。
// 原则（§10.1.2 注）：
//   - event 字段为强枚举，新增事件必须在本文件登记
//   - 不允许出现未登记的 event 值（业务代码必须用这里的常量）
//   - 测试可以加新值（test_ 前缀），但发布前必须移到本表
//
// 用法：
//
//	logging.Info(EventPollBatchEnd).Str("source", bizID).Msg("采集批次完成")
//
// 字段约定（§10.1.1）：
//   - 以下事件的"必填附加字段"在每个常量注释里说明
//   - 共有字段：trace_id / batch_id（按需）/ device_biz_id（按需）/ point_biz_id（按需）
const (
	// === 通用（5 个）===

	// EventServiceStart 服务启动。
	// 附加字段：version
	EventServiceStart = "service_start"

	// EventServiceStop 服务停止。
	EventServiceStop = "service_stop"

	// EventConfigReload 配置热重载。
	// 附加字段：version
	EventConfigReload = "config_reload"

	// EventConfigReloadFail 配置重载失败。
	// 附加字段：error
	EventConfigReloadFail = "config_reload_fail"

	// EventConfigExport config 快照已导出。
	// 附加字段：path
	EventConfigExport = "config_export"

	// === 采集端 collectPool（6 个）===

	// EventDeviceConnectOK 设备连接成功。
	// 附加字段：device_biz_id, source（worker 池名）
	EventDeviceConnectOK = "device_connect_ok"

	// EventDeviceConnectFail 设备连接失败。
	// 附加字段：device_biz_id, source, error
	EventDeviceConnectFail = "device_connect_fail"

	// EventDeviceDisconnect 检测到设备断连。
	// 附加字段：device_biz_id
	EventDeviceDisconnect = "device_disconnect"

	// EventDeviceTimeout 设备响应超时。
	// 附加字段：device_biz_id, duration_ms
	EventDeviceTimeout = "device_timeout"

	// EventDeviceResponseError Modbus/西门子异常码。
	// 附加字段：device_biz_id, error
	EventDeviceResponseError = "device_response_error"

	// EventPollBatchStart 批次轮询开始。
	// 附加字段：device_biz_id, batch_id
	EventPollBatchStart = "poll_batch_start"

	// EventPollBatchEnd 批次轮询结束。
	// 附加字段：device_biz_id, batch_id, point_count, error_count, duration_ms
	EventPollBatchEnd = "poll_batch_end"

	// EventPointReadFail 单点位读失败。
	// 附加字段：device_biz_id, point_biz_id, error
	EventPointReadFail = "point_read_fail"

	// === 回写端 writePool（5 个）===

	// EventWriteConnectOK PLC 写连接成功（writePool 借到连接）。
	// 附加字段：device_biz_id
	EventWriteConnectOK = "write_plc_connect_ok"

	// EventWriteConnectFail PLC 写连接失败。
	// 附加字段：device_biz_id, error
	EventWriteConnectFail = "write_plc_connect_fail"

	// EventWriteOK 回写成功。
	// 附加字段：device_biz_id, point_biz_id, duration_ms
	EventWriteOK = "write_ok"

	// EventWriteFail 回写失败。
	// 附加字段：device_biz_id, point_biz_id, error
	EventWriteFail = "write_fail"

	// EventWritePartial 部分点位回写成功。
	// 附加字段：device_biz_id, success_count, fail_count
	EventWritePartial = "write_partial"

	// === 转发端 forwardPool（3 个）===

	// EventForwardSendOK 转发成功。
	// 附加字段：target_biz_id, batch_size
	EventForwardSendOK = "forward_send_ok"

	// EventForwardSendFail 转发失败（投递失败，详见 §6.4.3）。
	// 附加字段：target_biz_id, batch_size, error
	EventForwardSendFail = "forward_send_fail"

	// EventTargetQueueFull Target 队列满（触发丢弃，详见 §6.1）。
	// 附加字段：target_biz_id, queue_size
	EventTargetQueueFull = "target_queue_full"

	// === 心跳（特有，4 个）—— Plan.md §6.3 ===

	// EventHeartbeatStart heartbeat worker 启动。
	// 附加字段：channel_biz_id
	EventHeartbeatStart = "heartbeat_start"

	// EventHeartbeatWriteFail heartbeat 写 PLC 失败。
	// 附加字段：channel_biz_id, address, error
	EventHeartbeatWriteFail = "heartbeat_write_fail"

	// EventHeartbeatFlip heartbeat 翻转（DEBUG 级别）。
	// 附加字段：channel_biz_id, value
	EventHeartbeatFlip = "heartbeat_flip"

	// EventHeartbeatChannelFail heartbeat 取 channel 失败。
	// 附加字段：channel_biz_id, error
	EventHeartbeatChannelFail = "heartbeat_channel_fail"

	// === Channel 连接管理（4 个）—— 内部 infrastructure ===

	// EventChannelEstablished channel 已建立。
	// 附加字段：channel_biz_id, type, address
	EventChannelEstablished = "channel_established"

	// EventChannelClosed channel 已断开。
	// 附加字段：channel_biz_id, reason
	EventChannelClosed = "channel_closed"

	// EventChannelConnectFail channel Connect 失败（连接阶段）。
	// 附加字段：channel_biz_id, type, address, error
	EventChannelConnectFail = "channel_connect_fail"

	// EventChannelInvalidate channel 已 Invalidate。
	// 附加字段：channel_biz_id
	EventChannelInvalidate = "channel_invalidate"

	// === Snapshot 探活/重连（3 个）—— ChannelRegistry 内部 ===

	// EventSnapshotProbeFail snapshot probe 失败。
	// 附加字段：channel_biz_id, error
	EventSnapshotProbeFail = "snapshot_probe_fail"

	// EventSnapshotReconnect snapshot 重连尝试。
	// 附加字段：channel_biz_id
	EventSnapshotReconnect = "snapshot_reconnect"

	// EventSnapshotReconnectOK snapshot 重连成功。
	// 附加字段：channel_biz_id
	EventSnapshotReconnectOK = "snapshot_reconnect_ok"

	// EventSnapshotReconnectFail snapshot 重连仍失败。
	// 附加字段：channel_biz_id, error
	EventSnapshotReconnectFail = "snapshot_reconnect_fail"

	// === Target 连接管理（4 个）—— forward.Target 内部 ===

	// EventTargetConnectOK Target 已连接。
	// 附加字段：target_biz_id
	EventTargetConnectOK = "target_connect_ok"

	// EventTargetConnectFail Target 连接失败。
	// 附加字段：target_biz_id, error
	EventTargetConnectFail = "target_connect_fail"

	// EventTargetDisconnect Target 断开。
	// 附加字段：target_biz_id
	EventTargetDisconnect = "target_disconnect"

	// EventTargetCloseTimeout Target 关闭超时。
	// 附加字段：target_biz_id
	EventTargetCloseTimeout = "target_close_timeout"

	// === Panic 防御（2 个）—— 任何 driver bug ===

	// EventStatsPanic ProtocolChannel.Stats panic。
	// 附加字段：channel_biz_id, recover
	EventStatsPanic = "stats_panic"

	// EventTargetStatsPanic TargetClient.Stats panic。
	// 附加字段：target_biz_id, recover
	EventTargetStatsPanic = "target_stats_panic"
)

// AllEvents 返回所有业务事件常量（用于单元测试枚举完整性）。
// 测试可以遍历此列表，反向 grep 代码中是否每个常量都至少被使用一次。
func AllEvents() []string {
	return []string{
		// 通用
		EventServiceStart, EventServiceStop, EventConfigReload, EventConfigReloadFail, EventConfigExport,
		// 采集
		EventDeviceConnectOK, EventDeviceConnectFail, EventDeviceDisconnect, EventDeviceTimeout,
		EventDeviceResponseError, EventPollBatchStart, EventPollBatchEnd, EventPointReadFail,
		// 回写
		EventWriteConnectOK, EventWriteConnectFail, EventWriteOK, EventWriteFail, EventWritePartial,
		// 转发
		EventForwardSendOK, EventForwardSendFail, EventTargetQueueFull,
		// 心跳
		EventHeartbeatStart, EventHeartbeatWriteFail, EventHeartbeatFlip, EventHeartbeatChannelFail,
		// Channel 连接管理
		EventChannelEstablished, EventChannelClosed, EventChannelConnectFail, EventChannelInvalidate,
		// Snapshot
		EventSnapshotProbeFail, EventSnapshotReconnect, EventSnapshotReconnectOK, EventSnapshotReconnectFail,
		// Target
		EventTargetConnectOK, EventTargetConnectFail, EventTargetDisconnect, EventTargetCloseTimeout,
		// Panic
		EventStatsPanic, EventTargetStatsPanic,
	}
}
