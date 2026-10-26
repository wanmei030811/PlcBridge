<script setup lang="ts">
import { ArrowDown, Upload } from '@element-plus/icons-vue'
import { ref, onMounted, computed, reactive, onUnmounted, defineAsyncComponent } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import {
  getDevice, type Device,
  createSource, updateSource, deleteSource, type Source,
  createPoint, updatePoint, deletePoint, type Point,
  updateDevice, deleteDevice
} from '@/api/devices'
import { batchWrite, type BatchWriteItem, type BatchWriteResp } from '@/api/write'
import { listChannels, type Channel } from '@/api/channels'
import { listProtocols, type ProtocolCapability } from '@/api/protocols'
import {
  createTarget, updateTarget, deleteTarget
} from '@/api/targets'
import {
  fetchTargetsDiag,
  diagLevel,
  diagStatusText,
  diagStatusTip,
  diagSentCount,
  diagErrorCount,
  diagQueueRatio,
  type TargetDiag,
} from '@/api/diag'
import type { Target } from '@/api/devices'
import Icon from '@/components/Icon.vue'
import IconButton from '@/components/IconButton.vue'
// ECharts 体积较大，懒加载避免拖慢设备详情页首屏
const TrendChart = defineAsyncComponent(() => import('@/components/TrendChart.vue'))
import { exportPointsUrl, downloadFile } from '@/api/devices'

const csvImportDialogOpen = ref(false)
const csvImportSourceBizId = ref('')
const csvImportFile = ref<File | null>(null)
const csvImportText = ref('')
const csvImportMode = ref<'file' | 'text'>('file')
const csvImportLoading = ref(false)
const csvImportResult = ref<{ imported: number; failed: number; skipped: number; errors: any[] } | null>(null)

async function onCsvImport() {
  if (!csvImportSourceBizId.value) {
    ElMessage.warning('请选择目标 Source')
    return
  }
  if (csvImportMode.value === 'file' && !csvImportFile.value) {
    ElMessage.warning('请选择 CSV 文件')
    return
  }
  if (csvImportMode.value === 'text' && !csvImportText.value.trim()) {
    ElMessage.warning('请输入 CSV 内容')
    return
  }
  csvImportLoading.value = true
  csvImportResult.value = null
  try {
    const { importPoints } = await import('@/api/devices')
    const csv = csvImportMode.value === 'file'
      ? csvImportFile.value!
      : csvImportText.value.trim()
    const resp = await importPoints(csvImportSourceBizId.value, csv)
    csvImportResult.value = resp
    ElMessage.success(`导入完成：成功 ${resp.imported}，跳过 ${resp.skipped}，失败 ${resp.failed}`)
    if (resp.imported > 0) refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '导入失败')
  } finally {
    csvImportLoading.value = false
  }
}

function downloadPointsCsv(sourceBizId: string) {
  if (!sourceBizId) return
  downloadFile(exportPointsUrl({ source_biz_id: sourceBizId }), `points_${sourceBizId}.csv`)
}

// === 克隆设备 ===
const cloneDialogOpen = ref(false)
const cloneForm = reactive({ new_device_biz_id: '', new_device_name: '', biz_id_prefix: 'clone_' })
const cloneLoading = ref(false)
function openCloneDialog() {
  cloneForm.new_device_biz_id = ''
  cloneForm.new_device_name = device.value?.name ? `${device.value.name} (副本)` : ''
  cloneForm.biz_id_prefix = 'clone_'
  cloneDialogOpen.value = true
}
async function onClone() {
  if (!cloneForm.new_device_biz_id.trim()) {
    ElMessage.warning('请输入新设备 ID')
    return
  }
  cloneLoading.value = true
  try {
    const { cloneDevice } = await import('@/api/devices')
    await cloneDevice(device.value!.id, cloneForm)
    ElMessage.success('克隆成功')
    cloneDialogOpen.value = false
    router.push('/devices')
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '克隆失败')
  } finally {
    cloneLoading.value = false
  }
}

const route = useRoute()
const router = useRouter()
const id = computed(() => Number(route.params.id))

const device = ref<Device | null>(null)
const channels = ref<Channel[]>([])
const collectChannels = computed(() => channels.value.filter(c => c.role === 'collect'))
const forwardChannels = computed(() => channels.value.filter(c => c.role === 'forward'))

// forward Target 的能力声明（来自 /api/v1/protocols 的 forward 列表）。
//
// 用途：判断某个 Target 是否支持历史趋势查询（capabilities.supports_history）。
// 由后端 Target 插件自报（Go 端 target.Capability），前端不再硬编码 type 白名单。
const targetCapabilities = ref<Record<string, ProtocolCapability>>({})

const loading = ref(false)
// 防止组件卸载后异步写入组件状态
let unmounted = false
let refreshSeq = 0

// loadTargetCapabilities 拉取 forward Target 的能力声明（supports_history 等）。
//
// 失败降级为空对象 → 所有 Target 都不显示「历史趋势」按钮（保守，不会误开弹窗）。
// 不抛错：协议元信息拉不到不该让整个设备详情页挂掉。
async function loadTargetCapabilities(): Promise<void> {
	try {
		const r = await listProtocols()
		if (unmounted) return
		const map: Record<string, ProtocolCapability> = {}
		for (const p of r.forward ?? []) {
			if (p?.name) map[p.name] = p.capabilities ?? ({} as ProtocolCapability)
		}
		targetCapabilities.value = map
	} catch (e: any) {
		if (unmounted) return
		ElMessage.warning('Target 能力元信息拉取失败，历史趋势按钮可能不显示')
	}
}

async function refresh() {
  const seq = ++refreshSeq
  loading.value = true
  try {
    const [d, c] = await Promise.all([getDevice(id.value), listChannels()])
    if (unmounted || seq !== refreshSeq) return
    for (const src of d.sources || []) {
      if (!Array.isArray(src.points)) src.points = []
    }
    device.value = d
    channels.value = c.items
    // 首次加载时默认展开所有 Source，让「实时值」列直接可见。
    // el-collapse 的 v-model 存的是展开项的 name（这里是 source.id），
    // 不设置的话面板全是收起的，用户看不到 Point 表格里的实时值。
    //
    // 只在首次加载时设一次：refresh() 也会在每次编辑后调用，
    // 无条件重置会把用户手动折叠的状态冲掉。
    if (!srcExpandedInitDone.value) {
      srcExpandedInitDone.value = true
      srcExpanded.value = (d.sources || []).map((s: Source) => s.id)
    }
  } catch (e: any) {
    if (unmounted || seq !== refreshSeq) return
    ElMessage.error(e?.response?.data?.error || e?.message || '加载失败')
  } finally {
    if (!unmounted && seq === refreshSeq) loading.value = false
  }
}

// ───── Source（弹对话框，新增/编辑统一） ─────
const srcDialogOpen = ref(false)
const editingSourceId = ref<number | null>(null)
const srcFormRef = ref<FormInstance>()
function blankSrcForm() {
  return {
    source_biz_id: '',
    channel_biz_id: '',
    collect_enabled: true,
    write_enabled: false,
    collect_interval_ms: 1000,
    timeout_ms: 300
  }
}
const srcForm = reactive(blankSrcForm())
const srcRules: FormRules = {
  source_biz_id: [{ required: true, message: '请输入 Source ID', trigger: 'blur' }],
  channel_biz_id: [{ required: true, message: '选择 Channel', trigger: 'change' }],
  collect_interval_ms: [{ type: 'number', min: 50, max: 600000, message: '50 ~ 600000 ms', trigger: 'blur' }],
  timeout_ms: [{ type: 'number', min: 10, max: 60000, message: '10 ~ 60000 ms', trigger: 'blur' }]
}
function openCreateSource() {
  editingSourceId.value = null
  Object.assign(srcForm, blankSrcForm())
  srcDialogOpen.value = true
}
function openEditSource(s: Source) {
  editingSourceId.value = s.id
  Object.assign(srcForm, {
    source_biz_id: s.source_biz_id,
    channel_biz_id: s.channel_biz_id,
    collect_enabled: s.collect_enabled,
    write_enabled: s.write_enabled,
    collect_interval_ms: s.collect_interval_ms,
    timeout_ms: s.timeout_ms
  })
  srcDialogOpen.value = true
}
async function onSaveSource() {
  if (!srcFormRef.value) return
  try { await srcFormRef.value.validate() } catch { return }
  try {
    if (editingSourceId.value == null) {
      await createSource({
        source_biz_id: srcForm.source_biz_id,
        channel_biz_id: srcForm.channel_biz_id,
        device_id: id.value,
        collect_enabled: srcForm.collect_enabled,
        write_enabled: srcForm.write_enabled,
        collect_interval_ms: srcForm.collect_interval_ms,
        timeout_ms: srcForm.timeout_ms
      })
      ElMessage.success('已创建 Source')
    } else {
      await updateSource(editingSourceId.value, {
        channel_biz_id: srcForm.channel_biz_id,
        collect_enabled: srcForm.collect_enabled,
        write_enabled: srcForm.write_enabled,
        collect_interval_ms: srcForm.collect_interval_ms,
        timeout_ms: srcForm.timeout_ms
      })
      ElMessage.success('Source 已更新')
    }
    srcDialogOpen.value = false
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.message || '保存失败')
  }
}
async function removeSource(s: Source) {
  try {
    await deleteSource(s.id)
    // 删除 source 后,Point 也要清掉,避免 UI 残留 stale state
    if (s.points) {
      for (const p of s.points) delete ptDrafts[ptKey(p)]
    }
    ElMessage.success('已删除')
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '删除失败')
  }
}

// ───── 新建 Point（弹窗） ─────
interface NewPointForm {
  source_id: number | null
  source_biz_id: string
  point_biz_id: string
  name: string
  address: string
  data_type: string
  collect_enabled: boolean
  write_enabled: boolean
  // 仅 data_type==='string' 时必填 1..55；其他类型忽略（后端强制 0）。
  string_length: number
}
const pointDialogOpen = ref(false)
const pointSubmitting = ref(false)
const pointForm = reactive<NewPointForm>({
  source_id: null,
  source_biz_id: '',
  point_biz_id: '',
  name: '',
  address: '',
  data_type: 'int16',
  collect_enabled: true,
  write_enabled: false,
  string_length: 16,
})
function openCreatePoint(src: Source) {
  pointForm.source_id = src.id
  pointForm.source_biz_id = src.source_biz_id
  pointForm.point_biz_id = ''
  pointForm.name = ''
  pointForm.address = ''
  pointForm.data_type = 'int16'
  pointForm.collect_enabled = true
  pointForm.write_enabled = false
  pointForm.string_length = 16
  pointDialogOpen.value = true
}
async function submitCreatePoint() {
  if (!pointForm.source_id) return
  if (!pointForm.point_biz_id || !pointForm.address) {
    ElMessage.warning('请填写 Point 业务 ID 和 PLC 地址')
    return
  }
  pointSubmitting.value = true
  try {
    // string 类型兜底：避免漏填导致后端报 got 0
    let sl = pointForm.data_type === 'string' ? (pointForm.string_length ?? 0) : 0
    if (pointForm.data_type === 'string' && (!sl || sl < 1 || sl > 55)) {
      sl = 16
    }
    await createPoint({
      source_id: pointForm.source_id,
      point_biz_id: pointForm.point_biz_id,
      name: pointForm.name,
      address: pointForm.address,
      data_type: pointForm.data_type,
      collect_enabled: pointForm.collect_enabled,
      write_enabled: pointForm.write_enabled,
      string_length: sl,
    })
    ElMessage.success('已创建 Point')
    pointDialogOpen.value = false
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '创建失败')
  } finally {
    pointSubmitting.value = false
  }
}

const DATA_TYPES = ['bool','int8','int16','int32','int64','uint8','uint16','uint32','uint64','float32','float64','string']

// ───── 行内 Point 编辑（草稿，按 device 隔离避免切换时残留） ─────
const ptDrafts = reactive<Record<string, Point>>({})
function ptKey(p: Point): string {
  return `${id.value}:${p.id}`
}
// Source 折叠展开：编辑时打开,保存 Point 后不影响 Source
//
// srcExpanded 默认展开所有 Source：实时值列在 Point 表格里，
// 而 Point 表格在 Source 折叠面板内 —— 若默认收起，用户进详情页
// 完全看不到「实时值」在哪（这正是 2026-10-24 反馈的问题）。
const srcExpanded = ref<number[]>([])
/** 是否已经做过"首次默认展开"（避免 refresh() 把用户的手动折叠冲掉）。 */
const srcExpandedInitDone = ref(false)
const srcBlockCollapsed = ref(false)
const tgtBlockCollapsed = ref(false)
function toggleSrcBlock() {
  srcBlockCollapsed.value = !srcBlockCollapsed.value
}
function toggleTgtBlock() {
  tgtBlockCollapsed.value = !tgtBlockCollapsed.value
}
function startEditPoint(p: Point) {
  // string 类型兜底：DB 里存的 string_length=0 时给个默认 16，避免保存时后端报
  // "data_type=string 时 string_length 必须在 1..55 (got 0)"。
  let sl = p.string_length ?? 0
  if (p.data_type === 'string' && (!sl || sl < 1 || sl > 55)) {
    sl = 16
  }
  ptDrafts[ptKey(p)] = { ...p, string_length: sl }
  // 自动展开所在 source
  if (p.source_id && !srcExpanded.value.includes(p.source_id)) {
    srcExpanded.value = [...srcExpanded.value, p.source_id]
  }
}
function cancelEditPoint(p: Point) {
  delete ptDrafts[ptKey(p)]
}
function isDirty(p: Point): boolean {
  const d = ptDrafts[ptKey(p)]
  if (!d) return false
  const pStr = p.string_length ?? 0
  const dStr = d.string_length ?? 0
  return d.name !== p.name
      || d.address !== p.address
      || d.data_type !== p.data_type
      || d.collect_enabled !== p.collect_enabled
      || d.write_enabled !== p.write_enabled
      || dStr !== pStr
}
async function savePoint(p: Point) {
  const d = ptDrafts[ptKey(p)]
  if (!d) return
  // string 类型时强制 string_length 落在 1..55，否则后端会拒
  let sl = d.data_type === 'string' ? (d.string_length ?? 0) : 0
  if (d.data_type === 'string' && (!sl || sl < 1 || sl > 55)) {
    ElMessage.warning('string 类型需填写 1~55 之间的字符串长度')
    return
  }
  try {
    await updatePoint(p.id, {
      point_biz_id: d.point_biz_id,
      name: d.name,
      address: d.address,
      data_type: d.data_type,
      collect_enabled: d.collect_enabled,
      write_enabled: d.write_enabled,
      string_length: sl,
    })
    ElMessage.success('Point 已更新')
    delete ptDrafts[ptKey(p)]
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '保存失败')
  }
}
async function removePoint(p: Point) {
  try {
    await deletePoint(p.id)
    delete ptDrafts[ptKey(p)]
    ElMessage.success('已删除')
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '删除失败')
  }
}

async function duplicatePoint(p: Point) {
  const newId = `${p.point_biz_id}_copy`
  try {
    await createPoint({
      source_id: p.source_id,
      point_biz_id: newId,
      name: p.name,
      address: p.address,
      data_type: p.data_type,
      collect_enabled: p.collect_enabled,
      write_enabled: p.write_enabled,
      string_length: p.data_type === 'string' ? (p.string_length ?? 0) : 0,
    })
    ElMessage.success('已复制 Point（新 ID = ' + newId + '）')
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '复制失败')
  }
}

// ───── Device 顶部信息条编辑 ─────
const editName = ref('')
const editBizId = ref('')
const editEnabled = ref(true)
function startEdit() {
  if (!device.value) return
  editName.value = device.value.name
  editBizId.value = device.value.device_biz_id
  editEnabled.value = device.value.enabled
}
async function saveEdit() {
  if (!device.value) return
  try {
    await updateDevice(id.value, {
      device_biz_id: editBizId.value,
      name: editName.value,
      enabled: editEnabled.value
    })
    ElMessage.success('设备已更新')
    await refresh()
    startEdit()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '保存失败')
  }
}

// ───── 批量写入（Plan2 新增） ─────
const batchDialogOpen = ref(false)
const batchSubmitting = ref(false)
interface BatchRow { point_biz_id: string; data_type: string; value: string }
const batchRows = ref<BatchRow[]>([])

// 聚合该设备所有 write-enabled 的 Point
const batchCandidates = computed<BatchRow[]>(() => {
  const out: BatchRow[] = []
  for (const s of device.value?.sources || []) {
    if (!s.write_enabled) continue
    for (const p of s.points || []) {
      if (!p.write_enabled) continue
      out.push({ point_biz_id: p.point_biz_id, data_type: p.data_type, value: '' })
    }
  }
  return out
})

function placeholderFor(dataType: string): string {
  const map: Record<string, string> = {
    bool: 'true / false',
    int8: '整数，如 42', int16: '整数，如 42', int32: '整数，如 42', int64: '整数，如 42',
    uint8: '非负整数', uint16: '非负整数', uint32: '非负整数', uint64: '非负整数',
    float32: '小数，如 3.14', float64: '小数，如 3.14',
    string: '任意字符串',
  }
  return map[dataType] || '输入值'
}

function openBatchWrite() {
  batchRows.value = batchCandidates.value.map(r => ({ ...r }))
  batchDialogOpen.value = true
}

async function submitBatchWrite() {
  const writes = batchRows.value
    .filter(r => r.value.trim() !== '')
    .map(r => {
      let v: unknown = r.value.trim()
      // 数值类型转换
      if (r.data_type === 'int8' || r.data_type === 'int16' || r.data_type === 'int32' || r.data_type === 'int64') {
        v = parseInt(r.value.trim(), 10)
        if (isNaN(v as number)) { ElMessage.warning(`"${r.point_biz_id}" 值不是有效整数`); return null }
      } else if (r.data_type === 'uint8' || r.data_type === 'uint16' || r.data_type === 'uint32' || r.data_type === 'uint64') {
        v = parseInt(r.value.trim(), 10)
        if (isNaN(v as number) || (v as number) < 0) { ElMessage.warning(`"${r.point_biz_id}" 值不是有效无符号整数`); return null }
      } else if (r.data_type === 'float32' || r.data_type === 'float64') {
        v = parseFloat(r.value.trim())
        if (isNaN(v as number)) { ElMessage.warning(`"${r.point_biz_id}" 值不是有效数字`); return null }
      } else if (r.data_type === 'bool') {
        v = r.value.trim() === 'true' || r.value.trim() === '1' || r.value.trim() === 'ON'
      }
      return { point_biz_id: r.point_biz_id, value: v, value_type: r.data_type } as BatchWriteItem
    })
    .filter(Boolean) as BatchWriteItem[]

  if (writes.length === 0) { ElMessage.warning('请至少填写一个点位的值'); return }
  if (!device.value) return

  batchSubmitting.value = true
  try {
    const r = await batchWrite({
      device_biz_id: device.value.device_biz_id,
      transactional: true,
      writes,
    }) as BatchWriteResp
    if (r.result === 'success') {
      ElMessage.success(`批量写入成功（${writes.length} 个点位）`)
    } else if (r.result === 'partial') {
      const fail = r.writes.filter(w => w.result === 'fail')
      ElMessage.warning(`部分成功 ${writes.length - fail.length}/${writes.length}，失败 ${fail.length} 个`)
    } else {
      const fail = r.writes.filter(w => w.result === 'fail')
      ElMessage.error(`写入失败：${fail.map(w => `${w.point_biz_id}: ${w.error}`).join('；')}`)
    }
    batchDialogOpen.value = false
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '批量写入失败')
  } finally {
    batchSubmitting.value = false
  }
}

// ───── Target CRUD ─────
interface TargetForm {
  target_biz_id: string
  forward_channel_biz_id: string
  topic: string
  format: string
  point_biz_ids: string[]
}
const targetDialogOpen = ref(false)
const editingTargetId = ref<number | null>(null)
const targetForm = ref<TargetForm>(blankTargetForm())

// ───── 历史趋势图（Plan3 3.5）─────
const trendDialogOpen = ref(false)
// field = InfluxDB 的 field key，对应写入侧的 Point.PointBizID。
// 一个 Target 可能关联多个点位 → 每个点位在 InfluxDB 里是独立的 field，
// 查询时必须指定具体是哪个，否则查不到数据。
const trendTarget = ref<{ bizId: string; topic: string; pointBizIds: string[] } | null>(null)

// 支持历史查询的 forward channel 类型。
//
// 数据来源：后端 /api/v1/protocols 的 forward 列表 → capabilities.supports_history。
// 由 Go 端 target.TargetPlugin.Capabilities() 自报（见 internal/infra/target/target.go），
// 后端历史查询接口本身也是按「客户端是否实现 target.HistoryQueryer」判定（internal/app/forward/workers.go），
// 两者同源，所以前端不需要（也不应该）再维护一份 type 白名单。
//
// 拉取失败时降级为空对象 → 所有 Target 都不显示「历史趋势」按钮（保守，不会误开）。
const targetSupportsHistory = (target: { forward_channel_biz_id?: string }): boolean => {
	const ch = channels.value.find(c => c.channel_biz_id === target.forward_channel_biz_id)
	if (!ch) return false
	return targetCapabilities.value[ch.type]?.supports_history === true
}

function openTrend(row: any) {
	trendTarget.value = {
		bizId: row.target_biz_id,
		topic: row.topic || '',
		pointBizIds: (row.point_biz_ids || []).filter((p: string) => p !== '*'),
	}
	trendDialogOpen.value = true
}

function blankTargetForm(): TargetForm {
  return {
    target_biz_id: '',
    forward_channel_biz_id: '',
    topic: '',
    format: 'json',
    point_biz_ids: []
  }
}

// Points 选择候选（聚合自所有 Sources）
const targetAvailablePoints = computed<{ point_biz_id: string; name: string }[]>(() => {
  const outMap = new Map<string, { point_biz_id: string; name: string }>()
  for (const s of device.value?.sources || []) {
    for (const p of s.points || []) {
      outMap.set(p.point_biz_id, { point_biz_id: p.point_biz_id, name: p.name })
    }
  }
  // 兜底：已选 point 不再 device 内时（极端情况）,保留 id 与 label
  for (const id of targetForm.value.point_biz_ids || []) {
    if (!outMap.has(id)) outMap.set(id, { point_biz_id: id, name: id })
  }
  return Array.from(outMap.values())
})

async function openCreateTarget() {
  editingTargetId.value = null
  targetForm.value = blankTargetForm()
  if (forwardChannels.value[0]) {
    targetForm.value.forward_channel_biz_id = forwardChannels.value[0].channel_biz_id
  }
  targetDialogOpen.value = true
}

async function openEditTarget(t: Target) {
  editingTargetId.value = t.id
  targetForm.value = {
    target_biz_id: t.target_biz_id,
    forward_channel_biz_id: t.forward_channel_biz_id,
    topic: t.topic,
    format: t.format,
    point_biz_ids: [...(t.point_biz_ids || [])]
  }
  targetDialogOpen.value = true
}

async function onSaveTarget() {
  if (!targetForm.value.target_biz_id || !targetForm.value.forward_channel_biz_id || !targetForm.value.topic) {
    ElMessage.warning('请填写业务 ID、Forward Channel 和 Topic')
    return
  }
  const payload: Partial<Target> = {
    target_biz_id: targetForm.value.target_biz_id,
    device_id: id.value,
    forward_channel_biz_id: targetForm.value.forward_channel_biz_id,
    topic: targetForm.value.topic,
    format: targetForm.value.format,
    point_biz_ids: targetForm.value.point_biz_ids
  }
  try {
    if (editingTargetId.value) {
      await updateTarget(editingTargetId.value, payload)
      ElMessage.success('Target 已更新')
    } else {
      await createTarget(payload)
      ElMessage.success('Target 已创建')
    }
    targetDialogOpen.value = false
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '保存失败')
  }
}

async function removeTarget(t: Target) {
  try {
    await deleteTarget(t.id)
    ElMessage.success('已删除')
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '删除失败')
  }
}

// === Target 诊断（Plan3 3.4）===
//
// 数据来自 plcbridge 主进程内存里的 forward client cache（web 端跨进程代理）。
// 与实时点位值一样：plcbridge 未运行时拿不到数据，此时静默降级为"暂无诊断数据"，
// 不弹全局错误（避免每 5s 弹一次）。
//
// 轮询节奏 5s：诊断数据的变化频率远低于采集值，没必要 2s 拉一次。
const TARGET_DIAG_POLL_MS = 5000

/** key = target_biz_id */
const targetDiags = ref<Record<string, TargetDiag>>({})
/** plcbridge 不可达时置位（用于在面板上给出明确说明，而不是空白） */
const diagUnavailable = ref(false)
let diagTimer: number | null = null
let diagInFlight = false

async function pollTargetDiags() {
  // 防止上一轮还没回来就叠加下一轮。
  if (diagInFlight) return
  diagInFlight = true
  try {
    const resp = await fetchTargetsDiag()
    if (unmounted) return
    const map: Record<string, TargetDiag> = {}
    for (const t of resp.targets || []) {
      if (t?.target_biz_id) map[t.target_biz_id] = t
    }
    targetDiags.value = map
    diagUnavailable.value = false
  } catch (e: any) {
    if (unmounted) return
    const code = e?.response?.data?.code
    if (code === 'plcbridge_unavailable' || code === 'forward_not_running') {
      diagUnavailable.value = true
    }
  } finally {
    diagInFlight = false
  }
}

function startTargetDiags() {
  stopTargetDiags()
  void pollTargetDiags()
  diagTimer = window.setInterval(() => { void pollTargetDiags() }, TARGET_DIAG_POLL_MS)
}

function stopTargetDiags() {
  if (diagTimer !== null) {
    clearInterval(diagTimer)
    diagTimer = null
  }
}

/** 取某个 Target 的诊断数据（未拉到返回 undefined）。 */
function diagOf(t: Target): TargetDiag | undefined {
  return targetDiags.value[t.target_biz_id]
}

/** Target 表格里的状态标签类型。 */
function diagTagType(t: Target): 'success' | 'danger' | 'info' {
  return diagLevel(diagOf(t))
}

/** Target 表格里的状态文案。 */
function diagTagText(t: Target): string {
  return diagStatusText(diagOf(t))
}

/** Target 表格里状态标签的 tooltip。 */
function diagTagTip(t: Target): string {
  return diagStatusTip(diagOf(t))
}

/** 打开单个 Target 的诊断详情弹窗。 */
const diagDialogOpen = ref(false)
const diagTarget = ref<Target | null>(null)
function openDiagDialog(t: Target) {
  diagTarget.value = t
  diagDialogOpen.value = true
}

/** 诊断详情弹窗里展示的诊断数据。 */
const diagDetail = computed<TargetDiag | undefined>(() => {
  if (!diagTarget.value) return undefined
  return targetDiags.value[diagTarget.value.target_biz_id]
})

/** 诊断弹窗里的协议特定信息行（MQTT / InfluxDB 字段不同）。 */
const diagExtraRows = computed<{ label: string; value: string }[]>(() => {
  const s = diagDetail.value?.stats
  if (!s) return []
  const rows: { label: string; value: string }[] = []
  if (s.broker_url) rows.push({ label: 'Broker 地址', value: s.broker_url })
  if (s.topic) rows.push({ label: 'Topic', value: s.topic })
  if (s.protocol_version) rows.push({ label: '协议版本', value: s.protocol_version })
  if (s.address) rows.push({ label: '服务器地址', value: s.address })
  if (s.db) rows.push({ label: '数据库', value: s.db })
  if (s.org) rows.push({ label: 'Org', value: s.org })
  if (s.bucket) rows.push({ label: 'Bucket', value: s.bucket })
  if (s.version) rows.push({ label: 'InfluxDB 版本', value: s.version })
  if (s.queue_depth != null) rows.push({ label: 'Broker 队列深度', value: String(s.queue_depth) })
  return rows
})

/** 诊断弹窗里的通用指标行。 */
const diagMetricRows = computed<{ label: string; value: string; danger?: boolean }[]>(() => {
  const d = diagDetail.value
  if (!d) return []
  const rows: { label: string; value: string; danger?: boolean }[] = []
  rows.push({ label: '成功发送', value: String(diagSentCount(d)) })
  rows.push({ label: '失败次数', value: String(diagErrorCount(d)), danger: diagErrorCount(d) > 0 })
  rows.push({ label: '累计入队', value: String(d.enqueued_total ?? 0) })
  rows.push({ label: '累计丢弃', value: String(d.dropped_total ?? 0), danger: (d.dropped_total ?? 0) > 0 })
  rows.push({ label: '队列', value: `${d.queue_len ?? 0} / ${d.queue_cap ?? 0}` })
  const last = d.stats?.last_send_ok_ms
  rows.push({ label: '最近成功', value: last ? formatAge(last, Date.now()) : '—' })
  return rows
})

async function onDelete() {
  try {
    await deleteDevice(id.value)
    ElMessage.success('已删除设备')
    location.href = '/devices'
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '删除失败')
  }
}

// === 实时点位值（Plan3 3.2） ===
//
// 数据来自 plcbridge 主进程内存缓存（web 端跨进程代理），故只反映"正在采集"的运行态。
//
// 刷新节奏：每 SOURCE_POLL_MS 拉一次**设备级聚合接口**（一次请求拿全设备的 Source），
// 而不是按 Source 并发 N 次 —— N 个 Source 就 N 倍 HTTP 开销。
//
// 容错：拉取失败不打断页面（静默保留上次数据），只有 plcbridge 未运行时才提示一次。
import {
  fetchDeviceValues,
  formatAge,
  formatPointValue,
  isValueStale,
  type DeviceValuesResp,
  type PointValue,
  type SourceHealth,
} from '@/api/values'

/** 轮询间隔。取 2s：比常见的 1s 采集周期慢一倍，人眼几乎察觉不到延迟，但请求数减半。 */
const SOURCE_POLL_MS = 2000

/** key = source_biz_id */
const liveSources = ref<Record<string, { values: Record<string, PointValue>; updated_at: number; health: SourceHealth }>>({})
const liveDeviceHealth = ref<SourceHealth | null>(null)
/** plcbridge 主进程不可达时置位，避免每 2s 弹一次全局错误 */
const liveUnavailable = ref(false)
/** 每次成功拉取后自增，供模板强制重算"x 前"文案 */
const liveTick = ref(0)

let liveTimer: number | null = null
let liveInFlight = false

/** 当前时刻的 age 判定用的时钟（模板里读 liveTick 以建立依赖）。 */
const liveNow = computed(() => {
  void liveTick.value
  return Date.now()
})

/** 设备整体健康状态文案（后端取最差的那个 Source）。 */
const deviceHealthLabel = computed(() => {
  const h = liveDeviceHealth.value
  if (!h || h.status === 'ok') return ''
  if (h.status === 'error') return '采集异常'
  return '未采集'
})

async function pollLiveValues() {
  // 防止上一轮还没回来就叠加下一轮（弱网/主进程卡住时会雪崩）
  if (liveInFlight) return
  const bizId = device.value?.device_biz_id
  if (!bizId) return
  liveInFlight = true
  try {
    const resp: DeviceValuesResp = await fetchDeviceValues(bizId)
    if (unmounted) return
    liveSources.value = (resp.sources || {}) as typeof liveSources.value
    liveDeviceHealth.value = resp.health || null
    liveUnavailable.value = false
    liveTick.value++
  } catch (e: any) {
    if (unmounted) return
    // 503 plcbridge_unavailable 才提示；其它错误（如网络抖动）静默重试即可
    const code = e?.response?.data?.code
    if (code === 'plcbridge_unavailable' || code === 'collector_not_running') {
      liveUnavailable.value = true
    }
  } finally {
    liveInFlight = false
  }
}

function startLiveValues() {
  stopLiveValues()
  void pollLiveValues()
  liveTimer = window.setInterval(() => { void pollLiveValues() }, SOURCE_POLL_MS)
}

function stopLiveValues() {
  if (liveTimer !== null) {
    clearInterval(liveTimer)
    liveTimer = null
  }
}

/** 取某 Source 的实时数据（未拉到返回 undefined）。 */
function liveOf(src: Source) {
  return liveSources.value[src.source_biz_id]
}

/**
 * 实时值单元格的展示数据。
 *
 * 返回 null 表示"无可展示的值"，模板据此退化成 — / 未采集。
 * 非 null 时 stale=true 表示这个值已经过期（PLC 掉线后缓存里留的旧值）。
 */
function liveCellOf(src: Source, pointBizId: string): { text: string; age: string; stale: boolean } | null {
  const data = liveOf(src)
  if (!data) return null
  const pv = data.values[pointBizId]
  if (!pv) return null
  return {
    text: formatPointValue(pv.value),
    age: formatAge(pv.timestamp, liveNow.value),
    stale: isValueStale(pv, data.health, liveNow.value),
  }
}

/** Source 级健康徽标文案。 */
function srcHealthText(src: Source): string {
  const h = liveOf(src)?.health
  if (!h) return ''
  if (h.status === 'ok') return ''
  if (h.status === 'error') {
    // 连续失败次数可能很大（几小时不修 = 上万次），
    // 徽标只放得下短文本，超过 99 就显示 "99+"，避免撑破布局。
    const n = h.consecutive_errors ?? 0
    if (n <= 1) return '采集失败'
    return `采集失败 ×${n > 99 ? '99+' : n}`
  }
  return '未采集'
}

/**
 * Source 级健康徽标的 tooltip 文案。
 *
 * 徽标上的计数超过 99 会缩写成 "99+"，所以这里补上精确次数，
 * 用户悬停仍能看到准确值。
 */
function srcHealthTooltip(src: Source): string {
  const h = liveOf(src)?.health
  if (!h) return ''
  const err = h.last_error || '该 Source 尚未采集到数据'
  if (h.status === 'error' && (h.consecutive_errors ?? 0) > 99) {
    return `${err}（连续失败 ${h.consecutive_errors} 次）`
  }
  return err
}

/**
 * Source 折叠头部的实时值摘要（形如 "M100=1 · No=\"\""）。
 *
 * 为什么放在折叠头：Point 表格（含实时值列）在折叠面板里，
 * 收起时用户完全看不到采集是否正常。摘要让状态一目了然，
 * 不用逐个点开。
 */
function srcLiveSummary(src: Source): string {
  const data = liveOf(src)
  if (!data) return ''
  const parts: string[] = []
  for (const p of src.points || []) {
    const pv = data.values[p.point_biz_id]
    if (!pv) continue
    parts.push(`${p.point_biz_id}=${formatPointValue(pv.value)}`)
    if (parts.length >= 3) break
  }
  if (!parts.length) return ''
  const more = (src.points || []).length - parts.length
  return parts.join(' · ') + (more > 0 ? ` · +${more}` : '')
}

onMounted(async () => {
  await Promise.all([refresh(), loadTargetCapabilities()])
  startEdit()
  startLiveValues()
  startTargetDiags()
})

onUnmounted(() => {
  unmounted = true
  stopLiveValues()
  stopTargetDiags()
})
</script>

<template>
  <div v-if="device" v-loading="loading" class="device-detail">

    <!-- 顶部信息条 + 顶部操作 -->
    <div class="info-card">
      <div class="info-icon">
        <Icon name="cpu" :size="20" color="#fff" />
      </div>
      <el-input v-model="editBizId" size="default" class="info-input-bizid" placeholder="业务 ID" />
      <el-input v-model="editName" size="default" class="info-input-name" placeholder="设备名称" />
      <div class="info-field info-switch">
        <span class="info-label">启用</span>
        <el-switch v-model="editEnabled" />
      </div>
      <div class="info-actions">
        <IconButton name="refresh" tip="刷新" :loading="loading" @click="refresh" />
        <IconButton name="check" type="primary" tip="保存修改" @click="saveEdit" />
        <el-popconfirm title="确认删除整个设备（含 Sources/Points/Targets）?" @confirm="onDelete" width="280">
          <template #reference>
            <IconButton name="trash" type="danger" tip="删除设备" no-tooltip />
          </template>
        </el-popconfirm>
        <IconButton name="copy" type="success" tip="克隆设备" @click="openCloneDialog" />
      </div>
    </div>

    <!-- 实时值不可用提示：plcbridge 主进程未运行时给出明确说明，
         而不是让"实时值"列一直空白让人误以为是数据为空 -->
    <el-alert
      v-if="liveUnavailable"
      type="warning"
      :closable="false"
      show-icon
      class="live-unavailable-alert"
      title="实时值暂不可用：plcbridge 主进程未运行或无法连接"
      description="实时值来自主进程内存缓存。请在仪表盘确认 plcbridge 已启动；下方实时值列在恢复后会自动刷新。"
    />

    <!-- 数据源 (Source) -->
    <div class="card-block">
      <div class="block-head block-head-inline block-head-toggle" @click="toggleSrcBlock">
        <span class="block-title">
          <el-icon class="block-chevron" :class="{ collapsed: srcBlockCollapsed }"><ArrowDown /></el-icon>
          数据源 (Source) · 共 {{ (device.sources || []).length }} 个
          <el-tag
            v-if="deviceHealthLabel"
            size="small"
            :type="liveDeviceHealth?.status === 'error' ? 'danger' : 'info'"
            effect="plain"
            class="src-health-tag block-title-health"
          >{{ deviceHealthLabel }}</el-tag>
        </span>
        <div class="block-head-actions" @click.stop>
          <IconButton name="plus" type="primary" tip="新建 Source" :disabled="!collectChannels.length" @click="openCreateSource" />
          <IconButton name="upload" tip="导入 CSV" @click="csvImportDialogOpen = true" />
          <IconButton name="download" tip="导出 CSV" @click="downloadPointsCsv(device.sources && device.sources[0]?.source_biz_id || '')" />
          <IconButton name="edit" type="warning" tip="批量写入" :disabled="!batchCandidates.length" @click="openBatchWrite" />
        </div>
      </div>

      <div v-show="!srcBlockCollapsed">
      <el-empty v-if="!device.sources || !device.sources.length" description="暂无 Source，使用上方表单新增" />

      <el-collapse v-if="device.sources && device.sources.length" v-model="srcExpanded" class="src-collapse">
        <el-collapse-item
          v-for="src in device.sources"
          :key="src.id"
          :name="src.id"
          class="src-collapse-item"
        >
          <template #title>
            <div class="src-head">
              <div class="src-head-info">
                <div class="src-index">#{{ device.sources.indexOf(src) + 1 }}</div>
                <div class="src-icon">
                  <Icon name="server" :size="14" />
                </div>
                <div class="src-meta">
                  <span class="src-name">{{ src.source_biz_id }}</span>
                  <span class="src-tag-channel">{{ src.channel_biz_id }}</span>
                  <span class="src-divider">·</span>
                  <span class="src-detail-text">{{ src.collect_interval_ms }}ms · 超时 {{ src.timeout_ms }}ms · {{ (src.points || []).length }} 个 Point</span>
                  <!-- 折叠态也能看到实时值摘要：点开前就能判断这个 Source 是否在正常采集 -->
                  <span v-if="srcLiveSummary(src)" class="src-live-summary">{{ srcLiveSummary(src) }}</span>
                </div>
              </div>
              <div class="src-tags" @click.stop>
                <el-tooltip
                  v-if="srcHealthText(src)"
                  :content="srcHealthTooltip(src)"
                  placement="top"
                >
                  <el-tag
                    size="small"
                    :type="liveOf(src)?.health?.status === 'error' ? 'danger' : 'info'"
                    effect="plain"
                    class="src-health-tag"
                  >
                    {{ srcHealthText(src) }}
                  </el-tag>
                </el-tooltip>
                <span class="src-pill" :class="src.collect_enabled ? 'is-on' : 'is-off'">
                  <i class="src-pill-dot"></i>采集 {{ src.collect_enabled ? 'ON' : 'OFF' }}
                </span>
                <span class="src-pill" :class="src.write_enabled ? 'is-on' : 'is-off'">
                  <i class="src-pill-dot"></i>写入 {{ src.write_enabled ? 'ON' : 'OFF' }}
                </span>
              </div>
              <div class="src-head-actions" @click.stop>
                <IconButton name="plus" type="primary" tip="新建 Point" size="small" :icon-size="13" @click="openCreatePoint(src)" />
                <IconButton name="edit" tip="新建 Source" size="small" :icon-size="13" @click="openEditSource(src)" />
                <el-popconfirm title="删除 Source（级联 Point）?" @confirm="removeSource(src)" width="220">
                  <template #reference>
                    <IconButton name="trash" type="danger" tip="删除" size="small" :icon-size="13" no-tooltip />
                  </template>
                </el-popconfirm>
              </div>
            </div>
          </template>

          <el-table
            :data="src.points"
            size="small"
            stripe
            row-key="id"
            class="pt-table"
            :empty-text="'该 Source 下暂无 Point'"
            :header-cell-style="{ background: '#fafbfc', color: '#4b5563', fontWeight: '600' }"
            :cell-style="{ height: '32px', padding: '0' }"
            :row-style="{ height: '32px' }"
          >
            <el-table-column prop="point_biz_id" label="业务 ID" width="100" />
            <el-table-column label="名称" width="80">
              <template #default="{ row }">
                <el-input v-if="ptDrafts[ptKey(row)]" v-model="ptDrafts[ptKey(row)].name" size="small" />
                <span v-else>{{ row.name || '—' }}</span>
              </template>
            </el-table-column>
            <el-table-column label="地址" width="120">
              <template #default="{ row }">
                <el-input v-if="ptDrafts[ptKey(row)]" v-model="ptDrafts[ptKey(row)].address" size="small" />
                <code v-else style="font-size:12px">{{ row.address }}</code>
              </template>
            </el-table-column>
            <el-table-column label="类型" width="70">
              <template #default="{ row }">
                <el-select v-if="ptDrafts[ptKey(row)]" v-model="ptDrafts[ptKey(row)].data_type" size="small">
                  <el-option v-for="d in DATA_TYPES" :key="d" :label="d" :value="d" />
                </el-select>
                <el-tag v-else size="small" effect="plain">{{ row.data_type }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="字符串长度" width="110">
              <template #default="{ row }">
                <el-input-number
                  v-if="ptDrafts[ptKey(row)] && ptDrafts[ptKey(row)].data_type === 'string'"
                  v-model="ptDrafts[ptKey(row)].string_length"
                  :min="1" :max="55" size="small" :controls="false"
                  style="width:80px"
                />
                <span v-else-if="row.data_type === 'string' && row.string_length">
                  <el-tag size="small" effect="plain">{{ row.string_length }}</el-tag>
                </span>
                <span v-else style="color:#c0c4cc">—</span>
              </template>
            </el-table-column>
            <el-table-column label="采集" width="70" align="center">
              <template #default="{ row }">
                <el-switch v-if="ptDrafts[ptKey(row)]" v-model="ptDrafts[ptKey(row)].collect_enabled" size="small" />
                <el-tag v-else :type="row.collect_enabled ? 'success' : 'info'" effect="plain" size="small">
                  {{ row.collect_enabled ? 'ON' : 'OFF' }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column label="写入" width="70" align="center">
              <template #default="{ row }">
                <el-switch v-if="ptDrafts[ptKey(row)]" v-model="ptDrafts[ptKey(row)].write_enabled" size="small" />
                <el-tag v-else :type="row.write_enabled ? 'warning' : 'info'" effect="plain" size="small">
                  {{ row.write_enabled ? 'ON' : 'OFF' }}
                </el-tag>
              </template>
            </el-table-column>
            <!-- 实时值（Plan3 3.2）：编辑中的行显示占位符，避免"未保存的修改"与"实际值"混淆 -->
            <el-table-column label="实时值" min-width="150">
              <template #default="{ row }">
                <span v-if="ptDrafts[ptKey(row)]" class="live-none">—</span>
                <div
                  v-else
                  class="live-cell"
                  :class="{ 'is-stale': liveCellOf(src, row.point_biz_id)?.stale }"
                >
                  <template v-if="liveCellOf(src, row.point_biz_id)">
                    <span class="live-value">{{ liveCellOf(src, row.point_biz_id)?.text }}</span>
                    <span class="live-age">{{ liveCellOf(src, row.point_biz_id)?.age }}</span>
                  </template>
                  <span v-else-if="!row.collect_enabled" class="live-none">未采集</span>
                  <span v-else class="live-none">—</span>
                </div>
              </template>
            </el-table-column>
            <el-table-column label="操作" width="120" align="right" fixed="right">
              <template #default="{ row }">
                <div class="ops-cell">
                <template v-if="ptDrafts[ptKey(row)]">
                  <IconButton name="check" type="primary" tip="保存" size="small" :icon-size="14" @click="savePoint(row)" />
                  <IconButton name="x" tip="取消" size="small" :icon-size="14" @click="cancelEditPoint(row)" />
                </template>
                <template v-else>
                  <IconButton name="edit" tip="编辑" size="small" :icon-size="14" @click="startEditPoint(row)" />
                  <IconButton name="copy" tip="复制" size="small" :icon-size="14" @click="duplicatePoint(row)" />
                  <el-popconfirm title="删除该 Point?" @confirm="removePoint(row)" width="180">
                    <template #reference>
                      <IconButton name="trash" type="danger" tip="删除" size="small" :icon-size="14" no-tooltip />
                    </template>
                  </el-popconfirm>
                </template>
                </div>
              </template>
            </el-table-column>
          </el-table>

          </el-collapse-item>
      </el-collapse>
      </div>
    </div>

    <!-- 转发目标 (Target) -->
    <div class="card-block">
      <div class="block-head block-head-inline block-head-toggle" @click="toggleTgtBlock">
        <span class="block-title">
          <el-icon class="block-chevron" :class="{ collapsed: tgtBlockCollapsed }"><ArrowDown /></el-icon>
          转发目标 (Target) · 共 {{ (device.targets || []).length }} 个
        </span>
        <div class="block-head-actions" @click.stop>
          <IconButton name="plus" type="primary" tip="新建 Target" :disabled="!forwardChannels.length" @click="openCreateTarget" />
        </div>
      </div>

      <div v-show="!tgtBlockCollapsed">
      <el-empty v-if="!device.targets || !device.targets.length" description="本设备暂无 Target，点击右上角 + 添加" />
      <el-table
        v-else
        :data="device.targets"
        size="small"
        stripe
        :header-cell-style="{ background: '#fafbfc', color: '#4b5563', fontWeight: '600' }"
      >
        <el-table-column prop="target_biz_id" label="业务 ID" width="160" />
        <el-table-column prop="forward_channel_biz_id" label="转发通道" width="140" />
        <el-table-column prop="topic" label="Topic" min-width="180" width="220" show-overflow-tooltip />
        <el-table-column prop="format" label="格式" width="80" />
        <el-table-column label="状态" width="90" align="center">
          <template #default="{ row }">
            <el-tooltip :content="diagTagTip(row)" placement="top" :show-after="200">
              <el-tag size="small" :type="diagTagType(row)" effect="plain">{{ diagTagText(row) }}</el-tag>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="采集" width="80" align="center">
          <template #default="{ row }">
            <el-tag size="small" effect="plain">{{ (row.point_biz_ids || []).length }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="170" align="right" fixed="right">
          <template #default="{ row }">
            <div class="ops-cell">
              <IconButton
                v-if="targetSupportsHistory(row)"
                name="activity"
                tip="历史趋势"
                size="small"
                :icon-size="14"
                @click="openTrend(row)"
              />
              <IconButton name="gauge" tip="诊断" size="small" :icon-size="14" @click="openDiagDialog(row)" />
              <IconButton name="edit" tip="编辑" size="small" :icon-size="14" @click="openEditTarget(row)" />
              <el-popconfirm title="删除该 Target?" @confirm="removeTarget(row)" width="200">
                <template #reference>
                  <IconButton name="trash" type="danger" tip="删除" size="small" :icon-size="14" no-tooltip />
                </template>
              </el-popconfirm>
            </div>
          </template>
        </el-table-column>
      </el-table>
      </div>
    </div>

    <!-- 新建 Point 对话框 -->
    <el-dialog v-model="pointDialogOpen" title="新建 Point" width="520px" align-center>
      <el-form :model="pointForm" label-width="100px">
        <el-form-item label="业务 ID" required>
          <el-input v-model="pointForm.point_biz_id" placeholder="例如:TempSensor_01" maxlength="64" show-word-limit />
        </el-form-item>
        <el-form-item label="名称">
          <el-input v-model="pointForm.name" placeholder="可选,便于识别的中文别名" maxlength="64" />
        </el-form-item>
        <el-form-item label="PLC 地址" required>
          <el-input v-model="pointForm.address" placeholder="例如:DB1.DBD100 / IW0 / Q0.0" />
        </el-form-item>
        <el-form-item label="数据类型">
          <el-select v-model="pointForm.data_type" style="width: 100%">
            <el-option v-for="d in DATA_TYPES" :key="d" :label="d" :value="d" />
          </el-select>
        </el-form-item>
        <el-form-item v-if="pointForm.data_type === 'string'" label="字符串长度" required>
          <el-input-number v-model="pointForm.string_length" :min="1" :max="55" :controls="false" style="width:140px" />
          <span class="form-hint">
            S7 STRING 字符数 N（标准布局总字节 4+N），范围 1..55。<br>
            <b>非标准 PLC 模拟器</b>（如 HslCommunication）：请到「通道 → s7 → STRING 头字节数」改为 1。
          </span>
        </el-form-item>
        <el-form-item label="名称">
          <el-switch v-model="pointForm.collect_enabled" />
          <span class="form-hint">开启后会按 Source 的采集周期读取该点位</span>
        </el-form-item>
        <el-form-item label="名称">
          <el-switch v-model="pointForm.write_enabled" />
          <span class="form-hint">开启后允许通过 API/Target 向该点位写入</span>
        </el-form-item>
      </el-form>
      <template #footer>
        <IconButton name="x" tip="取消" @click="pointDialogOpen = false" />
        <IconButton name="check" type="primary" tip="确定" :loading="pointSubmitting" @click="submitCreatePoint" />
      </template>
    </el-dialog>

    <!-- 编辑 Source 对话框 -->
    <el-dialog
      v-model="srcDialogOpen"
      :title="editingSourceId == null ? '新建 Source' : '编辑 Source'"
      width="560px"
      align-center
    >
      <el-form ref="srcFormRef" :model="srcForm" :rules="srcRules" label-width="140px">
        <el-form-item label="业务 ID" prop="source_biz_id">
          <el-input v-model="srcForm.source_biz_id" :disabled="editingSourceId != null" placeholder="建议小写字母+数字,如 plc01" />
        </el-form-item>
        <el-form-item label="采集通道" prop="channel_biz_id">
          <el-select v-model="srcForm.channel_biz_id" :disabled="editingSourceId != null" style="width:100%">
            <el-option v-for="c in collectChannels" :key="c.id" :label="`${c.channel_biz_id} (${c.type})`" :value="c.channel_biz_id" />
          </el-select>
        </el-form-item>
        <el-form-item label="采集间隔 (ms)" prop="collect_interval_ms">
          <el-input-number v-model="srcForm.collect_interval_ms" :min="50" :max="600000" :step="50" />
        </el-form-item>
        <el-form-item label="超时 (ms)" prop="timeout_ms">
          <el-input-number v-model="srcForm.timeout_ms" :min="10" :max="60000" :step="10" />
        </el-form-item>
        <el-form-item label="数据类型">
          <el-switch v-model="srcForm.collect_enabled" />
        </el-form-item>
        <el-form-item label="数据类型">
          <el-switch v-model="srcForm.write_enabled" />
        </el-form-item>
      </el-form>
      <template #footer>
        <IconButton name="x" tip="取消" @click="srcDialogOpen = false" />
        <IconButton name="check" type="primary" tip="保存" @click="onSaveSource" />
      </template>
    </el-dialog>

    <!-- 新建/编辑 Target 对话框 -->
    <el-dialog
      v-model="targetDialogOpen"
      :title="editingTargetId == null ? '新建 Target' : '编辑 Target'"
      width="640px"
      align-center
    >
      <el-form :model="targetForm" label-width="160px">
        <el-form-item label="业务 ID" required>
          <el-input v-model="targetForm.target_biz_id" placeholder="例如:to-mq-bj-1" :disabled="editingTargetId != null" />
        </el-form-item>
        <el-form-item label="转发通道" required>
          <el-select v-model="targetForm.forward_channel_biz_id" style="width:100%">
            <el-option v-for="c in forwardChannels" :key="c.id" :label="`${c.channel_biz_id} (${c.type})`" :value="c.channel_biz_id" />
          </el-select>
        </el-form-item>
        <el-form-item label="Topic" required>
          <el-input v-model="targetForm.topic" placeholder="例如:plc/line1/values" />
        </el-form-item>
        <el-form-item label="名称">
          <el-radio-group v-model="targetForm.format">
            <el-radio-button value="json">JSON</el-radio-button>
            <el-radio-button value="csv">CSV</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="选择 Points">
          <el-select v-model="targetForm.point_biz_ids" multiple filterable style="width:100%">
            <el-option v-for="p in targetAvailablePoints" :key="p.point_biz_id" :label="`${p.point_biz_id} (${p.name || '—'})`" :value="p.point_biz_id" />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <IconButton name="x" tip="取消" @click="targetDialogOpen = false" />
        <IconButton name="check" type="primary" tip="保存" @click="onSaveTarget" />
      </template>
    </el-dialog>

    <!-- Target 诊断对话框（Plan3 3.4） -->
    <el-dialog
      v-model="diagDialogOpen"
      :title="`Target 诊断 · ${diagTarget?.target_biz_id || ''}`"
      width="620px"
      align-center
    >
      <el-alert
        v-if="diagUnavailable"
        type="warning"
        :closable="false"
        show-icon
        title="Target 诊断数据暂不可用"
        description="plcbridge 主进程未运行或无法连接。诊断数据来自主进程内存缓存，主进程恢复后会自动刷新。"
      />
      <template v-else-if="!diagDetail">
        <el-empty description="暂无诊断数据（该 Target 可能尚未在 plcbridge 中启动）" />
      </template>
      <template v-else>
        <!-- 连接状态卡片 -->
        <div class="diag-status" :class="`is-${diagLevel(diagDetail)}`">
          <div class="diag-status-main">
            <span class="diag-status-dot"></span>
            <span class="diag-status-text">{{ diagStatusText(diagDetail) }}</span>
          </div>
          <div class="diag-status-sub">
            {{ diagDetail.type || '未知协议' }}
            <template v-if="diagDetail.format"> · {{ diagDetail.format }}</template>
          </div>
        </div>

        <!-- 最近错误 -->
        <el-alert
          v-if="diagDetail.last_error"
          type="error"
          :closable="false"
          show-icon
          :title="`最近错误：${diagDetail.last_error}`"
        />

        <!-- 通用指标 -->
        <div class="diag-section-title">实时指标</div>
        <div class="diag-metrics">
          <div v-for="m in diagMetricRows" :key="m.label" class="diag-metric">
            <div class="diag-metric-label">{{ m.label }}</div>
            <div class="diag-metric-value" :class="{ 'is-danger': m.danger }">{{ m.value }}</div>
          </div>
        </div>

        <!-- 队列使用率 -->
        <div class="diag-section-title">队列使用率</div>
        <el-progress
          :percentage="Math.round(diagQueueRatio(diagDetail) * 100)"
          :stroke-width="14"
          :status="diagQueueRatio(diagDetail) > 0.9 ? 'exception' : (diagQueueRatio(diagDetail) > 0.6 ? 'warning' : 'success')"
        />

        <!-- 协议特定信息 -->
        <template v-if="diagExtraRows.length">
          <div class="diag-section-title">协议信息</div>
          <table class="diag-table">
            <tbody>
              <tr v-for="r in diagExtraRows" :key="r.label">
                <td class="diag-table-key">{{ r.label }}</td>
                <td class="diag-table-val">{{ r.value }}</td>
              </tr>
            </tbody>
          </table>
        </template>
      </template>
      <template #footer>
        <IconButton name="x" tip="关闭" @click="diagDialogOpen = false" />
        <IconButton name="refresh" tip="刷新" @click="pollTargetDiags" />
      </template>
    </el-dialog>

    <!-- 历史趋势图对话框（Plan3 3.5） -->    <el-dialog v-model="trendDialogOpen" title="历史趋势" width="860px" align-center>
      <TrendChart
        v-if="trendTarget"
        :target-biz-id="trendTarget.bizId"
        :measurement="trendTarget.topic"
        :field-options="trendTarget.pointBizIds"
        :height="340"
      />
      <template #footer>
        <IconButton name="x" tip="关闭" @click="trendDialogOpen = false" />
      </template>
    </el-dialog>

    <!-- 批量写入对话框 -->
    <el-dialog v-model="batchDialogOpen" title="批量写入" width="640px" align-center>
      <div style="margin-bottom:12px; font-size:13px; color:#64748b">
        对 <b>{{ device?.device_biz_id }}</b> 下所有 write-enabled 的 Point 批量写入值。
        事务模式：中途失败会尝试回滚已写入的点位。
      </div>
      <el-table :data="batchRows" size="small" stripe max-height="360">
        <el-table-column prop="point_biz_id" label="点位 ID" width="160">
          <template #default="{ row }">
            <code style="font-size:12px">{{ row.point_biz_id }}</code>
          </template>
        </el-table-column>
        <el-table-column prop="data_type" label="类型" width="80">
          <template #default="{ row }">
            <el-tag size="small">{{ row.data_type }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="写入值" min-width="200">
          <template #default="{ row }">
            <el-input v-model="row.value" :placeholder="placeholderFor(row.data_type)" size="small" clearable />
          </template>
        </el-table-column>
      </el-table>
      <template #footer>
        <IconButton name="x" tip="取消" @click="batchDialogOpen = false" />
        <IconButton name="check" type="primary" tip="确认写入" :loading="batchSubmitting" @click="submitBatchWrite" />
      </template>
    </el-dialog>

    <!-- CSV 导入对话框 -->
    <el-dialog v-model="csvImportDialogOpen" title="导入点位 CSV" width="560px">
      <el-form label-width="100px" @submit.prevent>
        <el-form-item label="目标 Source">
          <el-select v-model="csvImportSourceBizId" placeholder="选择 Source" style="width: 100%">
            <el-option v-for="s in device?.sources" :key="s.source_biz_id" :label="s.source_biz_id" :value="s.source_biz_id" />
          </el-select>
        </el-form-item>
        <el-form-item label="输入方式">
          <el-radio-group v-model="csvImportMode">
            <el-radio value="file">上传文件</el-radio>
            <el-radio value="text">粘贴内容</el-radio>
          </el-radio-group>
        </el-form-item>
        <el-form-item v-if="csvImportMode === 'file'" label="CSV 文件">
          <el-upload drag :auto-upload="false" :limit="1" accept=".csv,.txt"
            :on-change="(f: any) => csvImportFile = f.raw">
            <el-icon><Upload /></el-icon>
            拖拽或点击上传
          </el-upload>
        </el-form-item>
        <el-form-item v-else label="CSV 内容">
          <el-input v-model="csvImportText" type="textarea" :rows="8" placeholder="粘贴 CSV 内容，第一行必须是表头：point_biz_id,name,address,data_type,collect_enabled,write_enabled" />
        </el-form-item>
        <el-alert v-if="csvImportResult" type="success" :closable="false">
          导入完成：成功 {{ csvImportResult.imported }}，跳过 {{ csvImportResult.skipped }}，失败 {{ csvImportResult.failed }}
        </el-alert>
        <el-alert v-if="csvImportResult?.errors.length" type="danger" style="margin-top: 8px" :closable="false">
          <template v-for="e in csvImportResult.errors.slice(0, 5)" :key="e.row + e.field">
            第 {{ e.row }} 行 · {{ e.field }}：{{ e.message }}<br/>
          </template>
          <span v-if="csvImportResult.errors.length > 5">... 共 {{ csvImportResult.errors.length }} 个错误</span>
        </el-alert>
      </el-form>
      <template #footer>
        <IconButton name="x" tip="取消" @click="csvImportDialogOpen = false" />
        <IconButton name="check" type="primary" tip="导入" :loading="csvImportLoading" @click="onCsvImport" />
      </template>
    </el-dialog>

    <!-- 克隆设备对话框 -->
    <el-dialog v-model="cloneDialogOpen" title="克隆设备" width="480px">
      <el-form label-width="120px" @submit.prevent>
        <el-form-item label="新设备 ID" required>
          <el-input v-model="cloneForm.new_device_biz_id" placeholder="唯一业务 ID" />
        </el-form-item>
        <el-form-item label="新设备名称">
          <el-input v-model="cloneForm.new_device_name" placeholder="设备名称（可选）" />
        </el-form-item>
        <el-form-item label="BizID 前缀">
          <el-input v-model="cloneForm.biz_id_prefix" placeholder="如 clone_ 或 copy_" />
        </el-form-item>
        <p style="color: #6b7280; font-size: 13px; padding: 8px 0">
          将深拷贝设备的所有 Source、Point、Target，Source/Point/Target 的 biz_id 加上前缀以保证唯一性。
        </p>
      </el-form>
      <template #footer>
        <IconButton name="x" tip="取消" @click="cloneDialogOpen = false" />
        <IconButton name="check" type="primary" tip="确认克隆" :loading="cloneLoading" @click="onClone" />
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.device-detail { max-width: 1200px; margin: 0 auto; }

/* ===== 顶部信息卡 ===== */
.info-card {
  display: flex;
  align-items: center;
  gap: 12px;
  background: #fff;
  border: 1px solid #eef0f5;
  border-radius: 10px;
  padding: 12px 18px;
  margin-bottom: 18px;
  box-shadow: 0 1px 2px rgba(0,0,0,0.04);
}
.info-icon {
  width: 36px; height: 36px;
  border-radius: 8px;
  background: linear-gradient(135deg, #3b82f6, #2563eb);
  display: flex; align-items: center; justify-content: center;
  flex-shrink: 0;
}
.info-input-bizid { width: 220px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; flex-shrink: 0; }
.info-input-name { flex: 1; min-width: 0; max-width: 240px; }
.info-field { display: flex; align-items: center; gap: 6px; font-size: 13px; flex-shrink: 0; padding: 0 6px; border-left: 1px solid #eef0f5; }
.info-switch .info-label { color: #6b7280; font-size: 12px; }
.info-actions { display: flex; gap: 6px; align-items: center; flex-shrink: 0; margin-left: 4px; }

/* ===== 通用块 ===== */
.card-block {
  background: #fff;
  border-radius: 10px;
  border: 1px solid #eef0f5;
  box-shadow: 0 1px 2px rgba(0,0,0,0.04);
  overflow: hidden;
  margin-bottom: 18px;
}
.block-head {
  padding: 14px 18px;
  border-bottom: 1px solid #f1f3f6;
  font-size: 14px;
  font-weight: 600;
  color: #111827;
}
.block-head-toggle { cursor: pointer; user-select: none; transition: background .15s; }
.block-head-toggle:hover { background: #f9fafb; }
.block-chevron { transition: transform .2s; vertical-align: -2px; margin-right: 4px; }
.block-chevron.collapsed { transform: rotate(-90deg); }
.block-head-actions { display: flex; gap: 6px; align-items: center; }
.block-head-inline {
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.block-title { font-weight: 600; }

/* ===== Source 折叠 ===== */
.src-collapse :deep(.el-collapse-item__header) {
  padding: 0 18px;
  height: auto;
  border: none;
  background: transparent;
}
.src-collapse :deep(.el-collapse-item__wrap) { border: none; }
.src-collapse :deep(.el-collapse-item__content) { padding: 0 18px 14px; }
.src-collapse-item :deep(.el-collapse-item__header) { padding-left: 0; padding-right: 16px; height: 34px; line-height: 34px; min-height: 34px; }
.src-collapse-item :deep(.el-collapse-item__header:hover) { background: #f8fafc; }
.src-collapse-item :deep(.el-collapse-item__header.is-active) { border-bottom: none; }
.src-collapse-item :deep(.el-collapse-item__wrap) { border-bottom: none; }

.src-head {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 0;
  min-height: 0;
  height: 34px;
  line-height: 34px;
}
.src-head-info { display: flex; align-items: center; gap: 8px; min-width: 0; flex: 1 1 auto; overflow: hidden; }
.src-index {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 11px;
  color: #94a3b8;
  background: #f1f5f9;
  padding: 0 6px;
  border-radius: 4px;
  flex-shrink: 0;
  min-width: 28px;
  text-align: center;
}
.src-icon {
  width: 22px; height: 22px;
  border-radius: 5px;
  background: linear-gradient(135deg, #ede9fe, #ddd6fe);
  color: #6d28d9;
  display: flex; align-items: center; justify-content: center;
  flex-shrink: 0;
}
.src-meta { min-width: 0; display: flex; align-items: center; gap: 8px; line-height: 1; overflow: hidden; white-space: nowrap; flex-wrap: nowrap; }
.src-name { font-weight: 600; color: #111827; font-size: 13px; line-height: 1; flex-shrink: 0; }
.src-tag-channel {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 10px;
  padding: 2px 6px;
  border-radius: 4px;
  background: #eef2ff;
  color: #4338ca;
  border: 1px solid #c7d2fe;
  min-width: 56px;
  text-align: center;
  line-height: 1.2;
  letter-spacing: 0.3px;
  flex-shrink: 0;
}
.src-divider { color: #d1d5db; font-size: 12px; flex-shrink: 0; user-select: none; }
.src-detail-text { font-size: 11px; color: #6b7280; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0; line-height: 1; }
.src-meta-divider { color: #d1d5db; font-size: 12px; user-select: none; }

.src-tags { display: flex; gap: 6px; flex-shrink: 0; }
.src-pill {
  display: inline-flex; align-items: center; gap: 5px;
  height: 20px; padding: 0 8px;
  border-radius: 999px;
  font-size: 11px; line-height: 1; font-weight: 600;
  white-space: nowrap;
  border: 1px solid transparent;
  letter-spacing: 0.2px;
}
.src-pill-dot { width: 6px; height: 6px; border-radius: 50%; flex-shrink: 0; }
.src-pill.is-on  { background: rgba(16,185,129,0.12); color: #047857; border-color: rgba(16,185,129,0.3); }
.src-pill.is-on .src-pill-dot { background: #10b981; box-shadow: 0 0 0 2px rgba(16,185,129,0.18); }
.src-pill.is-off { background: rgba(148,163,184,0.14); color: #64748b; border-color: rgba(148,163,184,0.25); }
.src-pill.is-off .src-pill-dot { background: #94a3b8; }
.src-health-tag { height: 20px; line-height: 18px; font-size: 11px; font-weight: 600; }
.block-title-health { margin-left: 8px; vertical-align: 2px; }

/* Source 折叠头部的实时值摘要 */
.src-live-summary {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 11px; color: #0f766e;
  background: rgba(13,148,136,0.08);
  border: 1px solid rgba(13,148,136,0.2);
  border-radius: 4px; padding: 1px 6px;
  margin-left: 8px; white-space: nowrap;
  max-width: 320px; overflow: hidden; text-overflow: ellipsis;
}

/* === 实时值列（Plan3 3.2） === */
.live-cell {
  display: flex; align-items: baseline; gap: 6px;
  min-width: 0; padding: 0 2px;
}
.live-value {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px; font-weight: 600; color: #1f2937;
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 110px;
}
.live-age { font-size: 11px; color: #9ca3af; white-space: nowrap; flex-shrink: 0; }
.live-none { font-size: 12px; color: #c0c4cc; }

/* 过期值：PLC 掉线后缓存里留的旧值，必须与新鲜值视觉上区分开，
   否则用户会把 10 分钟前的数字当成当前值。 */
.live-cell.is-stale .live-value { color: #b45309; }
.live-cell.is-stale .live-age { color: #d97706; font-weight: 600; }

.live-unavailable-alert { margin: 12px 0; }
.src-head-actions { display: flex; gap: 3px; flex-shrink: 0; padding-left: 8px; border-left: 1px solid #e5e7eb; margin-left: 8px; }
.src-collapse-item :deep(.src-head-actions .el-button.icon-btn) { padding: 4px; min-height: 24px; }

/* ===== Point ? ===== */
.pt-table :deep(.el-table__inner-wrapper::before) { display: none; }
.pt-table :deep(.el-table__cell) { padding: 6px 4px; }
.pt-table :deep(.el-input--small .el-input__wrapper) { padding: 1px 8px; }
.pt-table :deep(.el-select .el-input__wrapper) { padding: 1px 8px; }
.ops-cell { display: flex; gap: 4px; justify-content: flex-end; align-items: center; }
.pt-table .ops-cell .icon-btn { padding: 8px; }

/* ===== 表单提示 ===== */
.form-hint {
  margin-left: 10px;
  font-size: 12px;
  color: #94a3b8;
}

/* ===== Target 诊断面板（Plan3 3.4） ===== */
.diag-status {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 14px;
  border-radius: 8px;
  margin-bottom: 14px;
  border: 1px solid #e5e7eb;
  background: #f9fafb;
}
.diag-status.is-success { border-color: rgba(16,185,129,0.35); background: rgba(16,185,129,0.07); }
.diag-status.is-danger  { border-color: rgba(239,68,68,0.35);  background: rgba(239,68,68,0.07); }
.diag-status.is-info    { border-color: #e5e7eb; background: #f3f4f6; }
.diag-status-main { display: flex; align-items: center; gap: 8px; }
.diag-status-dot {
  width: 10px; height: 10px; border-radius: 50%;
  background: #94a3b8; flex-shrink: 0;
}
.diag-status.is-success .diag-status-dot { background: #10b981; box-shadow: 0 0 0 3px rgba(16,185,129,0.18); }
.diag-status.is-danger .diag-status-dot  { background: #ef4444; box-shadow: 0 0 0 3px rgba(239,68,68,0.18); }
.diag-status-text { font-size: 15px; font-weight: 700; color: #1f2937; }
.diag-status-sub { font-size: 12px; color: #6b7280; }

.diag-section-title {
  font-size: 12px;
  font-weight: 600;
  color: #4b5563;
  margin: 16px 0 8px;
}
.diag-metrics {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 8px;
}
.diag-metric {
  border: 1px solid #e5e7eb;
  border-radius: 6px;
  padding: 8px 10px;
  background: #fff;
}
.diag-metric-label { font-size: 11px; color: #6b7280; }
.diag-metric-value {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 15px;
  font-weight: 700;
  color: #1f2937;
  margin-top: 2px;
}
.diag-metric-value.is-danger { color: #dc2626; }

.diag-table { width: 100%; border-collapse: collapse; font-size: 12px; }
.diag-table td { border-bottom: 1px solid #f3f4f6; padding: 6px 8px; vertical-align: top; }
.diag-table-key { color: #6b7280; width: 140px; white-space: nowrap; }
.diag-table-val {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  color: #1f2937;
  word-break: break-all;
}
:global(.dark) .diag-status { border-color: #334155; background: #1e293b; }
:global(.dark) .diag-status.is-success { border-color: rgba(16,185,129,0.4); background: rgba(16,185,129,0.12); }
:global(.dark) .diag-status.is-danger { border-color: rgba(239,68,68,0.4); background: rgba(239,68,68,0.12); }
:global(.dark) .diag-status.is-info { border-color: #334155; background: #1e293b; }
:global(.dark) .diag-status-text { color: #e2e8f0; }
:global(.dark) .diag-status-sub { color: #94a3b8; }
:global(.dark) .diag-section-title { color: #cbd5e1; }
:global(.dark) .diag-metric { border-color: #334155; background: #1e293b; }
:global(.dark) .diag-metric-label { color: #94a3b8; }
:global(.dark) .diag-metric-value { color: #e2e8f0; }
:global(.dark) .diag-metric-value.is-danger { color: #f87171; }
:global(.dark) .diag-table td { border-color: #0f172a; }
:global(.dark) .diag-table-key { color: #94a3b8; }
:global(.dark) .diag-table-val { color: #e2e8f0; }
</style>
