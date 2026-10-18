import { apiGet, apiPost, apiPut, apiDelete, ListResp } from './client'

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
