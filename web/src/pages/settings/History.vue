<script setup lang="ts">
/**
 * 配置变更历史与回滚（Plan3 3.3）。
 * - 左侧：快照列表
 * - 右侧：与当前配置的 diff 视图 + 回滚按钮
 */
import { ref, computed, onMounted, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  listSnapshots, getSnapshot, createSnapshot, deleteSnapshot,
  diffSnapshot, rollbackSnapshot,
  type SnapshotItem, type SnapshotDiffEntry,
} from '@/api/snapshots'
import Icon from '@/components/Icon.vue'

const loading = ref(false)
const diffLoading = ref(false)
const rolling = ref(false)
const creating = ref(false)

const snapshots = ref<SnapshotItem[]>([])
const selectedId = ref<number | null>(null)
const diffs = ref<SnapshotDiffEntry[]>([])
const selectedDetail = ref<any>(null)

const entityLabels: Record<string, string> = {
  channels: '通道',
  devices: '设备',
  sources: '数据源',
  points: '点位',
  targets: '转发目标',
}

const changeLabels: Record<string, string> = {
  added: '新增',
  removed: '删除',
  modified: '修改',
}

const changeTagType: Record<string, 'success' | 'danger' | 'warning'> = {
  added: 'success',
  removed: 'danger',
  modified: 'warning',
}

const selected = computed(() => snapshots.value.find(s => s.id === selectedId.value) || null)

// 按 entity 分组 diff
const groupedDiffs = computed(() => {
  const map = new Map<string, SnapshotDiffEntry[]>()
  for (const d of diffs.value) {
    if (!map.has(d.entity)) map.set(d.entity, [])
    map.get(d.entity)!.push(d)
  }
  return Array.from(map.entries())
})

function fmtSize(bytes: number) {
  if (!bytes || bytes < 0) return '—'
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
  return (bytes / 1024 / 1024).toFixed(2) + ' MB'
}

function fmtTime(ms: number) {
  if (!ms) return '—'
  try { return new Date(ms).toLocaleString('zh-CN') } catch { return String(ms) }
}

async function refresh(selectFirst = true) {
  loading.value = true
  try {
    const r = await listSnapshots()
    snapshots.value = r.items || []
    if (selectFirst && snapshots.value.length > 0) {
      await select(snapshots.value[0].id)
    } else if (selectedId.value && !snapshots.value.find(s => s.id === selectedId.value)) {
      selectedId.value = snapshots.value.length > 0 ? snapshots.value[0].id : null
    }
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.msg || e?.message || '加载快照列表失败')
  } finally {
    loading.value = false
  }
}

async function select(id: number) {
  selectedId.value = id
  diffLoading.value = true
  diffs.value = []
  try {
    const [detail, diff] = await Promise.all([getSnapshot(id), diffSnapshot(id)])
    selectedDetail.value = detail
    diffs.value = diff.diffs || []
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.msg || e?.message || '加载 diff 失败')
  } finally {
    diffLoading.value = false
  }
}

async function onCreate() {
  creating.value = true
  try {
    const r = await createSnapshot('手动创建快照')
    ElMessage.success(`已创建快照 #${r.id}`)
    await refresh(false)
    if (r.id) await select(r.id)
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.msg || e?.message || '创建快照失败')
  } finally {
    creating.value = false
  }
}

async function onDelete(row: SnapshotItem) {
  try {
    await ElMessageBox.confirm(
      `确定删除快照 #${row.id}（${row.description}）吗？此操作不可恢复。`,
      '删除快照',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' }
    )
  } catch { return }

  try {
    await deleteSnapshot(row.id)
    ElMessage.success('已删除')
    if (selectedId.value === row.id) selectedId.value = null
    await refresh(false)
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.msg || e?.message || '删除失败')
  }
}

async function onRollback(row: SnapshotItem) {
  try {
    await ElMessageBox.confirm(
      `确定回滚到快照 #${row.id}（${row.description}）吗？\n` +
      `当前配置将被完全替换，且会自动创建一份"回滚前备份"快照。`,
      '回滚配置',
      { type: 'warning', confirmButtonText: '确认回滚', cancelButtonText: '取消' }
    )
  } catch { return }

  rolling.value = true
  try {
    const r = await rollbackSnapshot(row.id)
    // reload=false 表示数据库已回滚、但 config.json 没导出，
    // plcbridge 主进程仍在跑旧配置 —— 必须用 warning 而不是 success 提示。
    if (r.reload === false) {
      ElMessage.warning(r.message || '回滚完成，但 plcbridge 未热加载，请手动重载配置')
    } else {
      ElMessage.success(r.message || '回滚成功')
    }
    await refresh(false)
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.msg || e?.message || '回滚失败')
  } finally {
    rolling.value = false
  }
}

onMounted(() => refresh())
watch(selectedId, (id) => { if (id && !selectedDetail.value) select(id) })
</script>

<template>
  <div class="history-page">
    <div class="page-head">
      <div>
        <h2 class="page-title">配置变更历史</h2>
        <p class="page-desc">
          每次配置写入（通道 / 设备 / 数据源 / 点位 / 目标）都会自动创建快照。
          最多保留 50 个，可随时回滚到任意历史版本。
        </p>
      </div>
      <el-button type="primary" :loading="creating" @click="onCreate">
        <Icon name="plus" :size="13" style="margin-right:4px;vertical-align:-2px;" />
        手动创建快照
      </el-button>
    </div>

    <div class="split">
      <!-- 左侧：快照列表 -->
      <div class="snap-list card">
        <div class="list-head">
          <span>快照列表</span>
          <span class="count">{{ snapshots.length }}</span>
        </div>
        <div v-loading="loading" class="list-body">
          <div v-if="!loading && snapshots.length === 0" class="empty">
            暂无快照记录
          </div>
          <div
            v-for="s in snapshots"
            :key="s.id"
            class="snap-item"
            :class="{ active: s.id === selectedId }"
            @click="select(s.id)"
          >
            <div class="snap-item-top">
              <span class="snap-id">#{{ s.id }}</span>
              <el-tag v-if="s.is_auto" size="small" type="info" effect="plain">自动</el-tag>
              <span class="snap-records">{{ s.record_count }} 条</span>
            </div>
            <div class="snap-desc">{{ s.description }}</div>
            <div class="snap-meta">
              <span>{{ fmtTime(s.created_at) }}</span>
              <span>· {{ s.created_by }}</span>
              <span>· {{ fmtSize(s.size_bytes) }}</span>
            </div>
            <div class="snap-actions" @click.stop>
              <el-button size="small" text type="primary" :loading="rolling" @click="onRollback(s)">
                回滚
              </el-button>
              <el-button size="small" text type="danger" @click="onDelete(s)">
                删除
              </el-button>
            </div>
          </div>
        </div>
      </div>

      <!-- 右侧：diff 视图 -->
      <div class="diff-panel card">
        <div class="list-head">
          <span>配置差异（快照 vs 当前）</span>
          <el-tag v-if="diffs.length" size="small" type="warning" effect="plain">
            {{ diffs.length }} 处变更
          </el-tag>
        </div>
        <div v-loading="diffLoading" class="diff-body">
          <div v-if="!selected" class="empty">请选择左侧的一个快照</div>
          <div v-else-if="!diffLoading && diffs.length === 0" class="empty">
            该快照与当前配置完全一致
          </div>
          <div v-else class="diff-groups">
            <div v-for="[entity, list] in groupedDiffs" :key="entity" class="diff-group">
              <div class="group-title">
                {{ entityLabels[entity] || entity }}
                <span class="group-count">{{ list.length }}</span>
              </div>
              <table class="diff-table">
                <thead>
                  <tr>
                    <th style="width:90px;">变更</th>
                    <th style="width:150px;">对象</th>
                    <th style="width:130px;">字段</th>
                    <th>旧值</th>
                    <th>新值</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(d, i) in list" :key="i">
                    <td>
                      <el-tag size="small" :type="changeTagType[d.change] || 'info'" effect="light">
                        {{ changeLabels[d.change] || d.change }}
                      </el-tag>
                    </td>
                    <td class="mono">{{ d.key }}</td>
                    <td class="mono">{{ d.field || '—' }}</td>
                    <td class="mono val-old">{{ d.old_value || '—' }}</td>
                    <td class="mono val-new">{{ d.new_value || '—' }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.history-page { display: flex; flex-direction: column; gap: 16px; }
.page-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.page-title { margin: 0 0 4px; font-size: 18px; font-weight: 600; color: #111827; }
.page-desc { margin: 0; font-size: 13px; color: #6b7280; line-height: 1.6; }
:global(.dark) .page-title { color: #f1f5f9; }
:global(.dark) .page-desc { color: #94a3b8; }

.split { display: grid; grid-template-columns: 360px 1fr; gap: 16px; align-items: start; }
@media (max-width: 1100px) { .split { grid-template-columns: 1fr; } }

.card {
  background: #fff;
  border: 1px solid #f0f0f0;
  border-radius: 10px;
  overflow: hidden;
}
:global(.dark) .card { background: #1e293b; border-color: #334155; }

.list-head {
  display: flex; align-items: center; justify-content: space-between;
  padding: 12px 16px;
  font-size: 13px; font-weight: 600;
  border-bottom: 1px solid #f0f0f0;
  color: #374151;
}
:global(.dark) .list-head { border-color: #334155; color: #cbd5e1; }
.count {
  font-size: 11px; color: #6b7280;
  background: #f3f4f6; border-radius: 10px; padding: 1px 8px;
}
:global(.dark) .count { background: #334155; color: #94a3b8; }

.list-body { max-height: 620px; overflow-y: auto; }
.diff-body { max-height: 620px; overflow-y: auto; padding: 12px 16px; }

.empty { padding: 40px 16px; text-align: center; color: #9ca3af; font-size: 13px; }
:global(.dark) .empty { color: #64748b; }

.snap-item {
  padding: 10px 16px;
  border-bottom: 1px solid #fafafa;
  cursor: pointer;
  transition: background 120ms ease;
  border-left: 3px solid transparent;
}
:global(.dark) .snap-item { border-bottom-color: #0f172a; }
.snap-item:hover { background: #f9fafb; }
:global(.dark) .snap-item:hover { background: #1a2436; }
.snap-item.active {
  background: #ecfdf5;
  border-left-color: #10b981;
}
:global(.dark) .snap-item.active { background: rgba(16,185,129,0.08); }

.snap-item-top { display: flex; align-items: center; gap: 8px; }
.snap-id { font-size: 12px; font-weight: 700; color: #10b981; font-family: ui-monospace, monospace; }
.snap-records { font-size: 11px; color: #9ca3af; margin-left: auto; }
.snap-desc {
  font-size: 13px; color: #1f2937; margin: 4px 0;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
:global(.dark) .snap-desc { color: #e2e8f0; }
.snap-meta { display: flex; gap: 5px; font-size: 11px; color: #9ca3af; }
.snap-actions { margin-top: 6px; display: flex; gap: 4px; }

.diff-groups { display: flex; flex-direction: column; gap: 16px; }
.group-title {
  font-size: 13px; font-weight: 600; color: #374151; margin-bottom: 8px;
  display: flex; align-items: center; gap: 8px;
}
:global(.dark) .group-title { color: #cbd5e1; }
.group-count {
  font-size: 11px; color: #6b7280;
  background: #f3f4f6; border-radius: 10px; padding: 1px 8px;
}
:global(.dark) .group-count { background: #334155; color: #94a3b8; }

.diff-table { width: 100%; border-collapse: collapse; font-size: 12px; }
.diff-table th {
  text-align: left; font-weight: 500; color: #6b7280;
  border-bottom: 1px solid #e5e7eb; padding: 6px 8px; font-size: 11px;
}
:global(.dark) .diff-table th { color: #94a3b8; border-color: #334155; }
.diff-table td {
  border-bottom: 1px solid #f5f5f5; padding: 6px 8px;
  vertical-align: top; word-break: break-all;
}
:global(.dark) .diff-table td { border-color: #0f172a; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 11px; }
.val-old { color: #dc2626; }
.val-new { color: #059669; }
:global(.dark) .val-old { color: #f87171; }
:global(.dark) .val-new { color: #34d399; }
</style>
