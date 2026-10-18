<script setup lang="ts">
import { ArrowDown } from '@element-plus/icons-vue'
import { ref, onMounted, computed, reactive, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import {
  getDevice, type Device,
  createSource, updateSource, deleteSource, type Source,
  createPoint, updatePoint, deletePoint, type Point,
  updateDevice, deleteDevice
} from '@/api/devices'
import { listChannels, type Channel } from '@/api/channels'
import {
  createTarget, updateTarget, deleteTarget
} from '@/api/targets'
import type { Target } from '@/api/devices'
import Icon from '@/components/Icon.vue'
import IconButton from '@/components/IconButton.vue'

const route = useRoute()
const router = useRouter()
const id = computed(() => Number(route.params.id))

const device = ref<Device | null>(null)
const channels = ref<Channel[]>([])
const collectChannels = computed(() => channels.value.filter(c => c.role === 'collect'))
const forwardChannels = computed(() => channels.value.filter(c => c.role === 'forward'))

const loading = ref(false)
// 防止组件卸载后异步写入组件状态
let unmounted = false
let refreshSeq = 0
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
const srcExpanded = ref<number[]>([])
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

async function onDelete() {
  try {
    await deleteDevice(id.value)
    ElMessage.success('已删除设备')
    location.href = '/devices'
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '删除失败')
  }
}

onMounted(async () => {
  await refresh()
  startEdit()
})

onUnmounted(() => {
  unmounted = true
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
      </div>
    </div>

    <!-- 数据源 (Source) -->
    <div class="card-block">
      <div class="block-head block-head-inline block-head-toggle" @click="toggleSrcBlock">
        <span class="block-title">
          <el-icon class="block-chevron" :class="{ collapsed: srcBlockCollapsed }"><ArrowDown /></el-icon>
          数据源 (Source) · 共 {{ (device.sources || []).length }} 个
        </span>
        <div class="block-head-actions" @click.stop>
          <IconButton name="plus" type="primary" tip="新建 Source" :disabled="!collectChannels.length" @click="openCreateSource" />
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
                </div>
              </div>
              <div class="src-tags" @click.stop>
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
        <el-table-column label="采集" width="80" align="center">
          <template #default="{ row }">
            <el-tag size="small" effect="plain">{{ (row.point_biz_ids || []).length }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="100" align="right" fixed="right">
          <template #default="{ row }">
            <div class="ops-cell">
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
</style>
