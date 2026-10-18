import { apiGet, apiPost } from './client'

export interface Dashboard {
  channels: { total: number; collect: number; forward: number }
  devices: { total: number; enabled: number }
  sources: { total: number; collect_on: number; write_on: number }
  points: { total: number; collect_on: number; write_on: number }
  targets: { total: number }
  tokens_active: number
  storage_bytes: number
  collect_last_hour?: { total_errors: number }
  targets_last_hour?: { total_errors: number }
  write_last_hour?: { total_errors: number }
}

export function fetchDashboard() { return apiGet<Dashboard>('/dashboard') }
export function exportSnapshot() {
  return apiPost<{ ok: boolean; path: string; version: number }>('/snapshot', {})
}
