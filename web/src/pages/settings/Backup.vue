<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  fetchBackupInfo, listBackups, exportBackup, restoreBackup, downloadBackup,
  type BackupListResp, type ExportResp, type RestoreResp
} from '@/api/backup'
import IconButton from '@/components/IconButton.vue'

const loading = ref(false)
const exporting = ref(false)
const restoring = ref(false)

const info = ref<{ latest: any; items: any[] }>({
  latest: null,
  items: [],
})

function fmtSize(bytes: number) {
  if (!bytes || bytes < 0) return '—'
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
  return (bytes / 1024 / 1024).toFixed(2) + ' MB'
}

function fmtDate(iso: string) {
  if (!iso) return '—'
  try { return new Date(iso).toLocaleString('zh-CN') } catch { return iso }
}

async function refresh() {
  loading.value = true
  try {
    const [infoResp, listResp] = await Promise.all([
      fetchBackupInfo(),
      listBackups(),
    ])
    info.value = { latest: infoResp.latest, items: listResp.items || [] }
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '加载失败')
  } finally {
    loading.value = false
  }
}
onMounted(refresh)

async function onExport() {
  exporting.value = true
  try {
    const r = await exportBackup() as ExportResp
    ElMessage.success(`已导出：${r.filename}（${fmtSize(r.size)}，${r.records} 条记录）`)
    await refresh()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '导出失败')
  } finally {
    exporting.value = false
  }
}

function onUpload() {
  const input = document.createElement('input')
  input.type = 'file'
  input.accept = '.zip'
  input.onchange = async () => {
    const file = input.files?.[0]
    if (!file) return
    if (!file.name.endsWith('.zip')) {
      ElMessage.warning('请选择 .zip 备份文件')
      return
    }
    restoring.value = true
    try {
      const r = await restoreBackup(file) as RestoreResp
      if (r.ok) {
        ElMessageBox.alert(
          r.message + '\n\n点击「确定」刷新页面以加载新配置。',
          '配置已恢复', { confirmButtonText: '确定' }
        ).then(() => { location.reload() })
      } else {
        ElMessage.error(r.message || '恢复失败')
      }
    } catch (e: any) {
      ElMessage.error(e?.response?.data?.error || e?.message || '恢复失败')
    } finally {
      restoring.value = false
    }
  }
  input.click()
}

async function onDownload(filename: string) {
  try {
    downloadBackup(filename)
  } catch (e: any) {
    ElMessage.error(e?.message || '下载失败')
  }
}
</script>

<template>
  <div>
    <div style="display:flex; justify-content:space-between; margin-bottom:16px">
      <h2 style="margin:0">配置备份 / 恢复</h2>
      <div>
        <IconButton name="refresh" tip="刷新" :loading="loading" @click="refresh" />
      </div>
    </div>

    <!-- 备份工具栏 -->
    <div style="display:flex; gap:10px; margin-bottom:18px; flex-wrap:wrap">
      <el-button type="primary" :loading="exporting" @click="onExport">
        💾 导出备份
      </el-button>
      <el-button :loading="restoring" @click="onUpload">
        📂 从备份恢复
      </el-button>
    </div>

    <!-- 最新备份 + 历史备份 平行布局 -->
    <div style="display:flex; gap:16px; align-items:flex-start; flex-wrap:wrap">
      <!-- 最新备份 -->
      <el-card v-if="info.latest" style="width:400px; flex:0 0 400px">
        <template #header>
          <div style="font-weight:600">最新备份</div>
        </template>
        <div style="display:grid; grid-template-columns: auto 1fr; gap:8px 12px; font-size:13px">
          <span style="color:#64748b">文件名</span>
          <span style="font-family:monospace; word-break:break-all">{{ info.latest.filename }}</span>
          <span style="color:#64748b">创建时间</span>
          <span>{{ fmtDate(info.latest.created_at) }}</span>
          <span style="color:#64748b">数据库大小</span>
          <span>{{ fmtSize(info.latest.db_size_bytes) }}</span>
          <span style="color:#64748b">记录数</span>
          <span>{{ info.latest.db_records ?? '—' }}</span>
        </div>
        <template #footer>
          <el-button size="small" @click="onDownload(info.latest.filename)">下载</el-button>
        </template>
      </el-card>
      <el-alert v-else title="暂无备份" type="info" :closable="false" style="width:400px; flex:0 0 400px">
        点击「导出备份」创建第一个备份
      </el-alert>

      <!-- 备份列表 -->
      <el-card v-if="info.items.length > 1" style="flex:1 1 280px; min-width:0">
        <template #header>
          <div style="font-weight:600">历史备份（共 {{ info.items.length }} 份）</div>
        </template>
        <el-table :data="info.items" stripe size="small">
          <el-table-column prop="filename" label="文件名" min-width="200">
            <template #default="{ row }">
              <code style="font-size:12px">{{ row.filename }}</code>
            </template>
          </el-table-column>
          <el-table-column prop="created_at" label="创建时间" width="170">
            <template #default="{ row }">
              {{ fmtDate(row.created_at) }}
            </template>
          </el-table-column>
          <el-table-column prop="size" label="大小" width="100">
            <template #default="{ row }">
              {{ fmtSize(row.size) }}
            </template>
          </el-table-column>
          <el-table-column prop="db_records" label="记录数" width="80" />
          <el-table-column label="操作" width="80" align="center">
            <template #default="{ row }">
              <el-button size="small" link type="primary" @click="onDownload(row.filename)">下载</el-button>
            </template>
          </el-table-column>
        </el-table>
        <div style="margin-top:10px; font-size:12px; color:#909399">
          备份保留最近 10 份，超出自动清理。恢复前会自动备份当前状态（auto-before-restore-*.zip）。
        </div>
      </el-card>
    </div>
  </div>
</template>
