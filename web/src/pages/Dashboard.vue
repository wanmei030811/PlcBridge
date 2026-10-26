<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed } from 'vue'
import { useRouter } from 'vue-router'
import { fetchDashboard, type Dashboard } from '@/api/dashboard'
import {
  fetchWebHealth, fetchPlcbridgeSummary,
  postProcessControl, fetchSupervisorStatus,
  type WebHealth, type PlcbridgeSummary,
  type SupervisorStatusResp
} from '@/api/system'
import { ElMessage } from 'element-plus'
import IconButton from '@/components/IconButton.vue'
import { useConfigExport } from '@/composables/useConfigExport'

// 控制动作 ID：与后端 /api/v1/process/control 保持一致
type CtrlAction = 'export_config' | 'reload_config' | 'stop_plcbridge' | 'start_plcbridge' | 'restart_plcbridge-web' | 'start_supervisor' | 'stop_supervisor'

const router = useRouter()
const data = ref<Dashboard | null>(null)
const loading = ref(false)
// 防止组件卸载后异步写入组件状态
let unmounted = false
let refreshSeq = 0

async function refresh() {
  const seq = ++refreshSeq
  loading.value = true
  try {
    const r = await fetchDashboard()
    if (unmounted || seq !== refreshSeq) return
    data.value = r
  } catch (e: any) {
    if (unmounted || seq !== refreshSeq) return
    ElMessage.error(e?.response?.data?.error || e?.message || '加载概览失败')
  } finally {
    if (!unmounted && seq === refreshSeq) loading.value = false
  }
}

function goto(path: string) {
  router.push(path)
}

// === plcbridge 实时监控（每 3s 心跳） ===
const webHealth = ref<WebHealth | null>(null)
const plcSummary = ref<PlcbridgeSummary | null>(null)
const liveError = ref<string>('')

// 上一次 collect/write/target_sent 的快照，用于算「最近 3s 增量」
let lastSnapshotAt = 0
let lastCollectTotal = 0
let lastWriteTotal = 0
let lastTargetSent = 0
const collectPerSec = ref<number | null>(null)
const writePerSec = ref<number | null>(null)
const targetPerSec = ref<number | null>(null)

async function refreshLive() {
  if (unmounted) return
  try {
    // 并发请求，互不依赖
    const [h, m] = await Promise.all([
      fetchWebHealth(),
      fetchPlcbridgeSummary()
    ])
    if (unmounted) return
    webHealth.value = h
    plcSummary.value = m
    liveError.value = (h.status !== 'ok' ? 'web ' + h.db + ' ' : '') + (m.ok ? '' : 'plcbridge ' + (m.error || 'unknown'))
  } catch (e: any) {
    if (unmounted) return
    liveError.value = e?.message || '拉取失败'
  }

  // 计算 QPS（基于 counter 变化 / 间隔时间）。
  // 首次拉取只记录基线，不计算速率（避免显示 "0.0" 误导）。
  if (plcSummary.value && plcSummary.value.ok) {
    const now = performance.now()
    const c = plcSummary.value.collect_total.success + plcSummary.value.collect_total.fail
    const w = plcSummary.value.write_total.success + plcSummary.value.write_total.fail
    const t = plcSummary.value.target_sent_total
    if (lastSnapshotAt > 0) {
      const dt = (now - lastSnapshotAt) / 1000
      if (dt > 0) {
        // counter 不会倒退；如发现倒退（重置/重启），重新基线
        const dC = c - lastCollectTotal
        const dW = w - lastWriteTotal
        const dT = t - lastTargetSent
        if (dC >= 0) collectPerSec.value = dC / dt
        if (dW >= 0) writePerSec.value = dW / dt
        if (dT >= 0) targetPerSec.value = dT / dt
      }
    }
    lastSnapshotAt = now
    lastCollectTotal = c
    lastWriteTotal = w
    lastTargetSent = t
  }
}

let liveTimer: number | null = null
let supTimer: number | null = null
onMounted(() => {
  refresh()
  // 立即拉一次（不等首屏 1s）
  refreshLive()
  refreshSupervisor()
  liveTimer = window.setInterval(refreshLive, 1000)
  supTimer = window.setInterval(refreshSupervisor, 3000)
})
onUnmounted(() => {
  unmounted = true
  if (liveTimer !== null) {
    clearInterval(liveTimer)
    liveTimer = null
  }
  if (supTimer !== null) {
    clearInterval(supTimer)
    supTimer = null
  }
})

function fmtBytes(n: number) {
  if (!n || n < 0) return '—'
  if (n < 1024) return n + ' B'
  if (n < 1024 * 1024) return (n / 1024).toFixed(2) + ' KB'
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(2) + ' MB'
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB'
}
function fmtUptime(ms: number) {
  if (!ms || ms < 0) return '—'
  const s = Math.floor(ms / 1000)
  if (s < 60) return s + ' 秒'
  if (s < 3600) return Math.floor(s / 60) + ' 分 ' + (s % 60) + ' 秒'
  if (s < 86400) return Math.floor(s / 3600) + ' 时 ' + Math.floor((s % 3600) / 60) + ' 分'
  return Math.floor(s / 86400) + ' 天 ' + Math.floor((s % 86400) / 3600) + ' 时'
}
function fmtNum(n: number | null | undefined, digits = 1) {
  if (n === undefined || n === null || Number.isNaN(n)) return '—'
  if (Math.abs(n) >= 1000) return n.toFixed(0)
  return n.toFixed(digits)
}
function fmtAge(ms: number) {
  if (!ms || ms < 0) return '刚刚'
  const s = Math.floor(ms / 1000)
  if (s < 60) return s + ' 秒前'
  return Math.floor(s / 60) + ' 分前'
}

// === 进程控制（仪表盘操作面板） ===
const supStatus = ref<SupervisorStatusResp | null>(null)
const ctrlBusy = ref<string>('')

async function refreshSupervisor() {
  try {
    supStatus.value = await fetchSupervisorStatus()
  } catch {
    supStatus.value = null
  }
}

const supRunning = computed(() => !!supStatus.value?.supervisor_running)
const coreRunning = computed(() => (supStatus.value?.state?.core_pid ?? 0) > 0)
const webRunning = computed(() => (supStatus.value?.state?.web_pid ?? 0) > 0)

// 异步执行一个控制动作（不弹窗；调用方负责确认 UI）。
async function runCtrl(action: CtrlAction) {
  ctrlBusy.value = action
  try {
    const r = await postProcessControl(action, 'dashboard')
    if (r.ok) {
      if (r.triggered) {
        ElMessage.success(r.message || '指令已发出')
      } else {
        ElMessage.warning(r.message || '指令已发出，但 supervisor 未运行')
      }
    } else {
      ElMessage.error(r.message || '操作失败')
    }
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '操作失败')
  } finally {
    ctrlBusy.value = ''
    // refreshLive 下次 tick 会自动刷 supervisor
  }
}

const onExportConfig   = () => runCtrl('export_config')
const onReloadConfig    = () => runCtrl('reload_config')
const onStartPlcbridge  = () => runCtrl('start_plcbridge')
const onStopPlcbridge   = () => runCtrl('stop_plcbridge')
const onRestartWeb      = () => runCtrl('restart_plcbridge-web')
const onStartSupervisor = () => runCtrl('start_supervisor')
const onStopSupervisor  = () => runCtrl('stop_supervisor')

// === 配置脏状态（顶栏也有同一个按钮，这里复用同一份状态）===
//
// dirty = true → 改了配置还没导出 config.json，plcbridge 跑的还是旧配置。
const { dirty, pendingChanges } = useConfigExport()
</script>

<template>
  <div class="dashboard-page" v-loading="loading">
    <!-- 主要指标卡 4 列 -->
    <div class="metric-row" v-if="data">
      <div class="metric-card metric-blue" @click="goto('/channels')">
        <div class="metric-info">
          <div class="metric-label">通道</div>
          <div class="metric-value">{{ data.channels.total }}</div>
          <div class="metric-sub">采集 {{ data.channels.collect }} · 转发 {{ data.channels.forward }}</div>
        </div>
        <div class="metric-icon">
          <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="18" cy="5" r="3"/><circle cx="6" cy="12" r="3"/><circle cx="18" cy="19" r="3"/><line x1="8.59" y1="13.51" x2="15.42" y2="17.49"/><line x1="15.41" y1="6.51" x2="8.59" y2="10.49"/></svg>
        </div>
      </div>

      <div class="metric-card metric-green" @click="goto('/devices')">
        <div class="metric-info">
          <div class="metric-label">设备</div>
          <div class="metric-value">{{ data.devices.total }}</div>
          <div class="metric-sub">启用 {{ data.devices.enabled }} · 停用 {{ data.devices.total - data.devices.enabled }}</div>
        </div>
        <div class="metric-icon">
          <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="4" width="16" height="16" rx="2"/><rect x="9" y="9" width="6" height="6"/></svg>
        </div>
      </div>

      <div class="metric-card metric-purple" @click="goto('/devices')">
        <div class="metric-info">
          <div class="metric-label">数据源 (Source)</div>
          <div class="metric-value">{{ data.sources.total }}</div>
          <div class="metric-sub">采集 {{ data.sources.collect_on }} · 写入 {{ data.sources.write_on }}</div>
        </div>
        <div class="metric-icon">
          <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/></svg>
        </div>
      </div>

      <div class="metric-card metric-orange" @click="goto('/devices')">
        <div class="metric-info">
          <div class="metric-label">点位 (Point)</div>
          <div class="metric-value">{{ data.points.total }}</div>
          <div class="metric-sub">采集 {{ data.points.collect_on }} · 写入 {{ data.points.write_on }}</div>
        </div>
        <div class="metric-icon">
          <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>
        </div>
      </div>
    </div>

    <!-- 首次加载 / 数据未到达时的骨架占位，避免空白首屏 -->
    <div class="metric-row" v-else-if="loading">
      <div v-for="i in 4" :key="i" class="metric-card metric-skeleton">
        <div class="metric-info">
          <div class="skeleton-line w-30"></div>
          <div class="skeleton-value"></div>
          <div class="skeleton-line w-60"></div>
        </div>
        <div class="metric-icon skeleton-icon"></div>
      </div>
    </div>
    <!-- 加载失败占位 -->
    <div v-else class="error-card">
      <div class="error-title">加载概览失败</div>
      <div class="error-desc">检查后端服务是否运行正常，可点击右上角刷新按钮重试。</div>
      <IconButton name="refresh" type="primary" tip="重试" @click="refresh" />
    </div>

    <!-- 进程控制（仪表盘启停面板） -->
    <div class="control-section" v-if="data">
      <div class="control-header">
        <span class="control-title">进程控制</span>
        <span class="control-meta">
          <span v-if="!supRunning" class="ctrl-meta-warn" title="未检测到 supervisor，需要手动运行 scripts\supervisor.ps1">
            ⚠ supervisor 未运行
          </span>
          <span v-else class="ctrl-meta-ok">
            supervisor @ PID {{ supStatus?.state?.supervisor_pid }}
            · web @ {{ webRunning ? 'PID ' + supStatus?.state?.web_pid : 'down' }}
            · plcbridge @ {{ coreRunning ? 'PID ' + supStatus?.state?.core_pid : 'down' }}
          </span>
        </span>
      </div>

      <div class="ctrl-group">
        <div class="ctrl-group-head">
          <span class="ctrl-group-title">WEB 控制</span>
          <span class="ctrl-group-sub">管理 plcbridge-web 控制台进程</span>
        </div>
        <div class="control-grid">
          <!-- 1. 重启 Web（最前） -->
          <el-popconfirm
            title="重启 Web 控制台？浏览器会断开 5-10 秒，plcbridge 不受影响。"
            confirm-button-text="重启 Web"
            cancel-button-text="取消"
            confirm-button-type="warning"
            width="280"
            @confirm="onRestartWeb"
          >
            <template #reference>
              <button class="ctrl-btn ctrl-purple" :disabled="!!ctrlBusy">
                <div class="ctrl-icon">🔁</div>
                <div class="ctrl-body">
                  <div class="ctrl-label">重启 Web</div>
                  <div class="ctrl-sub">刷新页面会断开 5-10s，plcbridge 不受影响</div>
                </div>
              </button>
            </template>
          </el-popconfirm>
        </div>
      </div>

      <!-- 配置控制：导出 / 重载 -->
      <div class="ctrl-group">
        <div class="ctrl-group-head">
          <span class="ctrl-group-title">配置控制</span>
          <span class="ctrl-group-sub">写 config.json，触发 plcbridge 热重载</span>
        </div>
        <div class="control-grid">
          <!-- 2. 导出配置（仅导出 config.json 快照，plcbridge 自动 reload） -->
          <button
            class="ctrl-btn ctrl-cyan"
            :class="{ 'ctrl-cyan-dirty': dirty }"
            :disabled="!!ctrlBusy"
            @click="onExportConfig"
          >
            <div class="ctrl-icon">💾</div>
            <div class="ctrl-body">
              <div class="ctrl-label">
                导出配置
                <span v-if="dirty" class="ctrl-dirty-tag">有 {{ pendingChanges || '!' }} 项未导出</span>
              </div>
              <div class="ctrl-sub">仅写入 config.json，plcbridge 自动热重载</div>
            </div>
          </button>

          <!-- 3. 重载配置（= 导出 + 提示） -->
          <button class="ctrl-btn ctrl-blue" :disabled="!!ctrlBusy" @click="onReloadConfig">
            <div class="ctrl-icon">🔄</div>
            <div class="ctrl-body">
              <div class="ctrl-label">重载配置</div>
              <div class="ctrl-sub">导出 config.json → plcbridge 自动热重载</div>
            </div>
          </button>
        </div>
      </div>

      <!-- PLCBRIDGE 控制：启停 core / supervisor -->
      <div class="ctrl-group">
        <div class="ctrl-group-head">
          <span class="ctrl-group-title">PLCBRIDGE 控制</span>
          <span class="ctrl-group-sub">启停 plcbridge 主进程与整个 supervisor 链</span>
        </div>
        <div class="control-grid">
          <!-- 4. 停止 plcbridge（注意：pause 标志会让 supervisor 不再拉起） -->
          <el-popconfirm
            title="确定要停止 plcbridge 主进程吗？所有采集/转发将中断（web 仍可访问）。"
            confirm-button-text="停止 plcbridge"
            cancel-button-text="取消"
            confirm-button-type="warning"
            width="280"
            @confirm="onStopPlcbridge"
          >
            <template #reference>
              <button class="ctrl-btn ctrl-yellow" :disabled="!!ctrlBusy || !coreRunning">
                <div class="ctrl-icon">⏹</div>
                <div class="ctrl-body">
                  <div class="ctrl-label">停止 plcbridge</div>
                  <div class="ctrl-sub">中断采集/转发（supervisor 不会自动拉起）</div>
                </div>
              </button>
            </template>
          </el-popconfirm>

          <!-- 5. 启动 plcbridge -->
          <button class="ctrl-btn ctrl-green" :disabled="!!ctrlBusy || coreRunning" @click="onStartPlcbridge">
            <div class="ctrl-icon">▶</div>
            <div class="ctrl-body">
              <div class="ctrl-label">启动 plcbridge</div>
              <div class="ctrl-sub">恢复数据采集与转发</div>
            </div>
          </button>

          <!-- 6. 启动 supervisor（如果 supervisor 不在就拉起它） -->
          <button class="ctrl-btn ctrl-teal" :disabled="!!ctrlBusy || supRunning" @click="onStartSupervisor">
            <div class="ctrl-icon">🛡</div>
            <div class="ctrl-body">
              <div class="ctrl-label">启动 supervisor</div>
              <div class="ctrl-sub">detached 拉起 supervisor / web / plcbridge</div>
            </div>
          </button>

          <!-- 7. 停止整个 PlcBridge（注意：web 会保留, 仍可从'启动 supervisor'恢复) -->
          <el-popconfirm
            title="⚠ 将停止 plcbridge 主进程（采集/转发中断）。web 控制台保留在线，可用『启动 supervisor』按钮恢复。确定吗？"
            confirm-button-text="停止"
            cancel-button-text="取消"
            confirm-button-type="danger"
            width="300"
            @confirm="onStopSupervisor"
          >
            <template #reference>
              <button class="ctrl-btn ctrl-red" :disabled="!!ctrlBusy">
                <div class="ctrl-icon">⏻</div>
                <div class="ctrl-body">
                  <div class="ctrl-label">停止整个 PlcBridge</div>
                  <div class="ctrl-sub">web 控制台保留,可用『启动 supervisor』恢复</div>
                </div>
              </button>
            </template>
          </el-popconfirm>
        </div>
      </div>
    </div>

    <!-- plcbridge 实时监控（每 3s 心跳） -->
    <div class="live-section" v-if="data">      <div class="live-header">
        <span class="live-title">plcbridge 实时监控</span>
        <span class="live-meta">
          <span v-if="liveError" class="live-meta-err" :title="liveError">⚠ {{ liveError }}</span>
          <span v-else-if="plcSummary && webHealth" class="live-meta-ok">
            plcbridge @ {{ fmtAge(plcSummary.age_ms) }} · web @ {{ webHealth.process.go_version }}
          </span>
          <span v-else>连接中…</span>
        </span>
      </div>

      <div class="live-grid">
        <!-- web 进程 PID/启动 -->
        <div class="live-card live-card-cyan">
          <div class="live-icon">
            <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>
          </div>
          <div class="live-body">
            <div class="live-label">web 进程</div>
            <div class="live-value">
              <template v-if="webHealth">PID {{ webHealth.process.pid }} · {{ webHealth.process.num_cpu }}C</template>
              <template v-else>—</template>
            </div>
            <div class="live-sub" v-if="webHealth">uptime {{ fmtUptime(webHealth.process.uptime_ms) }} · {{ webHealth.process.num_goroutine }} goroutine</div>
            <div class="live-sub" v-else>加载中…</div>
          </div>
        </div>

        <!-- web RSS -->
        <div class="live-card live-card-rose">
          <div class="live-icon">
            <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="6" width="20" height="12" rx="2"/><line x1="6" y1="10" x2="6" y2="14"/><line x1="10" y1="10" x2="10" y2="14"/><line x1="14" y1="10" x2="14" y2="14"/><line x1="18" y1="10" x2="18" y2="14"/></svg>
          </div>
          <div class="live-body">
            <div class="live-label">web 内存 (RSS)</div>
            <div class="live-value">{{ webHealth ? fmtBytes(webHealth.process.rss_bytes) : '—' }}</div>
            <div class="live-sub" v-if="webHealth">Go {{ webHealth.process.go_version }}</div>
            <div class="live-sub" v-else>加载中…</div>
          </div>
        </div>

        <!-- plcbridge 采集 QPS -->
        <div class="live-card live-card-green">
          <div class="live-icon">
            <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="22" y1="2" x2="11" y2="13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/></svg>
          </div>
          <div class="live-body">
            <div class="live-label">采集速率</div>
            <div class="live-value">{{ collectPerSec === null ? '—' : fmtNum(collectPerSec) }} <span class="live-unit">次/s</span></div>
            <div class="live-sub" v-if="plcSummary && plcSummary.ok">
              累计 {{ plcSummary.collect_total.success + plcSummary.collect_total.fail }} · 失败 {{ plcSummary.collect_total.fail }}
              · 平均 {{ fmtNum(plcSummary.collect_avg_ms) }} ms
            </div>
            <div class="live-sub" v-else-if="plcSummary && !plcSummary.ok">plcbridge 不可达</div>
            <div class="live-sub" v-else>加载中…</div>
          </div>
        </div>

        <!-- plcbridge 写入 QPS -->
        <div class="live-card live-card-purple">
          <div class="live-icon">
            <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h9"/><path d="M16.5 3.5a2.121 2.121 0 1 1 3 3L7 19l-4 1 1-4 12.6-12.5z"/></svg>
          </div>
          <div class="live-body">
            <div class="live-label">写入速率</div>
            <div class="live-value">{{ writePerSec === null ? '—' : fmtNum(writePerSec) }} <span class="live-unit">次/s</span></div>
            <div class="live-sub" v-if="plcSummary && plcSummary.ok">
              累计 {{ plcSummary.write_total.success + plcSummary.write_total.fail }} · 失败 {{ plcSummary.write_total.fail }}
            </div>
            <div class="live-sub" v-else-if="plcSummary && !plcSummary.ok">plcbridge 不可达</div>
            <div class="live-sub" v-else>加载中…</div>
          </div>
        </div>

        <!-- plcbridge 转发 QPS -->
        <div class="live-card live-card-blue">
          <div class="live-icon">
            <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/></svg>
          </div>
          <div class="live-body">
            <div class="live-label">转发速率</div>
            <div class="live-value">{{ targetPerSec === null ? '—' : fmtNum(targetPerSec) }} <span class="live-unit">次/s</span></div>
            <div class="live-sub" v-if="plcSummary && plcSummary.ok">
              累计 {{ plcSummary.target_sent_total }} · 队列 {{ plcSummary.target_queue.total }}
            </div>
            <div class="live-sub" v-else-if="plcSummary && !plcSummary.ok">plcbridge 不可达</div>
            <div class="live-sub" v-else>加载中…</div>
          </div>
        </div>

        <!-- plcbridge 错误数（最近 1 小时，来自 /api/v1/dashboard） -->
        <div class="live-card" :class="{
              'live-card-orange': !data || ((data.collect_last_hour?.total_errors || 0) + (data.targets_last_hour?.total_errors || 0) + (data.write_last_hour?.total_errors || 0)) === 0,
              'live-card-red':   data && ((data.collect_last_hour?.total_errors || 0) + (data.targets_last_hour?.total_errors || 0) + (data.write_last_hour?.total_errors || 0)) > 0
            }">
          <div class="live-icon">
            <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>
          </div>
          <div class="live-body">
            <div class="live-label">最近 1h 错误</div>
            <div class="live-value" v-if="data">
              {{ (data.collect_last_hour?.total_errors || 0) + (data.targets_last_hour?.total_errors || 0) + (data.write_last_hour?.total_errors || 0) }}
              <span class="live-unit">次</span>
            </div>
            <div class="live-value" v-else>—</div>
            <div class="live-sub" v-if="data">
              采集 {{ data.collect_last_hour?.total_errors || 0 }} · 转发 {{ data.targets_last_hour?.total_errors || 0 }} · 写入 {{ data.write_last_hour?.total_errors || 0 }}
            </div>
            <div class="live-sub" v-else>加载中…</div>
          </div>
        </div>
      </div>
    </div>

    <!-- 次要指标 + 提示（同行布局） -->
    <div class="bottom-row" v-if="data">
      <div class="info-card">
        <div class="info-icon">
          <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="12" y1="16" x2="12" y2="12"/><line x1="12" y1="8" x2="12.01" y2="8"/></svg>
        </div>
        <div class="info-body">
          <div class="info-title">提示</div>
          <div class="info-desc">修改资源后请导出快照触发热重载；运行状态查看 Prometheus :9091。</div>
        </div>
      </div>
      <div class="metric-mini" @click="goto('/devices')">
        <div class="mini-icon mini-green">
          <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="22" y1="2" x2="11" y2="13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/></svg>
        </div>
        <div>
          <div class="mini-label">转发目标 (Target)</div>
          <div class="mini-value">{{ data.targets.total }}</div>
        </div>
      </div>
      <div class="metric-mini" @click="goto('/tokens')">
        <div class="mini-icon mini-blue">
          <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4"/></svg>
        </div>
        <div>
          <div class="mini-label">活跃 API Token</div>
          <div class="mini-value">{{ data.tokens_active }}</div>
        </div>
      </div>
      <div class="metric-mini">
        <div class="mini-icon mini-orange">
          <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/></svg>
        </div>
        <div>
          <div class="mini-label">SQLite 占用</div>
          <div class="mini-value">{{ fmtBytes(data.storage_bytes) }}</div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.dashboard-page { max-width: 1200px; margin: 0 auto; }

/* ===== 页面背景 ===== */
.dashboard-page { max-width: 1200px; margin: 0 auto; padding: 4px 0; }

/* ===== 主要指标卡 ===== */
.metric-row {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 16px;
  margin-bottom: 18px;
}
.metric-card {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 22px 22px;
  border-radius: 12px;
  background: #fff;
  border: 1px solid #eef0f5;
  box-shadow: 0 1px 2px rgba(15,23,42,0.04);
  cursor: pointer;
  transition: transform 150ms ease, box-shadow 150ms ease, border-color 150ms ease;
  position: relative;
  overflow: hidden;
  min-height: 112px;
}
.metric-card::after {
  content: '';
  position: absolute;
  right: -30px; top: -30px;
  width: 100px; height: 100px;
  border-radius: 50%;
  background: var(--accent-soft);
  opacity: 0.7;
}
.metric-card:hover {
  transform: translateY(-2px);
  box-shadow: 0 10px 24px rgba(15,23,42,0.08);
  border-color: var(--accent);
}
.metric-blue   { --accent: #3b82f6; --accent-soft: #dbeafe; }
.metric-green  { --accent: #10b981; --accent-soft: #d1fae5; }
.metric-purple { --accent: #a855f7; --accent-soft: #ede9fe; }
.metric-orange { --accent: #f97316; --accent-soft: #ffedd5; }

.metric-info { min-width: 0; position: relative; z-index: 1; }
.metric-label {
  font-size: 12px;
  color: #64748b;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.6px;
}
.metric-value {
  font-size: 36px;
  font-weight: 700;
  color: #0f172a;
  margin-top: 8px;
  font-feature-settings: "tnum";
  letter-spacing: -1.2px;
  line-height: 1;
}
.metric-sub {
  font-size: 12px;
  color: #64748b;
  margin-top: 8px;
}
.metric-sub b { color: var(--accent); font-weight: 600; }
.metric-icon {
  width: 48px; height: 48px;
  border-radius: 12px;
  display: flex; align-items: center; justify-content: center;
  color: #fff;
  flex-shrink: 0;
  position: relative; z-index: 1;
  box-shadow: 0 4px 10px rgba(15,23,42,0.12);
}
.metric-blue   .metric-icon { background: linear-gradient(135deg, #60a5fa, #2563eb); }
.metric-green  .metric-icon { background: linear-gradient(135deg, #34d399, #059669); }
.metric-purple .metric-icon { background: linear-gradient(135deg, #c084fc, #7c3aed); }
.metric-orange .metric-icon { background: linear-gradient(135deg, #fb923c, #ea580c); }

/* ===== 次要 mini 卡 + 提示卡(同行布局) ===== */
.bottom-row {
  display: grid;
  grid-template-columns: 1.4fr 1fr 1fr 1fr;
  gap: 16px;
  align-items: stretch;
}
.metric-mini {
  display: flex;
  align-items: center;
  gap: 14px;
  padding: 16px 18px;
  background: #fff;
  border-radius: 12px;
  border: 1px solid #eef0f5;
  box-shadow: 0 1px 2px rgba(15,23,42,0.04);
  cursor: pointer;
  transition: all 150ms;
  min-height: 72px;
}
.metric-mini:hover {
  border-color: #cbd5e1;
  transform: translateY(-1px);
  box-shadow: 0 6px 14px rgba(15,23,42,0.06);
}
.mini-icon {
  width: 38px; height: 38px;
  border-radius: 10px;
  display: flex; align-items: center; justify-content: center;
  color: #fff;
  flex-shrink: 0;
  box-shadow: 0 3px 8px rgba(15,23,42,0.10);
}
.mini-green  { background: linear-gradient(135deg, #34d399, #059669); }
.mini-blue   { background: linear-gradient(135deg, #60a5fa, #2563eb); }
.mini-orange { background: linear-gradient(135deg, #fb923c, #ea580c); }
.mini-label { font-size: 12px; color: #64748b; font-weight: 500; }
.mini-value {
  font-size: 20px;
  font-weight: 700;
  color: #0f172a;
  margin-top: 2px;
  font-feature-settings: "tnum";
  letter-spacing: -0.5px;
}

/* ===== 提示卡(同行版) ===== */
.info-card {
  display: flex;
  gap: 14px;
  padding: 14px 18px;
  background: linear-gradient(135deg, #f0f9ff, #ecfeff);
  border: 1px solid #bae6fd;
  border-radius: 12px;
  align-items: center;
  min-height: 72px;
}
.info-icon {
  width: 36px; height: 36px;
  background: linear-gradient(135deg, #38bdf8, #0284c7);
  border-radius: 10px;
  display: flex; align-items: center; justify-content: center;
  color: #fff;
  flex-shrink: 0;
  box-shadow: 0 3px 8px rgba(14,165,233,0.25);
}
.info-body { min-width: 0; }
.info-title { font-weight: 600; color: #0c4a6e; margin-bottom: 2px; font-size: 13px; }
.info-desc { font-size: 12px; color: #0369a1; line-height: 1.5; }

/* ===== 骨架占位 ===== */
.metric-skeleton { cursor: default; }
.metric-skeleton:hover { transform: none; box-shadow: 0 1px 2px rgba(15,23,42,0.04); border-color: #eef0f5; }
.skeleton-line {
  height: 12px; border-radius: 4px;
  background: linear-gradient(90deg, #f1f5f9 0%, #e2e8f0 50%, #f1f5f9 100%);
  background-size: 200% 100%;
  animation: skeleton-shimmer 1.4s ease-in-out infinite;
  margin-bottom: 10px;
}
.skeleton-value {
  width: 60%; height: 32px; border-radius: 6px; margin: 8px 0 8px;
  background: linear-gradient(90deg, #f1f5f9 0%, #e2e8f0 50%, #f1f5f9 100%);
  background-size: 200% 100%;
  animation: skeleton-shimmer 1.4s ease-in-out infinite;
}
.skeleton-icon {
  background: linear-gradient(90deg, #f1f5f9 0%, #e2e8f0 50%, #f1f5f9 100%);
  background-size: 200% 100%;
  animation: skeleton-shimmer 1.4s ease-in-out infinite;
  box-shadow: none;
}
.w-30 { width: 30%; }
.w-60 { width: 60%; }
@keyframes skeleton-shimmer {
  0% { background-position: 200% 0; }
  100% { background-position: -200% 0; }
}

/* ===== 错误态 ===== */
.error-card {
  background: #fff; border: 1px solid #fecaca;
  border-radius: 12px; padding: 32px;
  text-align: center; display: flex; flex-direction: column; align-items: center; gap: 12px;
  margin-bottom: 18px;
}
.error-title { font-size: 16px; font-weight: 600; color: #b91c1c; }
.error-desc { font-size: 13px; color: #6b7280; margin-bottom: 4px; }

/* ===== plcbridge 实时监控 ===== */
.live-section {
  margin-bottom: 18px;
  padding: 16px 18px;
  background: linear-gradient(135deg, #fafbff, #f5f7fb);
  border: 1px solid #e5e7eb;
  border-radius: 12px;
  box-shadow: 0 1px 2px rgba(15,23,42,0.03);
}
.live-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 12px;
}
.live-title {
  font-size: 13px;
  font-weight: 600;
  color: #0f172a;
  letter-spacing: 0.3px;
}
.live-title::before {
  content: '';
  display: inline-block;
  width: 6px; height: 6px;
  background: #10b981;
  border-radius: 50%;
  margin-right: 8px;
  vertical-align: middle;
  box-shadow: 0 0 0 0 rgba(16,185,129,0.7);
  animation: live-pulse 2s ease-in-out infinite;
}
@keyframes live-pulse {
  0%, 100% { box-shadow: 0 0 0 0 rgba(16,185,129,0.7); }
  50%      { box-shadow: 0 0 0 6px rgba(16,185,129,0); }
}
.live-meta { font-size: 11px; color: #64748b; }
.live-meta-ok { color: #059669; }
.live-meta-err { color: #b91c1c; }
.live-grid {
  display: grid;
  grid-template-columns: repeat(6, 1fr);
  gap: 12px;
}
.live-card {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 12px 14px;
  background: #fff;
  border-radius: 10px;
  border: 1px solid #eef0f5;
  box-shadow: 0 1px 2px rgba(15,23,42,0.04);
  min-height: 80px;
  transition: all 150ms ease;
  position: relative;
  overflow: hidden;
}
.live-card::after {
  content: '';
  position: absolute;
  left: 0; top: 0; bottom: 0;
  width: 3px;
  background: var(--live-accent, #3b82f6);
}
.live-card-cyan   { --live-accent: #06b6d4; }
.live-card-rose   { --live-accent: #f43f5e; }
.live-card-green  { --live-accent: #10b981; }
.live-card-purple { --live-accent: #a855f7; }
.live-card-blue   { --live-accent: #3b82f6; }
.live-card-orange { --live-accent: #f97316; }
.live-card-red    { --live-accent: #ef4444; }
.live-icon {
  width: 32px; height: 32px;
  border-radius: 8px;
  display: flex; align-items: center; justify-content: center;
  color: #fff;
  flex-shrink: 0;
  box-shadow: 0 2px 6px rgba(15,23,42,0.10);
}
.live-card-cyan   .live-icon { background: linear-gradient(135deg, #22d3ee, #0891b2); }
.live-card-rose   .live-icon { background: linear-gradient(135deg, #fb7185, #e11d48); }
.live-card-green  .live-icon { background: linear-gradient(135deg, #34d399, #059669); }
.live-card-purple .live-icon { background: linear-gradient(135deg, #c084fc, #7c3aed); }
.live-card-blue   .live-icon { background: linear-gradient(135deg, #60a5fa, #2563eb); }
.live-card-orange .live-icon { background: linear-gradient(135deg, #fb923c, #ea580c); }
.live-card-red    .live-icon { background: linear-gradient(135deg, #f87171, #dc2626); }
.live-body { min-width: 0; flex: 1; }
.live-label {
  font-size: 11px;
  color: #64748b;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.4px;
  margin-bottom: 4px;
}
.live-value {
  font-size: 18px;
  font-weight: 700;
  color: #0f172a;
  font-feature-settings: "tnum";
  letter-spacing: -0.5px;
  line-height: 1.1;
  margin-bottom: 4px;
  word-break: break-all;
}
.live-unit {
  font-size: 11px;
  font-weight: 500;
  color: #94a3b8;
  margin-left: 2px;
}
.live-sub {
  font-size: 10.5px;
  color: #64748b;
  line-height: 1.4;
  word-break: break-all;
}

/* 中等屏幕：6 列 → 3 列 */
@media (max-width: 1280px) {
  .live-grid { grid-template-columns: repeat(3, 1fr); }
}
/* 窄屏：3 列 → 2 列 */
@media (max-width: 720px) {
  .live-grid { grid-template-columns: repeat(2, 1fr); }
}

/* ===== 进程控制面板 ===== */
.control-section {
  margin-bottom: 18px;
  padding: 16px 18px;
  background: linear-gradient(135deg, #fff7ed, #fef3c7);
  border: 1px solid #fcd34d;
  border-radius: 12px;
  box-shadow: 0 1px 2px rgba(15,23,42,0.03);
}
.control-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 12px;
}
.control-title {
  font-size: 13px;
  font-weight: 600;
  color: #78350f;
  letter-spacing: 0.3px;
}
.control-title::before {
  content: '⚙';
  margin-right: 6px;
  font-size: 14px;
}
.control-meta { font-size: 11px; color: #92400e; font-family: ui-monospace, "Cascadia Code", "Consolas", monospace; }
.ctrl-meta-ok { color: #15803d; }
.ctrl-meta-warn {
  color: #b91c1c;
  background: #fee2e2;
  border: 1px solid #fecaca;
  padding: 2px 8px;
  border-radius: 4px;
  font-weight: 600;
}
.ctrl-group {
  margin-bottom: 14px;
  padding: 12px 14px;
  background: #f8fafc;
  border: 1px solid #e2e8f0;
  border-radius: 8px;
}
.ctrl-group:last-child { margin-bottom: 0; }
.ctrl-group-head {
  display: flex;
  align-items: baseline;
  gap: 12px;
  margin-bottom: 10px;
}
.ctrl-group-title {
  font-size: 13px;
  font-weight: 700;
  color: #0f172a;
  letter-spacing: 0.3px;
}
.ctrl-group-sub {
  font-size: 11px;
  color: #64748b;
}
.control-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 10px;
}
.ctrl-btn {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 12px;
  background: #fff;
  border: 1px solid #e5e7eb;
  border-radius: 10px;
  cursor: pointer;
  transition: all 150ms ease;
  text-align: left;
  font: inherit;
  color: inherit;
  min-height: 64px;
  position: relative;
  overflow: hidden;
}
.ctrl-btn:hover:not(:disabled) {
  transform: translateY(-1px);
  box-shadow: 0 4px 12px rgba(15,23,42,0.08);
}
.ctrl-btn:active:not(:disabled) {
  transform: translateY(0);
}
.ctrl-btn:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
.ctrl-icon {
  font-size: 18px;
  width: 32px;
  height: 32px;
  border-radius: 8px;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  color: #fff;
  box-shadow: 0 2px 6px rgba(15,23,42,0.10);
}
.ctrl-blue   .ctrl-icon { background: linear-gradient(135deg, #60a5fa, #2563eb); }
.ctrl-cyan   .ctrl-icon { background: linear-gradient(135deg, #22d3ee, #0891b2); }
.ctrl-yellow .ctrl-icon { background: linear-gradient(135deg, #fbbf24, #d97706); }
.ctrl-green  .ctrl-icon { background: linear-gradient(135deg, #34d399, #059669); }
.ctrl-teal   .ctrl-icon { background: linear-gradient(135deg, #5eead4, #14b8a6); }
.ctrl-purple .ctrl-icon { background: linear-gradient(135deg, #c084fc, #7c3aed); }
.ctrl-red    .ctrl-icon { background: linear-gradient(135deg, #f87171, #dc2626); }
.ctrl-body { min-width: 0; flex: 1; }
.ctrl-label {
  font-size: 13px;
  font-weight: 600;
  color: #0f172a;
  margin-bottom: 2px;
  display: flex;
  align-items: center;
  gap: 6px;
}
/* 有未导出改动时的强调样式 */
.ctrl-cyan-dirty {
  border-color: #f59e0b;
  background: #fffbeb;
  box-shadow: 0 0 0 3px rgba(245, 158, 11, 0.14);
}
.ctrl-cyan-dirty .ctrl-label { color: #b45309; }
.ctrl-dirty-tag {
  padding: 1px 6px;
  border-radius: 4px;
  background: #f59e0b;
  color: #fff;
  font-size: 10.5px;
  font-weight: 700;
  white-space: nowrap;
}
:global(.dark) .ctrl-cyan-dirty {
  background: rgba(245, 158, 11, 0.12);
  border-color: #f59e0b;
}
:global(.dark) .ctrl-cyan-dirty .ctrl-label { color: #fbbf24; }
.ctrl-sub {
  font-size: 10.5px;
  color: #64748b;
  line-height: 1.35;
}
</style>
