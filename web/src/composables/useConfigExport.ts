/**
 * 全局「导出配置」状态（useConfigExport）
 *
 * 解决的核心痛点：
 *   用户改完配置（设备/通道/点位/目标）后，必须切到仪表盘点才能点「导出配置」，
 *   否则 plcbridge 一直跑旧配置。来回切页面非常低效，也容易忘记导出。
 *
 * 本模块提供：
 *   1. `dirty`      —— 数据库配置是否比 config.json 新（有未导出的改动）
 *   2. `export()`   —— 一键导出，无需跳转仪表盘
 *   3. `status`     —— 完整状态（时间戳、路径、待导出条目数）
 *
 * 实现要点：
 *   - **单例状态**：模块级 reactive 对象，所有页面共享同一份 dirty 状态，
 *     避免每个页面各拉一次、各算一份。
 *   - **轮询兜底**：5s 拉一次 /config/status。写操作本身不通知前端
 *     （DB 里 updated_at 变了但前端不知道），所以靠轮询发现脏状态。
 *   - **写后立即刷新**：axios 响应拦截器在 2xx 写操作后广播一个事件，
 *     这里监听它 → 立刻重新拉状态，避免"改完还要等 5s 才亮角标"。
 */
import { reactive, computed, onMounted, onUnmounted } from 'vue'
import { ElMessage } from 'element-plus'
import { fetchConfigStatus, exportConfig, type ConfigStatus } from '@/api/config'
import { fetchSettings } from '@/api/settings'

/** 写操作成功后广播的事件名（axios 拦截器里 dispatch）。 */
export const CONFIG_CHANGED_EVENT = 'plcbridge:config-changed'

/** Ctrl+Shift+E 快捷键广播的事件名（快捷键模块 dispatch，这里消费）。 */
export const EXPORT_CONFIG_EVENT = 'plcbridge:export-config'

/** 轮询间隔（ms）。5s 足够快，又不会给后端造成压力。 */
const POLL_INTERVAL = 5000

interface ExportState {
  status: ConfigStatus | null
  loading: boolean
  exporting: boolean
  /** 上次导出成功的时间（epoch ms），用于"刚刚已导出"的短暂提示 */
  lastExportedAt: number
  /** 状态拉取失败次数（连续），用于降级展示 */
  errorCount: number
  /** 自动导出开关（后端 settings）。true = 改完配置会自动写 config.json。 */
  autoExport: boolean
}

const state = reactive<ExportState>({
  status: null,
  loading: false,
  exporting: false,
  lastExportedAt: 0,
  errorCount: 0,
  autoExport: false,
})

/** 是否存在未导出的配置改动。 */
const dirty = computed(() => state.status?.dirty === true)

/** 待导出的条目数（0 = 无改动）。 */
const pendingChanges = computed(() => state.status?.pending_changes ?? 0)

/** 自动导出是否开启。 */
const autoExport = computed(() => state.autoExport)

let pollTimer: number | null = null
let listenerBound = false
let refCount = 0

/** 拉一次脏状态。失败静默（不弹全局错误，页面已有状态展示）。 */
async function refresh(): Promise<void> {
  if (state.loading) return
  state.loading = true
  try {
    const s = await fetchConfigStatus()
    state.status = s
    state.errorCount = 0
  } catch {
    // 保留上一次的状态：网络抖动不该让"有未导出改动"的提示消失。
    state.errorCount++
  } finally {
    state.loading = false
  }
}

/**
 * 拉一次设置（自动导出开关）。
 *
 * 单独拉而不跟 refresh 合并：设置变更频率极低（用户手动开关），
 * 混进 5s 轮询里纯属浪费请求。
 */
export async function refreshSettings(): Promise<void> {
  try {
    const s = await fetchSettings()
    state.autoExport = s.auto_export_config === true
  } catch {
    // 拉不到 → 保持当前值（不猜）。
  }
}

/**
 * 导出配置（写 config.json → plcbridge 1s 内热重载）。
 *
 * @returns true = 导出成功
 */
async function doExport(): Promise<boolean> {
  if (state.exporting) return false
  state.exporting = true
  try {
    await exportConfig()
    state.lastExportedAt = Date.now()
    // 立刻同步状态（导出后 dirty 应变为 false）。
    await refresh()
    ElMessage.success('配置已导出，plcbridge 将在 1s 内自动热重载')
    return true
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.response?.data?.msg || e?.message || '导出配置失败')
    return false
  } finally {
    state.exporting = false
  }
}

function startPolling() {
  if (pollTimer !== null) return
  pollTimer = window.setInterval(refresh, POLL_INTERVAL)
}

function stopPolling() {
  if (pollTimer === null) return
  clearInterval(pollTimer)
  pollTimer = null
}

function bindChangeListener() {
  if (listenerBound) return
  listenerBound = true
  window.addEventListener(CONFIG_CHANGED_EVENT, () => {
    // 写操作刚完成 → 立刻重算脏状态（不等下一轮轮询）。
    void refresh()
  })
  // Ctrl+Shift+E → 导出配置
  window.addEventListener(EXPORT_CONFIG_EVENT, () => {
    void doExport()
  })
}

/**
 * 在组件里启用全局导出状态。
 *
 * 挂载时：拉一次 + 开始轮询 + 监听写操作事件。
 * 卸载时：引用计数 -1，归零则停轮询。
 *
 * 注意：状态本身是模块级单例，页面切换不会丢 dirty 状态。
 */
export function useConfigExport() {
  onMounted(() => {
    refCount++
    bindChangeListener()
    void refresh()
    void refreshSettings()
    startPolling()
  })

  onUnmounted(() => {
    refCount--
    if (refCount <= 0) {
      refCount = 0
      stopPolling()
    }
  })

  return {
    state,
    dirty,
    pendingChanges,
    autoExport,
    refresh,
    refreshSettings,
    exportConfig: doExport,
  }
}
