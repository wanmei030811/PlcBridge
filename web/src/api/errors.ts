/**
 * 后端 API 错误码集中表（与 internal/interface/web/errors.go 的 APIError 镜像）。
 *
 * Phase 1.2 (Plan2 §4.1) 过渡期：
 *   - 业务校验错误：返回 {code, msg}（通过 WriteError 统一）
 *   - handleStoreErr 内部错误：仍返回旧 {error: msg} 格式
 *
 * 前端使用：
 *   - 通用提示：client.ts interceptor 已经从 data.error / data.msg 取文案，无需感知
 *   - 业务特殊处理：根据 code 判断（例如 channel_not_found 时刷新列表、invalid_credentials 时清空密码）
 *   - 未来 i18n：把中文 msg 替换为 i18n key，code 不动
 */

export const ApiErrorCode = {
  // 通用
  InvalidParam: 'invalid_param',
  Unauthorized: 'unauthorized',
  Forbidden: 'forbidden',
  NotFound: 'not_found',
  Conflict: 'conflict',
  FKConstraint: 'fk_constraint',
  InternalError: 'internal_error',
  PlcbridgeUnavailable: 'plcbridge_unavailable',

  // Channel
  ChannelNotFound: 'channel_not_found',
  ChannelRoleMismatch: 'channel_role_mismatch',
  ChannelConflict: 'channel_conflict',
  ChannelConnectFail: 'channel_connect_fail',

  // Device
  DeviceNotFound: 'device_not_found',
  DeviceConflict: 'device_conflict',
  DeviceDisabled: 'device_disabled',

  // Source
  SourceNotFound: 'source_not_found',
  SourceConflict: 'source_conflict',

  // Point
  PointNotFound: 'point_not_found',
  PointConflict: 'point_conflict',

  // Target
  TargetNotFound: 'target_not_found',
  TargetConflict: 'target_conflict',

  // Auth
  InvalidCredentials: 'invalid_credentials',
  SessionExpired: 'session_expired'
} as const

export type ApiErrorCodeType = (typeof ApiErrorCode)[keyof typeof ApiErrorCode]

/**
 * 从 axios error 中抽取 code 字段。
 *
 * 注意：过渡期只有 WriteError 路径会带 code，handleStoreErr 旧格式没有 code。
 * 业务侧根据 code 决策时需兼容 undefined（回退到 status 判断）。
 */
export function getApiErrorCode(err: any): string | undefined {
  return err?.response?.data?.code
}

/**
 * 从 axios error 中抽取 i18n key（Plan2 主题 B 4.2 预留）。
 *
 * 当前未启用 i18n：后端 I18nKey 字段恒空，前端拿到 undefined 即 fallback 到 msg。
 * 启用时：前端 i18n 资源按 "errors.<code>" 组织（与 Go 端 WithI18nKey 调用约定一致），
 *         client.ts interceptor 优先按 i18n_key 翻译，找不到才回退到 msg。
 */
export function getApiI18nKey(err: any): string | undefined {
  return err?.response?.data?.i18n_key
}
