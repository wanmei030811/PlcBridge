// Target 历史趋势 API（Plan3 3.5）。
import { apiGet } from './client'

export interface HistoryPoint {
  /** Unix 毫秒时间戳 */
  t: number
  /** 点位值 */
  v: any
}

export interface HistoryResp {
  target_biz_id: string
  measurement: string
  field: string
  duration: string
  count: number
  points: HistoryPoint[]
}

export interface HistoryQuery {
  measurement: string
  field?: string
  duration?: string
  limit?: number
}

export function fetchTargetHistory(targetBizId: string, q: HistoryQuery) {
  const params = new URLSearchParams()
  params.set('measurement', q.measurement)
  if (q.field) params.set('field', q.field)
  if (q.duration) params.set('duration', q.duration)
  if (q.limit) params.set('limit', String(q.limit))
  // suppressGlobalError：趋势图自动刷新（默认 10s）会持续请求，
  // InfluxDB 挂掉时每 10s 弹一次错误会把页面淹没。组件内部已有错误提示区，
  // 由它负责展示"查询历史数据失败"。
  return apiGet<HistoryResp>(`/targets/${encodeURIComponent(targetBizId)}/history?${params.toString()}`, {
    suppressGlobalError: true
  })
}
