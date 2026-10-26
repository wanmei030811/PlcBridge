import axios, { AxiosInstance, AxiosRequestConfig } from 'axios'
import { ElMessage } from 'element-plus'

const http: AxiosInstance = axios.create({
  baseURL: '/api/v1',
  withCredentials: true,
  timeout: 15000
})

/**
 * 扩展 axios 配置：支持 `suppressGlobalError`。
 *
 * 背景（2026-10-25 修复"页面不停报错"）：
 * 全局拦截器对任何非 401 失败都会弹 `ElMessage.error`。但页面上有大量
 * **后台轮询**（仪表盘 1s、通道列表 1s、设备详情 2s/5s），它们失败时
 * 调用方本来就会自行降级处理（静默保留旧数据、置位"不可用"标记）。
 * 结果就是：下游服务一挂，页面每 1~5 秒弹一次同样的错误提示，
 * 完全淹没屏幕，用户既看不清真正的故障信息，也关不掉。
 *
 * 修法（两层）：
 *   1. 后台轮询显式传 `suppressGlobalError: true`，由调用方自己决定要不要提示。
 *   2. 即使调用方忘了标记，全局也做**同消息去重**：同一条消息在
 *      DEDUP_WINDOW 内只弹一次，避免任何高频请求把屏幕刷爆。
 */
export interface SuppressableRequestConfig extends AxiosRequestConfig {
  /** true = 该请求失败时不弹全局错误提示，由调用方自行处理。 */
  suppressGlobalError?: boolean
}

/**
 * 从 error 响应里抽出可显示给用户的消息。
 *
 * 兼容 Plan2 4.1 过渡期的两套错误格式：
 *   - 旧格式（handleStoreErr 还在用）：{ error: msg }
 *   - 新格式（业务校验 WriteError）：{ code, msg }
 *
 * 优先级：data.error (旧) > data.msg (新) > err.message (兜底)
 */
function extractErrorMessage(err: any): string {
    const data = err?.response?.data
  return data?.error || data?.msg || err?.message || '请求失败'
}

/** 同一条错误消息在这个窗口内只弹一次。 */
const DEDUP_WINDOW = 5000

/** 最近弹过的错误消息 → 时间戳。 */
const recentErrors = new Map<string, number>()

/**
 * 同消息去重：返回 true 表示这条消息**应该**弹提示。
 *
 * 为什么需要：现场最常见的失败是"下游服务挂了"，所有轮询接口会拿到
 * **完全相同**的错误文案。逐条弹会瞬间堆满屏幕。
 */
function shouldShowError(msg: string): boolean {
  const now = Date.now()
  // 顺手清理过期条目，避免 Map 无限增长。
  for (const [k, t] of recentErrors) {
    if (now - t > DEDUP_WINDOW) {
      recentErrors.delete(k)
    }
  }
  const last = recentErrors.get(msg)
  if (last !== undefined && now - last < DEDUP_WINDOW) {
    // 已弹过：刷新时间戳（持续失败时保持"静默"，避免每 5 秒反复弹）
    recentErrors.set(msg, now)
    return false
  }
  recentErrors.set(msg, now)
  return true
}

http.interceptors.response.use(
  (resp) => {
    // 写操作成功后广播事件，让「导出配置」按钮立刻重算脏状态，
    // 不用等下一轮 5s 轮询才发现"配置已改"。
    //
    // 只在 2xx 且方法是写操作时广播；GET/HEAD 不触发（读了不产生改动）。
    const m = (resp.config.method || 'get').toLowerCase()
    if (resp.status >= 200 && resp.status < 300 &&
        (m === 'post' || m === 'put' || m === 'delete' || m === 'patch')) {
      window.dispatchEvent(new CustomEvent('plcbridge:config-changed'))
    }
    return resp.data
  },
  (err) => {
    const status = err?.response?.status
    const msg = extractErrorMessage(err)
    // 401 跳登录页：
    //   - 排除 /me：router beforeEach 在每个跳转都会调 refresh()→me()，
    //     失败时若再 location.href=/login 会导致循环刷新。
    //   - 排除已经在 /login 的页面。
    const isMeCheck = err?.config?.url?.endsWith('/me') === true
    if (status === 401 && !isMeCheck && location.pathname !== '/login') {
      const next = encodeURIComponent(location.pathname + location.search)
      location.href = '/login?next=' + next
    } else if (status !== 401) {
      // 调用方显式声明"我自己处理错误"（后台轮询）→ 不弹全局提示。
      const suppressed = (err?.config as SuppressableRequestConfig | undefined)?.suppressGlobalError === true
      if (!suppressed && shouldShowError(msg)) {
        ElMessage.error(msg)
      }
    }
    return Promise.reject(err)
  }
)

export interface ListResp<T> {
  items: T[]
}

export async function apiGet<T = any>(url: string, config?: SuppressableRequestConfig): Promise<T> {
  return http.get(url, config) as unknown as T
}
export async function apiPost<T = any>(url: string, data?: any, config?: SuppressableRequestConfig): Promise<T> {
  return http.post(url, data, config) as unknown as T
}
export async function apiPut<T = any>(url: string, data?: any, config?: SuppressableRequestConfig): Promise<T> {
  return http.put(url, data, config) as unknown as T
}
export async function apiDelete<T = any>(url: string, config?: SuppressableRequestConfig): Promise<T> {
  return http.delete(url, config) as unknown as T
}

export default http
