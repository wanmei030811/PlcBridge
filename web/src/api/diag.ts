// Target 诊断 API（Plan3 3.4）。
//
// 数据来自 plcbridge 主进程（:8082）内存里的 forward.Workers client cache，
// web 端做跨进程代理，所以这里的 :bizId 传的是 **Target 业务 ID**（target_biz_id），
// 不是 SQLite 行号 —— 与 @/api/values.ts 同理。
import { apiGet } from './client'
/**
 * Target 运行时统计（来自 TargetClient.Stats()，各协议自报）。
 *
 * 字段是"尽力而为"的：不同 Target 实现的字段不一样，
 * 所以这里全部是可选的，读的时候用 `?? 默认值` 兜底。
 */
export interface TargetStats {
  /** 连接状态：Connected / Disconnected / Reconnecting */
  connection_state?: string
  /** 协议名（mqtt / influxdb） */
  protocol?: string
  /** 协议版本（mqtt: v5 / v3.1.1；influxdb: 1.x / 2 / 3） */
  protocol_version?: string
  /** MQTT: broker 地址 */
  broker_url?: string
  /** MQTT: 订阅/发布 topic */
  topic?: string
  /** InfluxDB: 服务器地址 */
  address?: string
  /** InfluxDB V1: 数据库名 */
  db?: string
  /** InfluxDB V2: org */
  org?: string
  /** InfluxDB V2: bucket */
  bucket?: string
  /** InfluxDB V2: 版本标识 */
  version?: string
  /** MQTT: broker 侧队列深度 */
  queue_depth?: number
  /** 成功发送的批次数 */
  sent_count?: number
  /** 成功发送的点数 */
  sent_points?: number
  /** 失败次数 */
  error_count?: number
  /** 最近一次发送成功时间（Unix ms） */
  last_send_ok_ms?: number
  /** 最近一次失败时间（Unix ms） */
  last_error_ms?: number
  /** 建连次数 */
  connect_count?: number
  /** 最近一次建连时间（Unix ms） */
  last_connect_ms?: number
  /** 健康检查次数 */
  health_check_count?: number
  /** 最近一次健康检查时间（Unix ms） */
  last_health_check_ms?: number
}

/** 单个 Target 的诊断信息。 */
export interface TargetDiag {
  target_biz_id: string
  /** 转发协议类型（mqtt / influxdb） */
  type?: string
  /** 数据序列化格式（json / line_protocol / raw） */
  format?: string
  /** 是否已连接 */
  connected?: boolean
  /** 最近一次错误信息（空 = 无错误） */
  last_error?: string
  stats?: TargetStats
  /** 转发队列当前长度 */
  queue_len?: number
  /** 转发队列容量 */
  queue_cap?: number
  /** 累计丢弃数（队列满） */
  dropped_total?: number
  /** 累计入队数 */
  enqueued_total?: number
}

/** 全量 Target 诊断列表的响应。 */
export interface TargetsDiagResp {
  targets?: TargetDiag[]
}

/** 单个 Target 的诊断信息。 */
export function fetchTargetDiag(targetBizId: string) {
  return apiGet<TargetDiag>(`/targets/${encodeURIComponent(targetBizId)}/diag`)
}

/**
 * 所有 Target 的诊断列表（一次请求拿全量，避免 N 次并发）。
 *
 * suppressGlobalError：这个接口被设备详情页每 5s 轮询一次，
 * 失败时调用方会自行置位"诊断不可用"标记，不需要全局错误弹窗
 * （否则下游服务一挂，页面每 5 秒弹一次，把屏幕刷爆）。
 */
export function fetchTargetsDiag() {
  return apiGet<TargetsDiagResp>('/targets/diag', { suppressGlobalError: true })
}

/**
 * 从 TargetStats 里取"成功发送数"。
 *
 * 不同 Target 实现的字段名不完全一致：
 *   - MQTT / InfluxDB 都有 sent_count（批次数）
 *   - sent_points 是点数（一个批次可能含多个点）
 * 优先用 sent_points（更贴近"发了多少数据"），没有则退回 sent_count。
 */
export function diagSentCount(d: TargetDiag | undefined): number {
  const s = d?.stats
  if (!s) return 0
  return s.sent_points ?? s.sent_count ?? 0
}

/** 从 TargetStats 里取失败次数。 */
export function diagErrorCount(d: TargetDiag | undefined): number {
  return d?.stats?.error_count ?? 0
}

/** 队列使用率（0~1）。队列容量为 0（未知）时返回 0。 */
export function diagQueueRatio(d: TargetDiag | undefined): number {
  const cap = d?.queue_cap ?? 0
  if (!cap) return 0
  return Math.min(1, (d?.queue_len ?? 0) / cap)
}

/**
 * Target 连接状态 → 展示用的语义。
 *
 * - `connected`  → 在线（绿）
 * - `error`     → 异常（红）：有 last_error
 * - `offline`   → 离线（灰）：从未连上或已断开且无错误信息
 */
export type DiagLevel = 'success' | 'danger' | 'info'

export function diagLevel(d: TargetDiag | undefined): DiagLevel {
  if (!d) return 'info'
  if (d.connected) return 'success'
  if (d.last_error) return 'danger'
  return 'info'
}

/** 连接状态文案。 */
export function diagStatusText(d: TargetDiag | undefined): string {
  if (!d) return '未知'
  if (d.connected) return '在线'
  if (d.last_error) return '异常'
  return '离线'
}

/**
 * 连接状态的 tooltip 详情。
 *
 * 断开且无错误信息时，提示"尚未建立连接"，而不是什么都不说 ——
 * 用户看到灰色标签至少知道系统没报故障，只是没连上。
 */
export function diagStatusTip(d: TargetDiag | undefined): string {
  if (!d) return '暂无诊断数据（plcbridge 主进程可能未运行）'
  const parts: string[] = [`状态：${diagStatusText(d)}`]
  if (d.last_error) parts.push(`最近错误：${d.last_error}`)
  if (d.stats?.connection_state) parts.push(`连接状态：${d.stats.connection_state}`)
  return parts.join('\n')
}
