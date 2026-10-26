// 实时点位值 API（Plan3 3.2）。
//
// 后端是 plcbridge 主进程（:8082）上的内存缓存，web 端做跨进程代理，
// 因此这里的 :id 传的是 **业务 ID**（source_biz_id / device_biz_id），不是行号。
import { apiGet } from './client'

/**
 * Source 采集健康状态。
 *
 * - `ok`：最近一次采集成功，值新鲜
 * - `error`：最近一次采集失败，`values` 里是**旧值**（多旧看 `last_success_ms`）
 * - `never`：从未成功采集过，`values` 为空
 */
export type HealthStatus = 'ok' | 'error' | 'never'

export interface SourceHealth {
  source_biz_id: string
  status: HealthStatus
  /** 最近一次采集成功的时间（Unix ms），从未成功为 0 */
  last_success_ms: number
  /** 最近一次采集失败的时间（Unix ms），从未失败为 0 */
  last_error_ms: number
  last_error?: string
  /** 连续失败次数，一次成功即归零 */
  consecutive_errors: number
  /** 该 Source 的采集间隔（ms），用于前端推导 stale 阈值 */
  interval_ms: number
}

/** 单点位的最新采集值。 */
export interface PointValue {
  point_biz_id: string
  address: string
  value: any
  /** 协议层质量标记（如 good / bad） */
  quality: string
  /** 采集时间（Unix ms） */
  timestamp: number
}

/** key = point_biz_id */
export type PointValues = Record<string, PointValue>

export interface SourceValuesResp {
  source_biz_id: string
  values: PointValues
  /** 本 Source 最新点位值时间戳（无值时为 0） */
  updated_at: number
  health: SourceHealth
}

export interface DeviceSourceValues {
  source_biz_id: string
  values: PointValues
  updated_at: number
  health: SourceHealth
}

export interface DeviceValuesResp {
  device_biz_id: string
  /** key = source_biz_id */
  sources: Record<string, DeviceSourceValues>
  /** 全部 Source 中最新的点位值时间戳 */
  updated_at: number
  /** 设备整体健康度：取最差的那个 Source */
  health: SourceHealth
  source_count: number
  error_count: number
  /** 该 device_biz_id 在当前配置中是否存在（false = 不存在或无 Source） */
  exists: boolean
}

/** 单个 Source 的实时值。 */
export function fetchSourceValues(sourceBizId: string) {
  return apiGet<SourceValuesResp>(`/sources/${encodeURIComponent(sourceBizId)}/values`)
}

/**
 * 设备级实时值聚合（一次拿全设备，避免前端并发 N 次单 Source 请求）。
 *
 * suppressGlobalError：设备详情页每 2s 轮询一次，失败时调用方静默降级
 * （保留旧值 + 置位"主进程不可达"），不弹全局错误。
 */
export function fetchDeviceValues(deviceBizId: string) {
  return apiGet<DeviceValuesResp>(`/devices/${encodeURIComponent(deviceBizId)}/values`, {
    suppressGlobalError: true
  })
}

// === 陈旧度判定 ===

/** 超过该倍数仍未刷新即视为过期（相对采集间隔）。 */
const STALE_INTERVAL_MULTIPLIER = 3
/** 陈旧阈值下限（ms），避免采集间隔很短时过于敏感。 */
const STALE_MIN_MS = 3000

/**
 * 计算某条实时值的陈旧度。
 *
 * 优先级：
 * 1. 健康状态非 `ok` → 非新鲜（这是最权威的信号，PLC 掉线时缓存仍留着旧值）
 * 2. 否则按时间戳与采集间隔推断（后端 health 可能滞后于本次轮询）
 */
export function isValueStale(
  pv: PointValue | undefined,
  health: SourceHealth | undefined,
  nowMs: number = Date.now(),
): boolean {
  if (!pv) return false
  if (health && health.status !== 'ok') return true
  if (!health) return false
  const interval = health.interval_ms || 1000
  const threshold = Math.max(interval * STALE_INTERVAL_MULTIPLIER, STALE_MIN_MS)
  return nowMs - pv.timestamp > threshold
}

/** 人类可读的相对时间，如 "3s 前" / "12m 前"。 */
export function formatAge(timestampMs: number, nowMs: number = Date.now()): string {
  if (!timestampMs) return '—'
  const diff = Math.max(0, nowMs - timestampMs)
  const s = Math.floor(diff / 1000)
  if (s < 60) return `${s}s 前`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m 前`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h 前`
  return `${Math.floor(h / 24)}d 前`
}

/**
 * 格式化点位值：数字保留合理精度，字符串/布尔直接显示，长文本截断。
 *
 * 空字符串特殊处理 → `""`：
 * PLC 的 string 类型点位常返回空串（PLC 侧确实没有内容）。
 * 直接渲染成空白会被误读成"没采到值"，加上引号能明确区分
 * 「采到了，值是空串」和「没采到」。
 */
export function formatPointValue(v: any): string {
  if (v === null || v === undefined) return '—'
  if (typeof v === 'boolean') return v ? 'true' : 'false'
  if (typeof v === 'number') {
    return Number.isInteger(v) ? String(v) : v.toFixed(3)
  }
  const s = typeof v === 'string' ? v : JSON.stringify(v)
  if (s === undefined) return '—'
  if (s === '') return '""'
  return s.length > 24 ? s.slice(0, 24) + '…' : s
}