// Web 控制台运行时设置
//
// 与「配置」区分开：
//   - 配置（channels/devices/sources/points/targets）→ 导出到 config.json → plcbridge 采集
//   - 设置（settings）→ 只影响 Web 控制台自身行为（如自动导出），不参与 plcbridge 采集
import { apiGet, apiPut } from './client'

/**
 * 全部设置项。
 *
 * 后端 knownSettings 是唯一白名单 —— 传未登记的键会被后端拒绝（400）。
 */
export interface WebSettings {
  /** 配置变更后自动导出 config.json（触发 plcbridge 热重载）。默认 false。 */
  auto_export_config: boolean
}

export function fetchSettings() {
  return apiGet<WebSettings>('/settings')
}

/**
 * 修改单个设置。
 *
 * 后端返回**全量**最新设置（省一次 GET），前端直接替换本地状态。
 */
export function updateSetting<K extends keyof WebSettings>(key: K, value: WebSettings[K]) {
  return apiPut<WebSettings>(`/settings/${key}`, { value })
}
