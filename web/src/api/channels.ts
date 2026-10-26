import { apiGet, apiPost, apiPut, apiDelete, ListResp } from './client'

export interface Channel {
  id: number
  channel_biz_id: string
  role: 'collect' | 'forward'
  type: string
  address: string
  config: string
  heartbeat_address: string
  heartbeat_max_interval_ms: number
  heartbeat_enabled: boolean
  heartbeat_debounce_ms: number
  queue_size: number
  timeout_ms: number
  created_at: string
  updated_at: string
}

export function listChannels() {
  return apiGet<ListResp<Channel>>('/channels')
}
export function getChannel(id: number) {
  return apiGet<Channel>(`/channels/${id}`)
}
export function createChannel(c: Partial<Channel>) {
  return apiPost<Channel>('/channels', c)
}
export function updateChannel(id: number, c: Partial<Channel>) {
  return apiPut<Channel>(`/channels/${id}`, c)
}
export function deleteChannel(id: number) {
  return apiDelete<{ ok: boolean }>(`/channels/${id}`)
}

// === 通道 runtime 状态（连接/错误/最近活动） ===

export interface ChannelRuntime {
  connected: boolean
  last_active_ms: number  // 0 = 从未使用
  last_error: string      // '' = 无最近错误
  protocol?: string       // PR1: 's7' / 'modbus_tcp' / 'opcua' / 'mqtt' / 'influxdb' / 'sink'
  stats?: Record<string, any>  // PR1: 协议特定诊断数据（connection_state / broker_url / rack / 等）
}

export interface ChannelsRuntimeResp {
  ok: boolean
  error: string
  fetched_at_ms: number
  age_ms: number
  channels: Record<string, ChannelRuntime>
}

export function getChannelsRuntime() {
  // suppressGlobalError：通道列表每 1s 轮询，失败时页面保留旧状态静默重试。
  return apiGet<ChannelsRuntimeResp>('/plcbridge/channels/runtime', { suppressGlobalError: true })
}
