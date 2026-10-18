<script setup lang="ts">
/**
 * 系统图标库浏览页
 * - 网格展示所有 Icon
 * - 搜索、调整尺寸、点击复制图标名
 */
import { computed, ref } from 'vue'
import { iconNames, type IconName } from '@/components/iconPaths'
import { ElMessage } from 'element-plus'

// ===== 主题分组(对每个 IconName 归类) =====
const groups: { title: string; icon: IconName; names: IconName[] }[] = [
  { title: '基础操作',  icon: 'edit',         names: ['plus', 'minus', 'x', 'check', 'edit', 'trash', 'copy', 'cut', 'save', 'undo', 'redo', 'refresh', 'rotate-cw', 'rotate-ccw', 'search', 'zoom-in', 'zoom-out', 'maximize', 'minimize', 'minimize-2', 'more-horizontal', 'more-vertical', 'menu', 'grid', 'list', 'delete', 'disc', 'divide', 'plus-square', 'minus-square', 'check-square', 'move', 'cross', 'loader'] },
  { title: '方向 / 导航', icon: 'navigation', names: ['arrow-up', 'arrow-down', 'arrow-left', 'arrow-right', 'arrow-up-right', 'arrow-up-left', 'arrow-down-right', 'arrow-down-left', 'chevron-up', 'chevron-down', 'chevron-left', 'chevron-right', 'chevrons-up', 'chevrons-down', 'chevrons-left', 'chevrons-right', 'corner-up-left', 'corner-up-right', 'corner-down-left', 'corner-down-right', 'navigation', 'compass', 'map', 'map-pin'] },
  { title: '文件 / 文档', icon: 'file-text',   names: ['file', 'file-text', 'file-plus', 'file-minus', 'file-check', 'folder', 'folder-plus', 'folder-minus', 'archive', 'clipboard', 'download', 'upload', 'download-cloud', 'upload-cloud', 'paperclip', 'link', 'external-link'] },
  { title: '通讯 / 网络', icon: 'send',        names: ['send', 'mail', 'inbox', 'phone', 'phone-call', 'phone-off', 'voicemail', 'message-circle', 'message-square', 'bell', 'bell-off', 'wifi', 'wifi-off', 'rss', 'cast', 'radio', 'share', 'share-2', 'repeat', 'globe'] },
  { title: '设备 / 硬件', icon: 'server',      names: ['server', 'cpu', 'hard-drive', 'database', 'terminal', 'monitor', 'smartphone', 'tablet', 'tv', 'watch', 'camera', 'printer', 'mouse-pointer', 'usb', 'power', 'battery', 'battery-charging'] },
  { title: '工业 / SCADA', icon: 'gauge',      names: ['activity', 'gauge', 'zap', 'zap-off', 'thermometer', 'sun', 'sunrise', 'sunset', 'moon', 'cloud', 'cloud-drizzle', 'cloud-snow', 'cloud-lightning', 'wind', 'umbrella', 'toggle-left', 'toggle-right', 'sliders', 'settings', 'tool', 'feather'] },
  { title: '媒体 / 播放', icon: 'play',        names: ['play', 'pause', 'stop', 'skip-forward', 'skip-back', 'rewind', 'fast-forward', 'volume', 'volume-1', 'volume-2', 'volume-x', 'mic', 'mic-off', 'video', 'video-off', 'image', 'film', 'music', 'headphones', 'circle', 'pause-circle', 'play-circle', 'stop-circle'] },
  { title: '业务 / 状态', icon: 'check-circle',names: ['check-circle', 'x-circle', 'alert-circle', 'alert-triangle', 'info', 'help-circle', 'flag', 'bookmark', 'star', 'heart', 'thumbs-up', 'thumbs-down', 'clock', 'calendar', 'watch-icon', 'hourglass', 'trending-up', 'trending-down', 'bar-chart', 'bar-chart-2', 'pie-chart', 'trending-up-icon', 'target', 'crosshair', 'aperture'] },
  { title: '账号 / 安全', icon: 'shield-check',names: ['user', 'user-plus', 'user-minus', 'user-check', 'users', 'user-x', 'lock', 'unlock', 'key', 'shield', 'shield-off', 'shield-check', 'eye', 'eye-off', 'log-in', 'log-out', 'log-in-2', 'log-out-2'] },
  { title: '物体 / 符号', icon: 'package',     names: ['home', 'package', 'gift', 'shopping-cart', 'shopping-bag', 'credit-card', 'dollar-sign', 'tag', 'book', 'book-open', 'briefcase', 'coffee', 'truck', 'anchor', 'box', 'layout', 'sidebar', 'columns', 'square', 'circle-shape', 'triangle', 'hash', 'percent', 'code', 'git-branch', 'git-commit', 'git-merge', 'chrome', 'github', 'slack', 'trello'] },
  { title: '杂项 / 表情', icon: 'smile',       names: ['award', 'smile', 'frown', 'meh', 'life-buoy', 'pocket', 'command', 'shuffle', 'filter', 'layers'] }
]

const query = ref('')
const iconSize = ref(28)

const filteredGroups = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return groups
  return groups
    .map(g => ({ ...g, names: g.names.filter(n => n.toLowerCase().includes(q)) }))
    .filter(g => g.names.length > 0)
})

const totalShown = computed(() => filteredGroups.value.reduce((s, g) => s + g.names.length, 0))
const totalAll = iconNames.length

async function copy(name: IconName) {
  const text = `<Icon name="${name}" />`
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success(`已复制: ${text}`)
  } catch {
    ElMessage.warning('复制失败,请手动复制')
  }
}

const sample = ref<IconName>('cpu')
</script>

<template>
  <div class="icon-lib">
    <!-- 顶部条带:搜索 + 尺寸 -->
    <div class="lib-bar">
      <div class="bar-left">
        <div class="search-box">
          <Icon name="search" :size="14" />
          <input v-model="query" placeholder="搜索图标名,例如 trash / user / cpu ..." />
          <span v-if="query" class="clear" @click="query = ''">
            <Icon name="x" :size="12" />
          </span>
        </div>
        <div class="count">
          匹配 <b>{{ totalShown }}</b> / {{ totalAll }} 个
        </div>
      </div>
      <div class="bar-right">
        <div class="size-control">
          <span class="lbl">尺寸</span>
          <input type="range" min="14" max="64" step="2" v-model.number="iconSize" />
          <span class="num">{{ iconSize }}px</span>
        </div>
        <div class="sample-card">
          <span class="lbl">预览</span>
          <select v-model="sample">
            <option v-for="n in iconNames" :key="n" :value="n">{{ n }}</option>
          </select>
          <span class="sample-ico">
            <Icon :name="sample" :size="iconSize" />
          </span>
        </div>
      </div>
    </div>

    <!-- 分组 -->
    <div v-for="g in filteredGroups" :key="g.title" class="group">
      <div class="group-head">
        <Icon :name="g.icon" :size="14" />
        <span class="title">{{ g.title }}</span>
        <span class="badge">{{ g.names.length }}</span>
      </div>
      <div class="grid">
        <div
          v-for="n in g.names"
          :key="n"
          class="cell"
          :title="`点击复制: <Icon name=&quot;${n}&quot; />`"
          @click="copy(n)"
        >
          <div class="ico">
            <Icon :name="n" :size="iconSize" />
          </div>
          <div class="name">{{ n }}</div>
        </div>
      </div>
    </div>

    <div v-if="filteredGroups.length === 0" class="empty">
      <Icon name="search" :size="32" color="#cbd5e1" />
      <p>没有匹配的图标</p>
    </div>
  </div>
</template>

<style scoped>
.icon-lib {
  display: flex;
  flex-direction: column;
  gap: 18px;
}

/* ===== 顶部条带 ===== */
.lib-bar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 16px;
  padding: 14px 18px;
  background: #fff;
  border-radius: 10px;
  border: 1px solid #e5e7eb;
  box-shadow: 0 1px 2px rgba(0,0,0,0.02);
  flex-wrap: wrap;
}
.bar-left { display: flex; align-items: center; gap: 14px; flex: 1; min-width: 0; }
.bar-right { display: flex; align-items: center; gap: 16px; flex-wrap: wrap; }

.search-box {
  position: relative;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 7px 12px;
  background: #f8fafc;
  border: 1px solid #e2e8f0;
  border-radius: 8px;
  min-width: 320px;
  color: #94a3b8;
  transition: all .15s ease;
}
.search-box:focus-within {
  background: #fff;
  border-color: #3b82f6;
  box-shadow: 0 0 0 3px rgba(59,130,246,0.1);
  color: #475569;
}
.search-box input {
  flex: 1;
  border: none;
  background: transparent;
  outline: none;
  font-size: 13px;
  color: #1f2937;
}
.search-box input::placeholder { color: #94a3b8; }
.search-box .clear {
  cursor: pointer;
  color: #94a3b8;
  display: inline-flex;
}
.search-box .clear:hover { color: #475569; }

.count { font-size: 12px; color: #64748b; white-space: nowrap; }
.count b { color: #0ea5e9; }

.size-control {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 10px;
  border-radius: 8px;
  background: #f8fafc;
  border: 1px solid #e2e8f0;
}
.size-control .lbl { font-size: 12px; color: #64748b; }
.size-control input[type=range] { width: 110px; accent-color: #3b82f6; }
.size-control .num {
  font-size: 12px;
  font-weight: 600;
  color: #1e293b;
  min-width: 40px;
  text-align: right;
}

.sample-card {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 8px 4px 12px;
  border-radius: 8px;
  background: #eff6ff;
  border: 1px solid #bfdbfe;
}
.sample-card .lbl { font-size: 12px; color: #1d4ed8; }
.sample-card select {
  border: none;
  background: #fff;
  border-radius: 4px;
  font-size: 12px;
  padding: 3px 6px;
  color: #1e293b;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  outline: none;
  cursor: pointer;
}
.sample-ico {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 44px; height: 44px;
  background: #fff;
  border-radius: 6px;
  color: #2563eb;
}

/* ===== 分组 ===== */
.group {
  background: #fff;
  border-radius: 10px;
  border: 1px solid #e5e7eb;
  overflow: hidden;
}
.group-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 16px;
  background: #f8fafc;
  border-bottom: 1px solid #e5e7eb;
  color: #475569;
  font-size: 13px;
}
.group-head .title { font-weight: 600; color: #1e293b; }
.group-head .badge {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 22px; height: 18px;
  padding: 0 6px;
  background: #e0f2fe;
  color: #0369a1;
  border-radius: 9px;
  font-size: 11px;
  font-weight: 600;
  margin-left: 4px;
}

.grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(110px, 1fr));
  gap: 1px;
  background: #f1f5f9;
}
.cell {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  padding: 14px 6px 10px;
  background: #fff;
  cursor: pointer;
  transition: all .12s ease;
  position: relative;
}
.cell:hover {
  background: #eff6ff;
  z-index: 1;
  box-shadow: 0 2px 8px rgba(15, 23, 42, 0.06);
}
.cell:hover .name { color: #2563eb; }
.cell:hover .ico { transform: translateY(-2px); color: #2563eb; }

.ico {
  color: #334155;
  transition: all .12s ease;
  display: inline-flex;
}
.name {
  font-size: 11px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  color: #64748b;
  text-align: center;
  word-break: break-all;
  line-height: 1.3;
  transition: color .12s ease;
}

.empty {
  text-align: center;
  padding: 60px 20px;
  color: #94a3b8;
}
.empty p { margin-top: 12px; font-size: 14px; }
</style>
