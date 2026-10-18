<script setup lang="ts">
import { ref, computed, onMounted, onBeforeUnmount, reactive, watch } from 'vue'
import { ElMessage, ElPopconfirm, type FormInstance, type FormRules } from 'element-plus'
import { listChannels, createChannel, updateChannel, deleteChannel, getChannelsRuntime, type Channel, type ChannelRuntime } from '@/api/channels'
import IconButton from '@/components/IconButton.vue'

const items = ref<Channel[]>([])
const loading = ref(false)
const dialogOpen = ref(false)
const editing = ref<Channel | null>(null)
const formRef = ref<FormInstance>()
const submitting = ref(false)

// S7 专用：STRING 头字节数（与 form.config JSON 中的 string_layout 双向同步）
//   null   = 未配置（后端默认 4 = 标准 S7 STRING）
//   4      = 标准 S7 STRING [2B maxLen][2B curLen][N chars]
//   2      = 短头            [2B curLen][N chars]
//   1      = Hsl 异型        [1B curLen][N chars]
//   0      = 无头裸字节      [N chars]
const stringLayout = ref<number | null>(null)
const STRING_LAYOUT_OPTIONS = [
  { value: null, label: '默认（标准 S7 STRING[4]）' },
  { value: 4, label: '4 = 标准 S7 STRING' },
  { value: 2, label: '2 = 短头 STRING' },
  { value: 1, label: '1 = Hsl 异型 STRING' },
  { value: 0, label: '0 = 无头裸字节' }
]

// === 各协议默认 JSON 模板（仅在用户未编辑时自动填充，便于上手） ===
// InfluxDB V2.x 模板：后端默认值（timeout/batch/flush/precision）已由 ParseConfig 兜底，
// 这里只放"用户必须改"的关键字段 + 一行注释提示。
const INFLUXDB_CONFIG_TEMPLATE = '{\n' +
  '  "version": "2.0",\n' +
  '  "org": "",\n' +
  '  "bucket": "",\n' +
  '  "token": "",\n' +
  '  "connection_timeout_ms": 2000,\n' +
  '  "batch_size": 1000,\n' +
  '  "flush_interval_ms": 1000,\n' +
  '  "precision": "ns"\n' +
  '}'
const S7_CONFIG_PLACEHOLDER = '{\n  "rack": 0,\n  "slot": 1\n}'

const form = reactive({
  channel_biz_id: '',
  role: 'collect' as 'collect' | 'forward',
  type: 's7',
  address: '',
  config: '',
  heartbeat_address: '',
  heartbeat_max_interval_ms: 5000,
  heartbeat_enabled: false,
  heartbeat_debounce_ms: 50,
  queue_size: 10000,
  timeout_ms: 300
})

const TYPES_COLLECT = ['s7', 'modbus', 'opcua']
const TYPES_FORWARD = ['mqtt', 'influxdb']
const typeOptions = computed(() => form.role === 'collect' ? TYPES_COLLECT : TYPES_FORWARD)
const isS7 = computed(() => form.type === 's7')
const isInfluxDB = computed(() => form.type === 'influxdb')

/**
 * 协议 → 默认 JSON 模板 的映射。
 * 仅在 form.config 当前为空 / 仍为占位符 '{}' 时填入，避免覆盖用户已编辑内容。
 */
function defaultConfigFor(t: string): string | null {
  if (t === 'influxdb') return INFLUXDB_CONFIG_TEMPLATE
  return null
}

// === 运行时连接状态（collect + forward 通道）===
// PR1：forward 通道也通过 /plcbridge/channels/runtime 暴露状态（之前是 "—" 占位）。
//      通用字段 connected/last_error + 协议特定 stats["connection_state"]。
const runtimeMap = ref<Record<string, ChannelRuntime>>({})
let runtimeTimer: number | null = null

function statusOf(c: Channel): { tag: 'success' | 'warning' | 'danger' | 'info'; label: string; tip: string } {
  const rt = runtimeMap.value[c.channel_biz_id]
  if (!rt) {
    return { tag: 'info', label: '—', tip: '暂无运行时状态（首次轮询未完成）' }
  }
  // 通用判断：connected + 无 error → ON
  if (rt.connected && !rt.last_error) {
    return { tag: 'success', label: 'ON', tip: tipForConnected(c, rt) }
  }
  // ON 但有最近错误：黄色
  if (rt.connected && rt.last_error) {
    return { tag: 'warning', label: 'ON', tip: `已连接但有最近错误: ${rt.last_error}` }
  }
  // OFF：有错误 → 红
  if (rt.last_error) {
    return { tag: 'danger', label: 'OFF', tip: `未连接: ${rt.last_error}` }
  }
  return { tag: 'info', label: 'OFF', tip: '未连接' }
}

function tipForConnected(c: Channel, rt: ChannelRuntime): string {
  // 协议特定 tip（PR2 扩展：显示 read/write/error 计数）
  const stats = rt.stats
  if (c.role === 'forward' && stats) {
    const sentBatch = stats['sent_count'] || 0
    const sentPoints = stats['sent_points'] || 0
    const errCount = stats['error_count'] || 0
    if (c.type === 'mqtt') {
      return `已连接 · 协议 ${stats['protocol_version'] || 'v5'} · broker ${stats['broker_url'] || c.address} · 已发 ${sentBatch} 批 / ${sentPoints} 条${errCount > 0 ? ` · 失败 ${errCount}` : ''}`
    }
    if (c.type === 'influxdb') {
      return `已连接 · 版本 ${stats['version'] || '2.0'} · ${stats['org'] || ''}/${stats['bucket'] || stats['db'] || ''} · 已写 ${sentBatch} 批 / ${sentPoints} 点${errCount > 0 ? ` · 失败 ${errCount}` : ''}`
    }
  }
  if (c.role === 'collect' && stats) {
    const readCnt = stats['read_count'] || 0
    const writeCnt = stats['write_count'] || 0
    const errCnt = stats['error_count'] || 0
    if (c.type === 's7') {
      return `已连接 · rack=${stats['rack']} slot=${stats['slot']} · 读 ${readCnt} · 写 ${writeCnt}${errCnt > 0 ? ` · 失败 ${errCnt}` : ''} · 最近活动 ${formatAge(rt.last_active_ms)}`
    }
    if (c.type === 'opcua') {
      return `已连接 · ${stats['endpoint'] || c.address} · 订阅 ${stats['subscription_count'] || 0} 项 · 读 ${readCnt} · 写 ${writeCnt}${errCnt > 0 ? ` · 失败 ${errCnt}` : ''}`
    }
    if (c.type === 'modbus') {
      return `已连接 · unit=${stats['unit_id']} · 读 ${readCnt} · 写 ${writeCnt}${errCnt > 0 ? ` · 失败 ${errCnt}` : ''} · 最近活动 ${formatAge(rt.last_active_ms)}`
    }
  }
  return `已连接 · 最近活动 ${formatAge(rt.last_active_ms)}`
}

function formatAge(ms: number): string {
  if (!ms) return '—'
  const age = Date.now() - ms
  if (age < 1000) return '刚刚'
  if (age < 60_000) return `${Math.floor(age / 1000)} 秒前`
  if (age < 3_600_000) return `${Math.floor(age / 60_000)} 分钟前`
  return `${Math.floor(age / 3_600_000)} 小时前`
}

async function refreshRuntime() {
  try {
    const r = await getChannelsRuntime()
    if (r.ok) runtimeMap.value = r.channels
  } catch {
    // 静默：保留旧状态，下一次轮询重试
  }
}

function startRuntimePolling() {
  if (runtimeTimer) return
  void refreshRuntime()
  // 端到端 ON/OFF 状态变更要求 < 1s 内反映。
  // 后端 web 缓存 300ms + plcbridge 探测 ≤ 1s + 网络 RTT ≈ 500ms 足够 1Hz 覆盖。
  // 之前 3s 间隔会让最坏情况拖到 3+ 秒，体感很差。
  runtimeTimer = window.setInterval(refreshRuntime, 1000)
}

function stopRuntimePolling() {
  if (runtimeTimer) {
    window.clearInterval(runtimeTimer)
    runtimeTimer = null
  }
}

// 表单校验规则
const rules: FormRules = {
  channel_biz_id: [
  { required: true, message: '请填写业务 ID', trigger: 'blur' },
  { pattern: /^[a-zA-Z0-9_-]{1,64}$/, message: '仅允许字母/数字/_-，长度 1-64', trigger: 'blur' }
  ],
  role: [{ required: true, message: '请选择角色', trigger: 'change' }],
  type: [{ required: true, message: '请选择协议', trigger: 'change' }],
  address: [
    { required: true, message: '请填写地址', trigger: 'blur' },
    { max: 256, message: '地址长度不超过 256', trigger: 'blur' }
  ],
  config: [
    {
      validator: (_rule, value, callback) => {
        if (!value || value.trim() === '') return callback()
        try {
          JSON.parse(value)
          callback()
        } catch (e) {
          callback(new Error('协议配置必须是合法 JSON: ' + (e as Error).message))
        }
      },
      trigger: 'blur'
    }
  ],
  queue_size: [{ type: 'number', min: 100, max: 1000000, message: '100 ~ 1000000', trigger: 'change' }],
  timeout_ms: [{ type: 'number', min: 10, max: 60000, message: '10 ~ 60000', trigger: 'change' }]
}

async function refresh() {
  loading.value = true
  try {
    const r = await listChannels()
    items.value = r.items
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = null
  Object.assign(form, {
    channel_biz_id: '',
    role: 'collect',
    type: 's7',
    address: '',
    config: '{}',
    heartbeat_address: '',
    heartbeat_max_interval_ms: 5000,
    heartbeat_enabled: false,
    heartbeat_debounce_ms: 50,
    queue_size: 10000,
    timeout_ms: 300
  })
  stringLayout.value = null
  dialogOpen.value = true
}

function openEdit(c: Channel) {
  editing.value = c
  // 显式 pick 字段，避免服务端额外字段（含 undefined）污染 reactive form
  Object.assign(form, {
    channel_biz_id: c.channel_biz_id,
    role: c.role,
    type: c.type,
    address: c.address,
    config: c.config ?? '{}',          // 后端 null → 防止 JSON.parse 报错
    heartbeat_address: c.heartbeat_address ?? '',
    heartbeat_max_interval_ms: c.heartbeat_max_interval_ms ?? 5000,
    heartbeat_enabled: c.heartbeat_enabled ?? false,
    heartbeat_debounce_ms: c.heartbeat_debounce_ms ?? 50,
    queue_size: c.queue_size ?? 10000,
    timeout_ms: c.timeout_ms ?? 300,
  })
  // 解析 S7 string_layout（仅 type=s7 时）
  stringLayout.value = readStringLayoutFromConfig(c.config)
  dialogOpen.value = true
}

function readStringLayoutFromConfig(configJson: string | null | undefined): number | null {
  if (!configJson) return null
  try {
    const obj = JSON.parse(configJson)
    if (obj && typeof obj === 'object' && Object.prototype.hasOwnProperty.call(obj, 'string_layout')) {
      const v = obj.string_layout
      if (v === 0 || v === 1 || v === 2 || v === 4) return v
    }
  } catch { /* 非法 JSON 时忽略，保留默认 null */ }
  return null
}

function writeStringLayoutToConfig(): void {
  // 合并 stringLayout 到 form.config；非 s7 通道不处理
  if (!isS7.value) return
  let obj: Record<string, unknown> = {}
  try { obj = JSON.parse(form.config || '{}') } catch { obj = {} }
  if (stringLayout.value === null) {
    // 用户选了"默认" → 移除字段（让后端走 sentinel 路径 → 默认 4）
    delete obj.string_layout
  } else {
    obj.string_layout = stringLayout.value
  }
  form.config = JSON.stringify(obj, null, 2)
}

// 用户在 STRING 下拉框里改选项时，实时写回 form.config（避免漏保存）
watch(stringLayout, () => {
  if (dialogOpen.value && isS7.value) writeStringLayoutToConfig()
})

// 用户切换协议到 influxdb 且当前 config 仍是空白/'{}' 时，自动填默认模板
watch(() => form.type, (newType) => {
  if (!dialogOpen.value) return
  const tpl = defaultConfigFor(newType)
  if (tpl === null) return
  const cur = (form.config ?? '').trim()
  if (cur === '' || cur === '{}') form.config = tpl
})

async function onSubmit() {
  if (!formRef.value) return
  try {
    await formRef.value.validate()
  } catch {
    return
  }
  submitting.value = true
  try {
    if (editing.value) {
      await updateChannel(editing.value.id, form)
      ElMessage.success('已更新')
    } else {
      await createChannel(form)
      ElMessage.success('已创建')
    }
    dialogOpen.value = false
    await refresh()
  } catch {
    // 错误已由 axios 拦截器弹 ElMessage
  } finally {
    submitting.value = false
  }
}

async function onDelete(c: Channel) {
  try {
    await deleteChannel(c.id)
    ElMessage.success('已删除')
    await refresh()
  } catch {
    // 错误已由 axios 拦截器弹 ElMessage
  }
}

onMounted(() => {
  refresh()
  startRuntimePolling()
})

onBeforeUnmount(stopRuntimePolling)
</script>

<template>
  <div>
    <div style="display:flex; justify-content:space-between; margin-bottom:16px">
      <h2 style="margin:0">通道（Channel）</h2>
      <div>
        <IconButton name="refresh" tip="刷新" :loading="loading" @click="refresh" />
        <IconButton name="plus" type="primary" tip="新建通道" @click="openCreate" />
      </div>
    </div>

    <el-table :data="items" v-loading="loading" stripe>
      <el-table-column prop="channel_biz_id" label="业务 ID" width="160" />
      <el-table-column prop="role" label="角色" width="100">
        <template #default="{ row }">
          <el-tag :type="row.role === 'collect' ? 'success' : 'warning'">{{ row.role }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="type" label="协议" width="100" />
      <el-table-column prop="address" label="地址" min-width="180" width="260" show-overflow-tooltip />
      <el-table-column label="状态" width="80">
        <template #default="{ row }">
          <el-tooltip :content="statusOf(row).tip" placement="top" :show-after="200">
            <el-tag :type="statusOf(row).tag" size="small">{{ statusOf(row).label }}</el-tag>
          </el-tooltip>
        </template>
      </el-table-column>
      <el-table-column label="心跳" width="80">
        <template #default="{ row }">
          <el-tag v-if="row.heartbeat_enabled" type="success" size="small">ON</el-tag>
          <el-tag v-else type="info" size="small">OFF</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="100" align="right">
        <template #default="{ row }">
          <IconButton name="edit" type="primary" text tip="编辑" :icon-size="14" @click="openEdit(row)" />
          <el-popconfirm title="确认删除?" @confirm="onDelete(row)">
            <template #reference>
              <IconButton name="trash" type="danger" text tip="删除" :icon-size="14" no-tooltip />
            </template>
          </el-popconfirm>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog
      v-model="dialogOpen"
      :title="editing ? '编辑通道' : '新建通道'"
      width="720px"
    >
      <el-form ref="formRef" :model="form" :rules="rules" label-width="140px">
        <el-form-item label="业务 ID" prop="channel_biz_id">
          <el-input v-model="form.channel_biz_id" :disabled="!!editing" />
        </el-form-item>
        <el-form-item label="角色" prop="role">
          <el-radio-group v-model="form.role" :disabled="!!editing">
            <el-radio-button value="collect">采集</el-radio-button>
            <el-radio-button value="forward">转发</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="协议" prop="type">
          <el-select v-model="form.type" :disabled="!!editing">
            <el-option v-for="t in typeOptions" :key="t" :label="t" :value="t" />
          </el-select>
        </el-form-item>
        <el-form-item label="地址" prop="address">
          <el-input v-model="form.address" placeholder="192.168.1.10:102 / tcp://broker:1883 / http://..." />
        </el-form-item>
        <el-form-item label="协议配置 (JSON)" prop="config">
          <el-input v-model="form.config" type="textarea" :rows="6"
            :placeholder="isInfluxDB ? INFLUXDB_CONFIG_TEMPLATE : S7_CONFIG_PLACEHOLDER" />
          <div v-if="isInfluxDB" style="margin-top:4px; color:#909399; font-size:12px">
            切换协议到 influxdb 时会自动填入默认模板；用户编辑后不会被覆盖
          </div>
        </el-form-item>
        <el-form-item v-if="isS7" label="STRING 头字节数">
          <el-select v-model="stringLayout" placeholder="默认（标准 S7 STRING[4]）" style="width:280px" clearable>
            <el-option v-for="o in STRING_LAYOUT_OPTIONS" :key="String(o.value)" :label="o.label" :value="o.value" />
          </el-select>
          <span style="margin-left:8px; color:#909399; font-size:12px">
            非标准 PLC 模拟器（如 HslCommunication）需把 STRING 头字节数改为 1
          </span>
        </el-form-item>
        <el-form-item label="队列大小" prop="queue_size">
          <el-input-number v-model="form.queue_size" :min="100" :max="1000000" />
        </el-form-item>
        <el-form-item label="超时 (ms)" prop="timeout_ms">
          <el-input-number v-model="form.timeout_ms" :min="10" :max="60000" />
        </el-form-item>

        <el-divider content-position="left">心跳检测（可选，用于采集通道存活监控）</el-divider>
        <el-form-item label="启用心跳">
          <el-switch v-model="form.heartbeat_enabled" />
        </el-form-item>
        <template v-if="form.heartbeat_enabled">
          <el-form-item label="心跳地址">
            <el-input v-model="form.heartbeat_address" placeholder="DB1.DBX0.0 / coil 1 / ns=2;s=Channel1.Device1.Tag" />
          </el-form-item>
          <el-form-item label="最大间隔 (ms)">
            <el-input-number v-model="form.heartbeat_max_interval_ms" :min="100" :max="600000" :step="500" />
          </el-form-item>
          <el-form-item label="防抖 (ms)">
            <el-input-number v-model="form.heartbeat_debounce_ms" :min="0" :max="60000" :step="10" />
            <span style="margin-left:8px; color:#999; font-size:12px">
              连续 N 次异常才判定通道断连（默认 50ms）            </span>
          </el-form-item>
        </template>
      </el-form>
      <template #footer>
        <IconButton name="x" tip="取消" @click="dialogOpen = false" />
        <IconButton name="check" type="primary" tip="保存" :loading="submitting" @click="onSubmit" />
      </template>
    </el-dialog>
  </div>
</template>

