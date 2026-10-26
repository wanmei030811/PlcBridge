// 批量写入 API（Plan2 新增功能）。
//
// 数据流：前端 → POST /api/v1/write/batch
// 后端 writePool → 补偿机制（事务模式失败回滚原值）
import { apiPost } from './client'

export interface BatchWriteItem {
  point_biz_id: string
  value: unknown
  value_type: string  // 对应 Point.data_type
}

export interface BatchWriteReq {
  trace_id?: string
  device_biz_id: string
  transactional?: boolean  // true = 失败补偿回滚
  timeout_ms?: number
  writes: BatchWriteItem[]
}

export interface BatchWriteResult {
  point_biz_id: string
  result: 'success' | 'fail'
  error?: string
}

export interface BatchWriteResp {
  trace_id: string
  result: 'success' | 'fail' | 'partial'
  compensation_triggered: boolean
  writes: BatchWriteResult[]
  duration_ms: number
}

export async function batchWrite(data: BatchWriteReq): Promise<BatchWriteResp> {
  return apiPost<BatchWriteResp>('/write/batch', data)
}
