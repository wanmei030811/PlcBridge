<script setup lang="ts">
/**
 * 通用设置页（Web 控制台运行时设置）
 *
 * 当前只有一个设置项：自动导出配置。
 *
 * 为什么默认关闭：
 *   现场改配置常常是"先改后看"——改了还没确认对不对，就先看采集结果。
 *   如果默认自动导出，改一下就立刻推给运行中的 plcbridge，
 *   可能把正在正常跑的采集改坏。默认关 = 行为不变，由用户显式选择。
 *
 * 为什么开关存在而不直接做成自动：
 *   有些部署（如生产）希望所有配置改动都经过评审/手工导出；
 *   另一些（如开发调试）希望改完即生效。开关让同一套代码适配两种场景。
 */
import { ref, onMounted, computed } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { fetchSettings, updateSetting, type WebSettings } from '@/api/settings'
import { useConfigExport } from '@/composables/useConfigExport'
import IconButton from '@/components/IconButton.vue'

const loading = ref(false)
const saving = ref(false)
const settings = ref<WebSettings>({ auto_export_config: false })

// 顶栏「导出配置」按钮共用同一份状态 → 开关状态也能在顶栏体现。
const { dirty, pendingChanges, refreshSettings } = useConfigExport()

const autoExport = computed({
  get: () => settings.value.auto_export_config,
  set: async (v: boolean) => {
    // 开启前确认一次：自动导出意味着配置改动会立刻推给运行中的 plcbridge。
    if (v) {
      try {
        await ElMessageBox.confirm(
          '开启后，每次修改通道/设备/点位/目标，都会自动把配置写入 config.json，' +
          'plcbridge 会在约 1.5 秒内自动热重载。\n\n' +
          '这意味着正在运行的采集会立刻跟着新配置走。请确认现场允许热重载。',
          '确认开启自动导出？',
          { confirmButtonText: '开启', cancelButtonText: '取消', type: 'warning' }
        )
      } catch {
        // 用户取消 → 恢复开关的视觉状态
        settings.value.auto_export_config = false
        return
      }
    }

    saving.value = true
    try {
      const r = await updateSetting('auto_export_config', v)
      settings.value = r
      // 同步顶栏的状态（顶栏那个绿点表示"自动导出已开启"）。
      await refreshSettings()
      ElMessage.success(v ? '已开启自动导出配置' : '已关闭自动导出配置')
    } catch (e: any) {
      // 失败 → 回滚 UI 状态（不能界面显示"开"但后端是关的）
      settings.value.auto_export_config = !v
      ElMessage.error(e?.response?.data?.msg || e?.response?.data?.error || e?.message || '保存失败')
    } finally {
      saving.value = false
    }
  },
})

async function refresh() {
  loading.value = true
  try {
    settings.value = await fetchSettings()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.msg || e?.message || '加载设置失败')
  } finally {
    loading.value = false
  }
}
onMounted(refresh)
</script>

<template>
  <div class="settings-page">
    <div class="page-header">
      <div>
        <h2 class="page-title">通用设置</h2>
        <p class="page-desc">
          这些设置只影响 <b>Web 控制台自身的行为</b>，不参与 plcbridge 的采集配置。
        </p>
      </div>
      <IconButton name="refresh" tip="刷新" :loading="loading" @click="refresh" />
    </div>

    <el-card class="setting-card">
      <div class="setting-row">
        <div class="setting-info">
          <div class="setting-title">自动导出配置</div>
          <div class="setting-desc">
            修改通道 / 设备 / 点位 / 目标后，自动把配置写入
            <code>config.json</code>，plcbridge 约 1.5 秒后自动热重载，无需手动点「导出配置」。
          </div>
          <div class="setting-note">
            <span class="note-tag">默认关闭</span>
            开启前请确认现场允许热重载 —— 正在运行的采集会立刻跟着新配置走。
          </div>
        </div>
        <el-switch
          v-model="autoExport"
          :loading="saving"
          :disabled="loading"
          active-text="开启"
          inactive-text="关闭"
        />
      </div>
    </el-card>

    <!-- 当前导出状态：让用户知道自动导出到底有没有把改动推出去 -->
    <el-card class="setting-card">
      <div class="setting-row">
        <div class="setting-info">
          <div class="setting-title">当前配置同步状态</div>
          <div class="setting-desc">
            自动导出开启时，这里显示的"未导出条目"会在约 1.5 秒内自动归零。
          </div>
        </div>
        <div class="status-box">
          <span v-if="dirty" class="status-pill status-dirty">
            {{ pendingChanges || '?' }} 项待导出
          </span>
          <span v-else class="status-pill status-clean">
            已同步
          </span>
        </div>
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.settings-page { max-width: 900px; margin: 0 auto; }

.page-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  margin-bottom: 16px;
}
.page-title { font-size: 20px; font-weight: 600; margin: 0 0 4px; color: #0f172a; }
.page-desc { font-size: 13px; color: #64748b; margin: 0; line-height: 1.6; }

.setting-card { margin-bottom: 14px; }
.setting-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 24px;
}
.setting-info { min-width: 0; flex: 1; }
.setting-title {
  font-size: 14px;
  font-weight: 600;
  color: #0f172a;
  margin-bottom: 4px;
}
.setting-desc {
  font-size: 12.5px;
  color: #64748b;
  line-height: 1.6;
}
.setting-desc code {
  background: #f1f5f9;
  padding: 1px 5px;
  border-radius: 4px;
  font-size: 12px;
  color: #475569;
}
.setting-note {
  margin-top: 8px;
  font-size: 12px;
  color: #92400e;
  display: flex;
  align-items: center;
  gap: 6px;
}
.note-tag {
  padding: 1px 6px;
  border-radius: 4px;
  background: #fef3c7;
  color: #b45309;
  font-weight: 600;
  font-size: 11px;
  flex-shrink: 0;
}

.status-box { flex-shrink: 0; }
.status-pill {
  display: inline-block;
  padding: 5px 12px;
  border-radius: 999px;
  font-size: 12px;
  font-weight: 600;
}
.status-dirty {
  background: #fffbeb;
  color: #b45309;
  border: 1px solid #fcd34d;
}
.status-clean {
  background: #ecfdf5;
  color: #065f46;
  border: 1px solid #a7f3d0;
}

:global(.dark) .page-title,
:global(.dark) .setting-title { color: #f1f5f9; }
:global(.dark) .setting-desc code {
  background: #0f172a;
  color: #94a3b8;
}
:global(.dark) .status-dirty {
  background: rgba(245, 158, 11, 0.12);
  color: #fbbf24;
  border-color: #f59e0b;
}
:global(.dark) .status-clean {
  background: rgba(16, 185, 129, 0.12);
  color: #34d399;
  border-color: #10b981;
}
</style>
