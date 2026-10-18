<script setup lang="ts">
import { ref, onMounted, reactive } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { listDevices, createDevice, deleteDevice, updateDevice, type Device } from '@/api/devices'
import { listChannels, type Channel } from '@/api/channels'
import IconButton from '@/components/IconButton.vue'
import Icon from '@/components/Icon.vue'

const router = useRouter()
const devices = ref<Device[]>([])
const channels = ref<Channel[]>([])
const loading = ref(false)
const search = ref('')

const enabledSaving = ref<Record<number, boolean>>({})
const devDialog = ref(false)
const devForm = reactive({ device_biz_id: '', name: '', enabled: true })

async function refresh() {
  loading.value = true
  try {
    const [r, c] = await Promise.all([listDevices(), listChannels()])
    devices.value = r.items
    channels.value = c.items.filter(x => x.role === 'collect')
    applyFilter()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '加载失败')
  } finally {
    loading.value = false
  }
}

const filteredDevices = ref<Device[]>([])
function applyFilter() {
  const kw = search.value.trim().toLowerCase()
  if (!kw) { filteredDevices.value = devices.value; return }
  filteredDevices.value = devices.value.filter(d =>
    d.device_biz_id.toLowerCase().includes(kw) ||
    d.name.toLowerCase().includes(kw)
  )
}
onMounted(async () => {
  await refresh()
  applyFilter()
})

function openCreateDevice() {
  Object.assign(devForm, { device_biz_id: '', name: '', enabled: true })
  devDialog.value = true
}

async function onCreateDevice() {
  if (!devForm.device_biz_id || !devForm.name) { ElMessage.warning('请填写业务ID和名称'); return }
  try {
    await createDevice(devForm)
    ElMessage.success('已创建')
    devDialog.value = false
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '创建失败')
  }
}

async function onDeleteDevice(d: Device) {
  try {
    await deleteDevice(d.id)
    ElMessage.success('已删除')
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '删除失败')
  }
}

async function onToggleEnabled(row: Device, val: boolean | string | undefined) {
  const next = Boolean(val)
  if (next === row.enabled) return
  enabledSaving.value[row.id] = true
  const prev = row.enabled
  row.enabled = next
  try {
    await updateDevice(row.id, {
      device_biz_id: row.device_biz_id,
      name: row.name,
      enabled: next
    })
    ElMessage.success(next ? '已启用' : '已停用')
  } catch (e: any) {
    row.enabled = prev
    ElMessage.error(e?.message || '更新失败')
  } finally {
    enabledSaving.value[row.id] = false
  }
}

function openDetail(d: Device) { router.push(`/devices/${d.id}`) }
const totalCount = () => devices.value.length
const enabledCount = () => devices.value.filter(d => d.enabled).length
</script>

<template>
  <div class="device-page">
    <!-- 顶部摘要卡 -->
    <div class="summary-row">
      <div class="summary-card">
        <div class="summary-label">设备总数</div>
        <div class="summary-value">{{ totalCount() }}</div>
        <div class="summary-icon icon-blue">
          <Icon name="cpu" :size="20" color="#fff" />
        </div>
      </div>
      <div class="summary-card">
        <div class="summary-label">已启用</div>
        <div class="summary-value text-green">{{ enabledCount() }}</div>
        <div class="summary-icon icon-green">
          <Icon name="check-circle" :size="20" color="#fff" />
        </div>
      </div>
      <div class="summary-card">
        <div class="summary-label">已停用</div>
        <div class="summary-value text-gray">{{ totalCount() - enabledCount() }}</div>
        <div class="summary-icon icon-gray">
          <Icon name="x-circle" :size="20" color="#fff" />
        </div>
      </div>
      <div class="summary-card">
        <div class="summary-label">采集通道</div>
        <div class="summary-value">{{ channels.length }}</div>
        <div class="summary-icon icon-purple">
          <Icon name="share" :size="20" color="#fff" />
        </div>
      </div>
    </div>

    <!-- 主表卡 -->
    <div class="card-block">
      <div class="card-toolbar">
        <div class="toolbar-left">
          <el-input v-model="search" placeholder="业务ID / 名称" clearable style="width:220px" @input="applyFilter">
            <template #prefix><Icon name="search" :size="14" color="#94a3b8" /></template>
          </el-input>
        </div>
        <div class="toolbar-right">
          <IconButton name="refresh" tip="刷新" :loading="loading" @click="refresh" />
          <IconButton name="plus" type="primary" tip="新建设备" :disabled="!channels.length" @click="openCreateDevice" />
        </div>
      </div>

      <el-table :data="filteredDevices" v-loading="loading" row-key="id" :empty-text="'暂无设备'" class="data-table" :header-cell-style="{ background: '#fafbfc', color: '#4b5563', fontWeight: '600' }">
        <el-table-column type="index" label="#" width="54" align="center" />
        <el-table-column label="设备" min-width="240">
          <template #default="{ row }">
            <div class="device-cell">
              <div class="device-avatar">{{ row.device_biz_id[0]?.toUpperCase() || 'D' }}</div>
              <div class="device-meta">
                <div class="device-name">{{ row.name }}</div>
                <div class="device-id">{{ row.device_biz_id }}</div>
              </div>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="140" align="center">
          <template #default="{ row }">
            <div class="state-toggle" :class="{ 'is-loading': enabledSaving[row.id] }">
              <el-radio-group :model-value="row.enabled" :disabled="enabledSaving[row.id]" size="small" @update:model-value="onToggleEnabled(row, $event)">
                <el-radio-button :value="true"><span class="state-dot state-on"></span>启用</el-radio-button>
                <el-radio-button :value="false"><span class="state-dot state-off"></span>停用</el-radio-button>
              </el-radio-group>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="96" align="center">
          <template #default="{ row }">
            <IconButton name="eye" type="primary" text tip="查看详情" size="small" :icon-size="14" @click="openDetail(row)" />
            <el-popconfirm title="确认删除设备（级联删除Sources/Points/Targets）?" @confirm="onDeleteDevice(row)">
              <template #reference><IconButton name="trash" type="danger" text tip="删除" size="small" :icon-size="14" no-tooltip /></template>
            </el-popconfirm>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <!-- 新建设备对话框 -->
    <el-dialog v-model="devDialog" title="新建设备" width="480px" align-center>
      <el-form :model="devForm" label-width="100px" label-position="right">
        <el-form-item label="业务ID" required>
          <el-input v-model="devForm.device_biz_id" placeholder="建议小写字母+数字" />
        </el-form-item>
        <el-form-item label="名称" required>
          <el-input v-model="devForm.name" />
        </el-form-item>
        <el-form-item label="启用">
          <el-switch v-model="devForm.enabled" />
        </el-form-item>
      </el-form>
      <template #footer>
        <IconButton name="x" tip="取消" @click="devDialog = false" />
        <IconButton name="check" type="primary" tip="创建" @click="onCreateDevice" />
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.device-page { max-width: 1200px; margin: 0 auto; }
/* ===== 顶部摘要卡 ===== */
.summary-row { display: grid; grid-template-columns: repeat(4, 1fr); gap: 14px; margin-bottom: 18px; }
.summary-card { position: relative; background: #fff; border-radius: 10px; padding: 14px 18px; box-shadow: 0 1px 2px rgba(0,0,0,0.04); border: 1px solid #eef0f5; overflow: hidden; }
.summary-label { font-size: 12px; color: #6b7280; font-weight: 500; }
.summary-value { font-size: 24px; font-weight: 700; color: #111827; margin-top: 4px; letter-spacing: -0.5px; font-feature-settings: "tnum"; line-height: 1.2; }
.summary-icon { position: absolute; top: 14px; right: 16px; width: 32px; height: 32px; border-radius: 8px; display: flex; align-items: center; justify-content: center; color: #fff; }
.icon-blue   { background: linear-gradient(135deg, #3b82f6, #2563eb); }
.icon-green  { background: linear-gradient(135deg, #10b981, #059669); }
.icon-gray   { background: linear-gradient(135deg, #94a3b8, #64748b); }
.icon-purple { background: linear-gradient(135deg, #a855f7, #7c3aed); }
.text-green { color: #059669; }
.text-gray  { color: #6b7280; }
/* ===== 主卡 ===== */
.card-block { background: #fff; border-radius: 10px; border: 1px solid #eef0f5; box-shadow: 0 1px 2px rgba(0,0,0,0.04); overflow: hidden; }
.card-toolbar { display: flex; justify-content: space-between; align-items: center; padding: 14px 16px; border-bottom: 1px solid #f1f3f6; }
.toolbar-left { display: flex; gap: 8px; align-items: center; }
.toolbar-right { display: flex; gap: 8px; align-items: center; }
.data-table { border: none !important; }
.data-table :deep(.el-table__inner-wrapper::before) { display: none; }
.data-table :deep(.el-table__cell) { padding: 6px 0; }
.data-table :deep(.el-table__row) { transition: background 100ms; }
.data-table :deep(.el-table__row:hover > td) { background: #f8fafc !important; }
.data-table :deep(.el-table__cell) { padding: 6px 0; }
.data-table :deep(.el-table__row) td .cell { line-height: 1.3; }
/* ===== 设备列 ===== */
.device-cell { display: flex; align-items: center; gap: 10px; padding: 2px 0; }
.device-avatar { width: 30px; height: 30px; border-radius: 8px; background: linear-gradient(135deg, #e0f2fe 0%, #bae6fd 100%); color: #0369a1; font-weight: 700; display: flex; align-items: center; justify-content: center; font-size: 13px; flex-shrink: 0; }
.device-meta { min-width: 0; display: flex; align-items: center; gap: 8px; line-height: 1.2; }
.device-name { font-size: 13px; font-weight: 600; color: #111827; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 220px; }
.device-id { font-size: 11px; color: #6b7280; font-family: ui-monospace, "JetBrains Mono", Menlo, monospace; padding: 2px 6px; background: #f1f5f9; border-radius: 4px; white-space: nowrap; }
/* ===== 状态点 ===== */
.state-dot { display: inline-block; width: 6px; height: 6px; border-radius: 50%; margin-right: 5px; vertical-align: 1px; }
.state-on  { background: #10b981; box-shadow: 0 0 0 2px rgba(16,185,129,0.18); }
.state-off { background: #9ca3af; }
.state-toggle { display: inline-flex; align-items: center; justify-content: center; white-space: nowrap; }
.state-toggle :deep(.el-radio-button__inner) { padding: 5px 10px !important; font-size: 12px !important; }
</style>