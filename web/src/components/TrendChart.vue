<script setup lang="ts">
/**
 * 历史趋势图（Plan3 3.5）。
 * 使用 ECharts 绘制 InfluxDB 历史数据折线图。
 */
import { ref, computed, onMounted, onUnmounted, watch, shallowRef } from 'vue'
import * as echarts from 'echarts/core'
import { LineChart } from 'echarts/charts'
import {
  GridComponent, TooltipComponent, LegendComponent,
  DataZoomComponent, ToolboxComponent, MarkLineComponent,
} from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import { fetchTargetHistory, type HistoryPoint } from '@/api/history'
import { ElMessage } from 'element-plus'

echarts.use([
  LineChart, GridComponent, TooltipComponent, LegendComponent,
  DataZoomComponent, ToolboxComponent, MarkLineComponent, CanvasRenderer,
])

const props = defineProps<{
  targetBizId: string
  measurement: string
  field?: string
  /** 可选的 field 候选列表（= Target 关联的点位业务 ID）。非空时显示下拉选择器。 */
  fieldOptions?: string[]
  duration?: string
  height?: number
}>()

const emit = defineEmits<{
  (e: 'loaded', count: number): void
}>()

const chartRef = ref<HTMLDivElement>()
const chart = shallowRef<echarts.ECharts>()
const loading = ref(false)
const points = ref<HistoryPoint[]>([])
const errorMsg = ref('')

const duration = ref(props.duration || '1h')

// === 自动刷新（2026-10-21 新增）===
//
// 之前只有手动点「刷新」按钮才重新拉数据，图表是死的——
// 现场盯着趋势图等数据变化只能反复手点。
//
// 现在默认开启轮询，间隔可选手动选择：
//   - 关闭：不轮询（仅手动刷新）
//   - 5s / 10s / 30s / 60s：轮询间隔
//
// 两个关键细节：
//   1) 页面不可见时暂停（visibilitychange）。这个图在 el-dialog 里，
//      弹窗被其它标签页盖住时还在轮询纯属浪费。
//   2) 轮询刷新不显示 loading 遮罩。否则每 5s 图表就被遮一下，
//      反而看不清数据。loading 只用于首次加载和手动刷新。
const autoRefresh = ref(true)
const refreshIntervalSec = ref(10)

// 正在请求中：避免上一轮没回来就发下一轮（InfluxDB 查询不便宜）
let inFlight = false
// 组件是否已卸载：防止请求返回后 setState 报错
let disposed = false
let timer: number | null = null

// 当前选中的 field。
// 写入侧把 Point.PointBizID 当作 InfluxDB 的 field key，
// 所以这里的候选项就是点位业务 ID。
const selectedField = ref(props.field || '')

// fieldOptions 变化时同步默认选中项（取第一个）。
watch(
  () => props.fieldOptions,
  (opts) => {
    if (props.field) {
      selectedField.value = props.field
      return
    }
    if (opts && opts.length > 0 && !opts.includes(selectedField.value)) {
      selectedField.value = opts[0]
    }
  },
  { immediate: true },
)

function fmtTime(ms: number) {
  const d = new Date(ms)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

function isDark() {
  return document.documentElement.classList.contains('dark')
}

function buildOption() {
  const dark = isDark()
  const axisColor = dark ? '#475569' : '#d1d5db'
  const textColor = dark ? '#94a3b8' : '#6b7280'
  const splitColor = dark ? '#1e293b' : '#f0f0f0'

  const data = points.value.map(p => [p.t, p.v])

  return {
    grid: { left: 56, right: 20, top: 36, bottom: 56 },
    tooltip: {
      trigger: 'axis',
      backgroundColor: dark ? '#1e293b' : '#fff',
      borderColor: dark ? '#334155' : '#e5e7eb',
      textStyle: { color: dark ? '#e2e8f0' : '#1f2937', fontSize: 12 },
      axisPointer: { type: 'line' },
    },
    legend: { show: false },
    toolbox: {
      show: true,
      right: 8,
      top: 4,
      iconStyle: { borderColor: textColor },
      feature: {
        dataZoom: { yAxisIndex: 'none', title: { zoom: '框选缩放', back: '还原缩放' } },
        restore: { title: '重置' },
        saveAsImage: { title: '保存图片', backgroundColor: dark ? '#1e293b' : '#fff' },
      },
    },
    xAxis: {
      type: 'time',
      axisLine: { lineStyle: { color: axisColor } },
      axisLabel: { color: textColor, fontSize: 11, hideOverlap: true },
      splitLine: { show: false },
    },
    yAxis: {
      type: 'value',
      axisLine: { show: false },
      axisLabel: { color: textColor, fontSize: 11 },
      splitLine: { lineStyle: { color: splitColor } },
    },
    dataZoom: [
      { type: 'inside', throttle: 60 },
      {
        type: 'slider', height: 18, bottom: 12,
        backgroundColor: dark ? '#0f172a' : '#fafafa',
        borderColor: dark ? '#334155' : '#e5e7eb',
        fillerColor: 'rgba(16,185,129,0.15)',
        handleStyle: { color: '#10b981' },
        textStyle: { color: textColor, fontSize: 10 },
      },
    ],
    series: [{
      type: 'line',
      showSymbol: false,
      smooth: true,
      sampling: 'lttb',
      data,
      lineStyle: { width: 2, color: '#10b981' },
      itemStyle: { color: '#10b981' },
      areaStyle: {
        color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
          { offset: 0, color: 'rgba(16,185,129,0.28)' },
          { offset: 1, color: 'rgba(16,185,129,0.02)' },
        ]),
      },
    }],
  }
}

function render() {
  if (!chart.value) return
  chart.value.setOption(buildOption(), true)
}

/**
 * 拉取历史数据。
 *
 * @param silent 静默刷新（轮询用）。静默模式下不显示 loading 遮罩，
 *               避免每 N 秒把图表闪一下；错误也保留上一次的数据不清空，
 *               免得网络抖一下图表就空了。
 */
async function load(silent = false) {
  if (!props.targetBizId || !props.measurement) {
    points.value = []
    render()
    return
  }
  // 上一轮请求还没回来就跳过这一轮，避免请求堆积。
  if (inFlight) return
  inFlight = true

  if (!silent) {
    loading.value = true
    errorMsg.value = ''
  }
  try {
    const r = await fetchTargetHistory(props.targetBizId, {
      measurement: props.measurement,
      field: selectedField.value || undefined,
      duration: duration.value,
      limit: 2000,
    })
    if (disposed) return
    points.value = r.points || []
    emit('loaded', points.value.length)
    errorMsg.value = ''
    render()
  } catch (e: any) {
    if (disposed) return
    // 静默刷新失败时保留旧数据 + 旧错误提示，不清空图表。
    if (!silent) {
      errorMsg.value = e?.response?.data?.msg || e?.message || '查询历史数据失败'
      points.value = []
      render()
    }
  } finally {
    inFlight = false
    if (!silent && !disposed) loading.value = false
  }
}

function resize() {
  chart.value?.resize()
}

// === 轮询调度 ===

function stopTimer() {
  if (timer !== null) {
    window.clearInterval(timer)
    timer = null
  }
}

function startTimer() {
  stopTimer()
  if (!autoRefresh.value) return
  timer = window.setInterval(() => {
    // 页面不可见时跳过这一轮（回到前台时立刻补一次）。
    if (document.hidden) return
    void load(true)
  }, refreshIntervalSec.value * 1000)
}

// 页面可见性变化：切回前台时立即补一次数据。
function onVisibilityChange() {
  if (!document.hidden && autoRefresh.value) {
    void load(true)
  }
}

onMounted(() => {
  if (chartRef.value) {
    chart.value = echarts.init(chartRef.value, undefined, { renderer: 'canvas' })
  }
  void load()
  startTimer()
  window.addEventListener('resize', resize)
  document.addEventListener('visibilitychange', onVisibilityChange)
})

onUnmounted(() => {
  disposed = true
  stopTimer()
  window.removeEventListener('resize', resize)
  document.removeEventListener('visibilitychange', onVisibilityChange)
  chart.value?.dispose()
  chart.value = undefined
})

// 切换 field / 时间窗口 / target 时立即重新拉一次。
watch(() => [props.targetBizId, props.measurement, selectedField.value, duration.value], () => {
  void load()
})
// 自动刷新开关 / 间隔变化时重启定时器。
watch([autoRefresh, refreshIntervalSec], startTimer)
// 主题切换时重绘
watch(() => isDark(), render)
</script>

<template>
  <div class="trend-chart">
    <div class="chart-toolbar">
      <span class="chart-title">
        历史趋势
        <span class="chart-sub">{{ measurement || '—' }}{{ selectedField ? ' / ' + selectedField : '' }}</span>
      </span>
      <div class="chart-actions">
        <el-select
          v-if="fieldOptions && fieldOptions.length > 0"
          v-model="selectedField"
          size="small"
          style="width:130px;"
          placeholder="选择点位"
        >
          <el-option v-for="f in fieldOptions" :key="f" :label="f" :value="f" />
        </el-select>
        <el-select v-model="duration" size="small" style="width:110px;">
          <el-option label="最近 15 分钟" value="15m" />
          <el-option label="最近 1 小时" value="1h" />
          <el-option label="最近 6 小时" value="6h" />
          <el-option label="最近 24 小时" value="24h" />
          <el-option label="最近 3 天" value="3d" />
          <el-option label="最近 7 天" value="7d" />
        </el-select>
        <!-- 自动刷新开关 + 间隔（2026-10-21 新增） -->
        <el-tooltip content="自动刷新" placement="top">
          <el-switch
            v-model="autoRefresh"
            size="small"
            inline-prompt
            active-text="自动"
            inactive-text="手动"
            style="--el-switch-on-color: #10b981;"
          />
        </el-tooltip>
        <el-select
          v-if="autoRefresh"
          v-model="refreshIntervalSec"
          size="small"
          style="width:78px;"
        >
          <el-option label="5 秒" :value="5" />
          <el-option label="10 秒" :value="10" />
          <el-option label="30 秒" :value="30" />
          <el-option label="60 秒" :value="60" />
        </el-select>
        <el-button size="small" :loading="loading" @click="load()">刷新</el-button>
      </div>
    </div>

    <div v-loading="loading" class="chart-body" :style="{ height: (height || 280) + 'px' }">
      <div ref="chartRef" class="chart-canvas"></div>
      <div v-if="!loading && errorMsg" class="chart-overlay error">
        <span>{{ errorMsg }}</span>
      </div>
      <div v-else-if="!loading && points.length === 0" class="chart-overlay">
        <span>暂无历史数据</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.trend-chart {
  background: var(--card-bg, #fff);
  border: 1px solid var(--card-border, #f0f0f0);
  border-radius: 10px;
  padding: 12px 14px 8px;
}
.chart-toolbar {
  display: flex; align-items: center; justify-content: space-between;
  margin-bottom: 6px;
}
.chart-title { font-size: 13px; font-weight: 600; color: var(--el-text-color-primary, #111827); }
.chart-sub { font-weight: 400; font-size: 11px; color: var(--el-text-color-secondary, #9ca3af); margin-left: 6px; }
.chart-actions { display: flex; gap: 8px; align-items: center; }
.chart-body { position: relative; width: 100%; }
.chart-canvas { width: 100%; height: 100%; }
.chart-overlay {
  position: absolute; inset: 0;
  display: flex; align-items: center; justify-content: center;
  font-size: 12px; color: var(--el-text-color-secondary, #9ca3af);
  pointer-events: none;
}
.chart-overlay.error { color: #ef4444; }
</style>
