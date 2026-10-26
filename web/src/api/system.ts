// plcbridge 主进程 + web 子进程的实时监控端点。
//
// 数据来源：
//   - GET /api/v1/health               → web 子进程健康/PID/uptime/RSS/Goroutine
//   - GET /api/v1/plcbridge/metrics/summary → 主进程 Prometheus 聚合摘要
//
// 这两个端点用于 Dashboard.vue 每 3s 轮询。
import { apiGet, apiPost } from './client'

export interface WebProcess {
  pid: number
  started_at_ms: number
  uptime_ms: number
  rss_bytes: number
  num_goroutine: number
  go_version: string
  num_cpu: number
}

export interface WebHealth {
  status: 'ok' | 'degraded'
  db: string
  process: WebProcess
}

// /api/v1/plcbridge/metrics/summary 返回结构（plcbridge 进程维度）
export interface PlcbridgeSummary {
  ok: boolean
  error?: string
  fetched_at_ms: number
  age_ms: number
  config_version: number
  collect_total: { success: number; fail: number }
  write_total: { success: number; fail: number }
  target_sent_total: number
  target_queue: { total: number; by_target: Record<string, number> }
  pool_running: { total: number; by_pool: Record<string, number> }
  collect_p95_ms: number
  collect_avg_ms: number
}

export function fetchWebHealth() { return apiGet<WebHealth>('/health', { suppressGlobalError: true }) }

/**
 * plcbridge 主进程指标摘要。
 *
 * suppressGlobalError：仪表盘每 1s 轮询，失败时调用方只更新 liveError 文案
 * （页面已有错误展示区），不需要再弹一次全局提示。
 */
export function fetchPlcbridgeSummary() {
  return apiGet<PlcbridgeSummary>('/plcbridge/metrics/summary', { suppressGlobalError: true })
}

// ============ 进程控制（仪表盘启停面板） ============

export type ProcessAction =
  | 'reload_config'
  | 'export_config'
  | 'stop_plcbridge'
  | 'start_plcbridge'
  | 'restart_plcbridge-web'
  | 'start_supervisor'
  | 'stop_supervisor'

export interface ProcessControlResp {
  ok: boolean
  action: string
  message: string
  triggered: boolean  // supervisor 是否在线；false 表示指令可能不会被执行
}

export interface SupervisorState {
  status: string
  supervisor_pid: number
  web_pid: number
  core_pid: number
  message: string
  updated_at: string
}

export interface SupervisorStatusResp {
  supervisor_running: boolean
  runtime_dir: string
  state?: SupervisorState
}

export function postProcessControl(action: ProcessAction, reason = '') {
  return apiPost<ProcessControlResp>('/process/control', { action, reason })
}
export function fetchSupervisorStatus() {
  // supervisor 状态每 3s 轮询，失败时静默置空（页面显示"未运行"），不弹错误。
  return apiGet<SupervisorStatusResp>('/process/supervisor', { suppressGlobalError: true })
}