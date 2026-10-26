// 配置导出（把 SQLite 里的配置写入 config.json，触发 plcbridge 热重载）
//
// 为什么单独一个模块：
//   "改完配置要回仪表盘点导出" 是最常见的操作路径，但导出按钮只藏在
//   仪表盘「进程控制 → 配置控制」里，换个页面就找不到。
//   这里把它抽成全局能力，配合 useConfigExport composable，
//   顶栏任何页面都能一键导出。
import { apiGet, apiPost } from './client'

export interface ConfigStatus {
  ok: boolean
  /** config.json 的绝对路径 */
  config_path: string
  /** config.json 最后一次导出的时间（epoch ms）；0 = 从未导出过 */
  last_export_ms: number
  /** 数据库里最后一次配置变更的时间（epoch ms）；0 = 从未变更 */
  last_change_ms: number
  /** true = 数据库比 config.json 新（改了配置还没导出） */
  dirty: boolean
  /** 未导出的变更行数 */
  pending_changes: number
}

export interface ExportResult {
  ok: boolean
  path?: string
  version?: number
}

/**
 * 拉取配置脏状态。
 *
 * suppressGlobalError：这个状态只用来画顶栏角标，拉不到就保持旧状态，
 * 不该弹全局错误把用户从当前操作里打断。
 */
export function fetchConfigStatus() {
  return apiGet<ConfigStatus>('/config/status', { suppressGlobalError: true })
}

/**
 * 导出配置：把当前 SQLite 配置写入 config.json。
 *
 * plcbridge 通过 fsnotify 监听 config.json，1s 内自动热重载。
 */
export function exportConfig() {
  return apiPost<ExportResult>('/snapshot', {})
}
