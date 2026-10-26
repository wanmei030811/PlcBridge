<script setup lang="ts">
/**
 * 快捷键清单页
 *
 * 数据来源：shortcutRegistry.ts 的 GLOBAL_SHORTCUT_DEFS（单一数据源）
 * + 页面级动态注册的快捷键。
 *
 * 关键设计：这个页面**不硬编码**任何快捷键。
 * 它读的就是运行时实际注册的那份定义表 ——
 * 所以"文档写了的"和"实际能按的"永远是同一批，不会漂移。
 */
import { ref, computed, onMounted, onUnmounted } from 'vue'
import {
  buildGlobalShortcutMetas,
  getPageShortcuts,
  subscribePageShortcuts,
  SHORTCUT_GROUPS,
  type ShortcutMeta,
} from '@/composables/shortcutRegistry'

interface Row extends ShortcutMeta {
  /** 唯一键 */
  id: string
}

/** 全局快捷键（静态定义表）。 */
const globalRows = computed<Row[]>(() =>
  buildGlobalShortcutMetas().map((m, i) => ({ ...m, id: `global-${i}` })),
)

/** 页面级快捷键（动态注册，可能随页面变化）。 */
const pageRows = ref<Row[]>([])

function refreshPageRows() {
  pageRows.value = getPageShortcuts().map(s => ({
    id: s.id,
    label: s.label,
    desc: s.desc,
    group: s.group,
    scope: s.scope,
  }))
}

let unsubscribe: (() => void) | null = null
onMounted(() => {
  refreshPageRows()
  unsubscribe = subscribePageShortcuts(refreshPageRows)
})
onUnmounted(() => {
  unsubscribe?.()
  unsubscribe = null
})

/** 按分组归类（全局 + 页面级合并展示）。 */
const grouped = computed(() => {
  const all = [...globalRows.value, ...pageRows.value]
  const out: { group: string; items: Row[] }[] = []
  for (const g of SHORTCUT_GROUPS) {
    const items = all.filter(r => r.group === g)
    if (items.length) out.push({ group: g, items })
  }
  // 未登记在 SHORTCUT_GROUPS 里的分组（页面自定义）兜底放最后
  const known = new Set<string>(SHORTCUT_GROUPS)
  const rest = all.filter(r => !known.has(r.group))
  if (rest.length) out.push({ group: '页面自定义', items: rest })
  return out
})

const totalCount = computed(() => globalRows.value.length + pageRows.value.length)

/**
 * 把组合键渲染成独立的 <kbd> 片段。
 * "Ctrl+Shift+E" → ["Ctrl", "+", "Shift", "+", "E"]
 */
function splitKeys(label: string): string[] {
  return label.split('+').map(s => s.trim()).filter(Boolean)
}

/** 平台提示：Mac 用户用 Cmd 代替 Ctrl。 */
const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent)
const platformNote = computed(() =>
  isMac ? 'macOS 上请用 ⌘（Cmd）代替 Ctrl，Shift 保持不变。' : ''
)
</script>

<template>
  <div class="shortcuts-page">
    <div class="page-header">
      <div>
        <h2 class="page-title">快捷键</h2>
        <p class="page-desc">
          共 <b>{{ totalCount }}</b> 个快捷键。在输入框、下拉框内打字时不会触发快捷键。
        </p>
      </div>
    </div>

    <el-alert v-if="platformNote" :title="platformNote" type="info" :closable="false" class="platform-alert" />

    <div v-for="g in grouped" :key="g.group" class="group-block">
      <div class="group-head">
        <span class="group-title">{{ g.group }}</span>
        <span class="group-count">{{ g.items.length }} 项</span>
      </div>

      <div class="group-body">
        <div v-for="item in g.items" :key="item.id" class="shortcut-row">
          <div class="sc-meta">
            <div class="sc-desc">{{ item.desc }}</div>
            <div v-if="item.scope" class="sc-scope">{{ item.scope }}</div>
          </div>
          <div class="sc-keys">
            <template v-for="(k, i) in splitKeys(item.label)" :key="i">
              <span v-if="k === '+'" class="sc-plus">+</span>
              <kbd v-else class="sc-kbd">{{ k }}</kbd>
            </template>
          </div>
        </div>
      </div>
    </div>

    <div v-if="!grouped.length" class="empty-state">
      暂无可用快捷键
    </div>
  </div>
</template>

<style scoped>
.shortcuts-page { max-width: 900px; margin: 0 auto; }

.page-header { margin-bottom: 16px; }
.page-title { font-size: 20px; font-weight: 600; margin: 0 0 4px; color: #0f172a; }
.page-desc { font-size: 13px; color: #64748b; margin: 0; }
.page-desc b { color: #10b981; }

.platform-alert { margin-bottom: 16px; }

.group-block { margin-bottom: 16px; }
.group-head {
  display: flex;
  align-items: baseline;
  gap: 10px;
  margin-bottom: 8px;
}
.group-title {
  font-size: 13px;
  font-weight: 700;
  color: #0f172a;
  letter-spacing: 0.3px;
}
.group-count { font-size: 11px; color: #94a3b8; }

.group-body {
  background: #fff;
  border: 1px solid #eef0f5;
  border-radius: 12px;
  box-shadow: 0 1px 2px rgba(15, 23, 42, 0.04);
  overflow: hidden;
}

.shortcut-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 20px;
  padding: 12px 18px;
  border-bottom: 1px solid #f1f5f9;
}
.shortcut-row:last-child { border-bottom: none; }
.shortcut-row:hover { background: #fafbfc; }

.sc-meta { min-width: 0; flex: 1; }
.sc-desc { font-size: 13px; color: #1f2937; }
.sc-scope {
  margin-top: 2px;
  font-size: 11px;
  color: #b45309;
  background: #fffbeb;
  border: 1px solid #fcd34d;
  border-radius: 4px;
  padding: 0 5px;
  display: inline-block;
}

.sc-keys {
  display: flex;
  align-items: center;
  gap: 3px;
  flex-shrink: 0;
}
.sc-kbd {
  display: inline-block;
  min-width: 26px;
  padding: 3px 7px;
  border-radius: 6px;
  background: #f1f5f9;
  border: 1px solid #cbd5e1;
  border-bottom-width: 2px;
  font-family: ui-monospace, "Cascadia Code", "Consolas", monospace;
  font-size: 11.5px;
  font-weight: 600;
  color: #334155;
  text-align: center;
  line-height: 1.3;
}
.sc-plus {
  font-size: 11px;
  color: #94a3b8;
  padding: 0 1px;
}

.empty-state {
  padding: 48px;
  text-align: center;
  color: #94a3b8;
  font-size: 13px;
  background: #fff;
  border: 1px solid #eef0f5;
  border-radius: 12px;
}

:global(.dark) .page-title,
:global(.dark) .group-title { color: #f1f5f9; }
:global(.dark) .sc-desc { color: #cbd5e1; }
:global(.dark) .group-body { background: #1e293b; border-color: #334155; }
:global(.dark) .shortcut-row { border-bottom-color: #334155; }
:global(.dark) .shortcut-row:hover { background: #1a2436; }
:global(.dark) .sc-kbd {
  background: #0f172a;
  border-color: #475569;
  color: #cbd5e1;
}
:global(.dark) .sc-plus { color: #64748b; }
:global(.dark) .sc-scope {
  background: rgba(245, 158, 11, 0.12);
  border-color: #f59e0b;
  color: #fbbf24;
}
</style>
