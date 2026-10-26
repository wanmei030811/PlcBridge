// 备份 / 恢复 API（Plan2 新增功能）。
import { apiGet, apiPost } from './client'

export interface BackupMeta {
  filename: string
  created_at: string
  db_records: number
  size: number
}

export interface BackupListResp {
  items: Array<{
    filename: string
    created_at: string
    db_records?: number
    size: number
  }>
}

export interface BackupInfoResp {
  latest: BackupMeta | null
}

export interface ExportResp {
  ok: boolean
  filename: string
  size: number
  records: number
}

export interface RestoreResp {
  ok: boolean
  db_restored: boolean
  config_restored: boolean
  requires_web_refresh: boolean
  message: string
}

export interface ListResp<T> {
  items: T[]
}

export function fetchBackupInfo() {
  return apiGet<BackupInfoResp>('/config/backup/info')
}

export function listBackups() {
  return apiGet<BackupListResp>('/config/backups')
}

export function exportBackup() {
  return apiPost<ExportResp>('/config/backup')
}

export async function restoreBackup(file: File): Promise<RestoreResp> {
  const form = new FormData()
  form.append('file', file)
  const resp = await fetch('/api/v1/config/restore', {
    method: 'POST',
    credentials: 'include',
    body: form,
  })
  const data = await resp.json()
  if (!resp.ok) {
    const err: any = new Error(data.msg || data.error || '恢复失败')
    err.response = { data }
    throw err
  }
  return data
}

export function downloadBackup(filename: string) {
  window.open(`/api/v1/config/backup/${encodeURIComponent(filename)}`, '_blank')
}
