// 配置变更历史与回滚 API（Plan3 3.3）。
import { apiGet, apiPost, apiDelete } from './client'

export interface SnapshotItem {
  id: number
  description: string
  created_by: string
  record_count: number
  is_auto: boolean
  created_at: number
  created_at_str: string
  size_bytes: number
}

export interface SnapshotDetail extends SnapshotItem {
  config_json: string
}

export interface SnapshotDiffEntry {
  entity: string
  key: string
  field: string
  old_value: string
  new_value: string
  change: 'added' | 'removed' | 'modified'
}

export interface SnapshotDiffResp {
  snapshot_id: number
  diffs: SnapshotDiffEntry[]
  count: number
}

export interface RollbackResp {
  rollback: boolean
  snapshot_id: number
  new_snapshot: number
  message: string
  /** 后端是否已导出 config.json 触发 plcbridge 热加载。
   *  false = 数据库已回滚，但主进程还在跑旧配置，需要手动重载。 */
  reload?: boolean
}

export function listSnapshots() {
  return apiGet<{ items: SnapshotItem[] }>('/config/snapshots')
}

export function getSnapshot(id: number) {
  return apiGet<SnapshotDetail>(`/config/snapshots/${id}`)
}

export function createSnapshot(description?: string) {
  return apiPost<{ id: number; description: string; record_count: number }>(
    '/config/snapshots',
    { description: description || '' }
  )
}

export function deleteSnapshot(id: number) {
  return apiDelete<{ ok: boolean; id: number }>(`/config/snapshots/${id}`)
}

export function diffSnapshot(id: number) {
  return apiGet<SnapshotDiffResp>(`/config/snapshots/${id}/diff`)
}

export function rollbackSnapshot(id: number) {
  return apiPost<RollbackResp>(`/config/snapshots/${id}/rollback`)
}
