import { apiGet, apiPost, apiPut, apiDelete, ListResp } from './client'
import type { AxiosProgressEvent } from 'axios'

export interface Source {
  id: number
  source_biz_id: string
  device_id: number
  device_biz_id?: string
  channel_biz_id: string
  collect_enabled: boolean
  write_enabled: boolean
  collect_interval_ms: number
  timeout_ms: number
  points?: Point[]
  created_at: string
  updated_at: string
}

export interface Point {
  id: number
  point_biz_id: string
  source_id: number
  name: string
  address: string
  data_type: string
  collect_enabled: boolean
  write_enabled: boolean
  // 仅 data_type==="string" 时使用：S7 STRING layout 字符数 N（总字节 4+N），范围 1..55。
  // 非 string 类型忽略；后端返回 0 时前端不展示输入。
  string_length?: number
  created_at: string
  updated_at: string
}

export interface Device {
  id: number
  device_biz_id: string
  name: string
  enabled: boolean
  sources?: Source[]
  targets?: Target[]
  created_at: string
  updated_at: string
}

export interface Target {
  id: number
  target_biz_id: string
  device_id: number
  device_biz_id?: string
  forward_channel_biz_id: string
  topic: string
  format: string
  point_biz_ids?: string[]
  created_at: string
  updated_at: string
}

export function listDevices() { return apiGet<ListResp<Device>>('/devices') }
export function getDevice(id: number) { return apiGet<Device>(`/devices/${id}`) }
export function createDevice(d: Partial<Device>) { return apiPost<Device>('/devices', d) }
export function updateDevice(id: number, d: Partial<Device>) { return apiPut<Device>(`/devices/${id}`, d) }
export function deleteDevice(id: number) { return apiDelete<{ ok: boolean }>(`/devices/${id}`) }
export function listDeviceSources(deviceId: number) { return apiGet<ListResp<Source>>(`/devices/${deviceId}/sources`) }

export function createSource(s: Partial<Source>) { return apiPost<Source>('/sources', s) }
export function updateSource(id: number, s: Partial<Source>) { return apiPut<Source>(`/sources/${id}`, s) }
export function deleteSource(id: number) { return apiDelete<{ ok: boolean }>(`/sources/${id}`) }
export function listSourcePoints(sourceId: number) { return apiGet<ListResp<Point>>(`/sources/${sourceId}/points`) }

export function createPoint(p: Partial<Point>) { return apiPost<Point>('/points', p) }
export function updatePoint(id: number, p: Partial<Point>) { return apiPut<Point>(`/points/${id}`, p) }
export function deletePoint(id: number) { return apiDelete<{ ok: boolean }>(`/points/${id}`) }

// === CSV 导入/导出（Plan3 3.1） ===

export interface ImportResult {
  imported: number
  failed: number
  skipped: number
  errors: ImportError[]
}
export interface ImportError {
  row: number
  field: string
  message: string
}

/**
 * 导入点位 CSV。
 * @param sourceBizId 目标 Source 的 biz_id
 * @param csv CSV 文件或纯文本内容
 * @param onProgress 上传进度回调
 */
export function importPoints(sourceBizId: string, csv: File | string) {
  const isFile = csv instanceof File
  if (isFile) {
    const fd = new FormData()
    fd.append('file', csv)
    fd.append('source_biz_id', sourceBizId)
    return apiPost<ImportResult>('/points/import', fd, {
      headers: { 'Content-Type': 'multipart/form-data' }
    })
  }
  // 纯文本
  return apiPost<ImportResult>('/points/import', { source_biz_id: sourceBizId, text: csv })
}

/**
 * 导出点位 CSV（返回下载 URL）。
 * @param params 筛选参数：source_biz_id / device_biz_id / channel_biz_id 三选一
 */
export function exportPointsUrl(params: { source_biz_id?: string; device_biz_id?: string; channel_biz_id?: string }) {
  const base = '/api/v1'
  const q = new URLSearchParams()
  if (params.source_biz_id) q.set('source_biz_id', params.source_biz_id)
  if (params.device_biz_id) q.set('device_biz_id', params.device_biz_id)
  if (params.channel_biz_id) q.set('channel_biz_id', params.channel_biz_id)
  return `${base}/points/export?${q}`
}

/** 触发 CSV 文件下载（自动带 Cookie 鉴权）。 */
export async function downloadFile(url: string, filename: string) {
  const resp = await fetch(url, { credentials: 'include' })
  if (!resp.ok) throw new Error(`下载失败: ${resp.status}`)
  const blob = await resp.blob()
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = filename
  a.click()
  URL.revokeObjectURL(a.href)
}

// === 设备克隆（Plan3 3.7） ===

export interface CloneRequest {
  new_device_biz_id: string
  new_device_name?: string
  biz_id_prefix?: string
}
export interface CloneResult {
  device: Device
  cloned: { sources: number; points: number; targets: number }
}

/** 克隆设备（深拷贝 Source/Point/Target）。 */
export function cloneDevice(id: number, req: CloneRequest) {
  return apiPost<CloneResult>(`/devices/${id}/clone`, req)
}
