<script setup lang="ts">
import { computed, ref, onMounted, watch, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useDark, useToggle } from '@vueuse/core'
import { useAuthStore } from '@/stores/auth'
import { getDevice, type Device } from '@/api/devices'
import { useRouteShortcuts } from '@/composables/useGlobalShortcuts'
import { useConfigExport } from '@/composables/useConfigExport'
import Icon from '@/components/Icon.vue'
import NavItem from '@/components/NavItem.vue'
import {
  navLeaves, navGroups, isLeafActive, findGroupOf,
  type NavGroup,
} from '@/config/nav'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()

// === Dark Mode（Plan3 3.6）===
const isDark = useDark({
  storageKey: 'plcbridge-theme',
  valueDark: 'dark',
  valueLight: 'light',
})
const toggleTheme = useToggle(isDark)

// 侧边栏折叠状态 (持久化)
const collapsed = ref(false)
onMounted(() => {
  try {
    const saved = localStorage.getItem('plcbridge:sidebar:collapsed')
    if (saved === '1') collapsed.value = true
  } catch { /* localStorage 不可用（隐私模式等）静默忽略 */ }
})
watch(collapsed, (v) => {
  try { localStorage.setItem('plcbridge:sidebar:collapsed', v ? '1' : '0') } catch { /* ignore */ }
})
function toggleCollapsed() {
  collapsed.value = !collapsed.value
}

// === 二级菜单展开状态 ===
//
// 每个分组独立记录是否展开，持久化到 localStorage：
//   - 用户手动展开/收起 → 记住
//   - 当前页所属分组 → 总是自动展开（否则进去看不到自己在哪）
const expandedKeys = ref<string[]>([])
const STORAGE_KEY = 'plcbridge:sidebar:expanded'
onMounted(() => {
  try {
    const saved = localStorage.getItem(STORAGE_KEY)
    if (saved) expandedKeys.value = JSON.parse(saved)
  } catch { /* localStorage 不可用（隐私模式等）静默忽略 */ }
})
watch(expandedKeys, (v) => {
  try { localStorage.setItem(STORAGE_KEY, JSON.stringify(v)) } catch { /* ignore */ }
})

/** 某个分组下是否有叶子项命中当前路由 */
function groupHasActive(group: NavGroup): boolean {
  return group.children.some(c => isLeafActive(route.path, c.path))
}

function toggleGroup(group: NavGroup) {
  const i = expandedKeys.value.indexOf(group.key)
  if (i >= 0) {
    expandedKeys.value = expandedKeys.value.filter(k => k !== group.key)
  } else {
    expandedKeys.value = [...expandedKeys.value, group.key]
  }
}

/** 分组是否展开（折叠侧边栏时二级不显示，直接由 isGroupOpen 之外的 v-show 控制） */
function isGroupOpen(group: NavGroup): boolean {
  return expandedKeys.value.includes(group.key)
}

/**
 * 折叠态下点分组标题 → 直接进该分组第一个页面。
 * 折叠态没有空间展开二级，不给用户一个"点了没反应"的死按钮。
 */
function onGroupClick(group: NavGroup) {
  if (collapsed.value) {
    const first = group.children[0]
    if (first) {
      if (first.href) window.open(first.href, '_blank', 'noopener')
      else router.push(first.path)
    }
    return
  }
  toggleGroup(group)
}

// 路由切到某个叶子时，自动展开它所属的分组。
watch(() => route.path, (p) => {
  const g = findGroupOf(p)
  if (g && !expandedKeys.value.includes(g.key)) {
    expandedKeys.value = [...expandedKeys.value, g.key]
  }
})

// 面包屑：只在进入 /devices/:id 时按需拉单个设备，避免每次路由变化都全量拉列表
const deviceCache = ref<Record<number, Device>>({})
let unmounted = false
let breadcrumbSeq = 0

async function loadDeviceForBreadcrumb(id: number) {
  if (deviceCache.value[id]) return // 已缓存
  const seq = ++breadcrumbSeq
  try {
    const d = await getDevice(id)
    if (unmounted || seq !== breadcrumbSeq) return
    deviceCache.value = { ...deviceCache.value, [id]: d as unknown as Device }
  } catch { /* ignore */ }
}

onUnmounted(() => { unmounted = true })

onMounted(() => {
  // 初始进入时如果是设备详情，立即按需加载
  const m = /^\/devices\/(\d+)$/.exec(route.path)
  if (m) loadDeviceForBreadcrumb(Number(m[1]))
})

// 只在路径匹配 /devices/:id 时按需加载，不全量拉列表
watch(() => route.fullPath, (newPath) => {
  const m = /^\/devices\/(\d+)$/.exec(newPath)
  if (m) loadDeviceForBreadcrumb(Number(m[1]))
})

const breadcrumbs = computed(() => {
  const map: Record<string, string> = {
    '/dashboard': '仪表盘',
    '/devices': '设备',
    '/channels': '通道',
    '/tokens': 'API Token',
    '/settings/account': '账号 / 密码',
    '/settings/general': '通用设置',
    '/settings/backup': '配置备份',
    '/settings/history': '变更历史',
    '/system/icons': '图标库',
    '/system/shortcuts': '快捷键',
    '/swagger': 'API 文档',
  }
  const segs = route.path.split('/').filter(Boolean)
  const out: { label: string; to?: string }[] = []
  let acc = ''
  for (let i = 0; i < segs.length; i++) {
    const s = segs[i]
    acc += '/' + s
    let label = map[acc] || s
    // /devices/:id 末段用设备名称
    if (acc === '/devices' && i + 1 < segs.length) {
      // 设备列表项 — 此段是列表路径
    } else if (/^\/devices\/\d+$/.test(acc)) {
      const id = Number(segs[i])
      const dev = deviceCache.value[id]
      if (dev) label = dev.name
    }
    out.push({ label, to: acc })
  }

  // 二级菜单分组名插到页面前面：系统 / 通用设置
  // （分组本身没有独立路由，所以 to 指向该分组第一个叶子页）
  const group = findGroupOf(route.path)
  if (group) {
    const first = group.children.find(c => isLeafActive(route.path, c.path))
    out.unshift({ label: group.label, to: first?.path })
  }
  return out
})

async function onLogout() {
  await auth.logout()
  router.replace('/login')
}

// === 全局「导出配置」按钮（顶栏常驻）===
//
// 痛点：改完配置（设备/通道/点位/目标）必须切回仪表盘点才能点「导出配置」，
// 否则 plcbridge 一直跑旧配置。把按钮常驻顶栏，任何页面一键导出。
const {
  state: configExportState,
  dirty,
  pendingChanges,
  autoExport,
  exportConfig: doExportConfig,
} = useConfigExport()

const exporting = computed(() => configExportState.exporting)

// === 全局快捷键（Plan3 3.8）===
//
// 挂在布局层而不是 App.vue：App.vue 是所有路由（含 /login）的根，
// 登录页按 Ctrl+K / Ctrl+D 跳仪表盘毫无意义，且未登录时会被路由守卫弹回。
// 布局层只在已登录的 app shell 内渲染，正好对应"应用内快捷键"的语义。
const unregisterShortcuts = useRouteShortcuts(router)
onUnmounted(() => { unregisterShortcuts() })
</script>

<template>
  <div class="app-shell" :class="{ 'is-collapsed': collapsed }">
    <!-- 侧边栏 -->
    <aside class="app-aside">
      <div class="app-brand">
        <el-tooltip :content="collapsed ? '点击展开' : ''" :disabled="!collapsed" placement="right">
          <div class="brand-logo" @click="collapsed ? toggleCollapsed() : null" :title="collapsed ? '点击展开' : ''">P</div>
        </el-tooltip>
        <div class="brand-text">
          <div class="brand-title">PlcBridge</div>
          <div class="brand-sub">工业采集网关</div>
        </div>
        <button class="collapse-btn" :title="collapsed ? '展开侧边栏' : '折叠侧边栏'" @click="toggleCollapsed">
          <Icon :name="collapsed ? 'chevron-right' : 'chevron-left'" :size="14" />
        </button>
      </div>

      <nav class="app-nav">
        <!-- 一级：直达项 -->
        <el-tooltip
          v-for="item in navLeaves"
          :key="item.path"
          :content="item.label"
          placement="right"
          :disabled="!collapsed"
        >
          <router-link
            :to="item.path"
            class="nav-item"
            :class="{ active: isLeafActive(route.path, item.path) }"
          >
            <span class="nav-icon">
              <Icon :name="item.icon as any" :size="18" />
            </span>
            <span class="nav-label">{{ item.label }}</span>
          </router-link>
        </el-tooltip>

        <!-- 一级：可展开分组 -->
        <div v-for="g in navGroups" :key="g.key" class="nav-group">
          <el-tooltip
            :content="g.label"
            placement="right"
            :disabled="!collapsed"
          >
            <button
              class="nav-item nav-group-head"
              :class="{ active: groupHasActive(g), open: isGroupOpen(g) }"
              :aria-expanded="isGroupOpen(g)"
              @click="onGroupClick(g)"
            >
              <span class="nav-icon">
                <Icon :name="g.icon as any" :size="18" />
              </span>
              <span class="nav-label">{{ g.label }}</span>
              <Icon
                class="nav-group-caret"
                :name="isGroupOpen(g) ? 'chevron-down' : 'chevron-right'"
                :size="14"
              />
            </button>
          </el-tooltip>

          <!-- 折叠态：hover 弹出二级，保证窄侧边栏下也能进到每个子页面 -->
          <div v-show="collapsed" class="nav-flyout">
            <div class="nav-flyout-title">{{ g.label }}</div>
            <a
              v-for="c in g.children"
              :key="c.path"
              :href="c.href"
              :target="c.href ? '_blank' : undefined"
              :rel="c.href ? 'noopener noreferrer' : undefined"
              class="nav-flyout-item"
              :class="{ active: isLeafActive(route.path, c.path) }"
              @click="!c.href && $router.push(c.path)"
            >
              <Icon :name="c.icon as any" :size="14" />
              <span>{{ c.label }}</span>
            </a>
          </div>

          <!-- 二级 -->
          <div v-show="!collapsed && isGroupOpen(g)" class="nav-sub">
            <NavItem
              v-for="c in g.children"
              :key="c.path"
              :path="c.path"
              :icon="c.icon"
              :label="c.label"
              :href="c.href"
              :active="isLeafActive(route.path, c.path)"
            />
          </div>
        </div>
      </nav>

      <div class="app-aside-foot">
        <div class="user-chip">
          <div class="avatar">{{ (auth.username || 'U')[0].toUpperCase() }}</div>
          <div class="user-meta">
            <div class="user-name">{{ auth.username }}</div>
            <div class="user-role">管理员</div>
          </div>
        </div>
        <el-tooltip content="退出登录" placement="right" :disabled="!collapsed">
          <button class="logout-btn" @click="onLogout">
            <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" y1="12" x2="9" y2="12"/></svg>
            <span class="logout-label">退出</span>
          </button>
        </el-tooltip>
      </div>
    </aside>

    <!-- 主区 -->
    <div class="app-main">
      <header class="app-header">
        <div class="app-breadcrumb">
          <template v-for="(b, i) in breadcrumbs" :key="b.to">
            <span v-if="i > 0" class="bc-sep">/</span>
            <router-link v-if="i < breadcrumbs.length - 1 && b.to" :to="b.to" class="bc-link">{{ b.label }}</router-link>
            <span v-else class="bc-current">{{ b.label }}</span>
          </template>
        </div>
        <div class="app-header-right">
          <!-- 全局「导出配置」按钮（常驻顶栏，任何页面一键可用） -->
          <el-tooltip
            placement="bottom"
            :show-after="150"
          >
            <template #content>
              <div class="export-tip">
                <div class="export-tip-title">导出配置 → config.json</div>
                <div class="export-tip-desc">
                  把设备/通道/点位/目标的改动写入 config.json，plcbridge 1s 内自动热重载。
                </div>
                <div class="export-tip-desc">
                  快捷键：Ctrl+Shift+E
                </div>
                <div v-if="autoExport" class="export-tip-desc export-tip-auto">
                  自动导出已开启 —— 改完配置约 1.5s 后会自动同步。
                </div>
                <div v-else class="export-tip-desc export-tip-auto">
                  自动导出未开启 —— 需要手动点此按钮。
                </div>
              </div>
            </template>
            <button
              class="export-btn"
              :class="{ 'is-dirty': dirty, 'is-busy': exporting, 'is-auto': autoExport }"
              :disabled="exporting"
              @click="doExportConfig"
            >
              <Icon :name="exporting ? 'refresh' : 'download'" :size="15" />
              <span class="export-btn-text">{{ exporting ? '导出中…' : '导出配置' }}</span>
              <span v-if="dirty && !exporting" class="export-badge">{{ pendingChanges || '!' }}</span>
              <span v-else-if="autoExport && !exporting" class="export-auto-dot" title="自动导出已开启"></span>
            </button>
          </el-tooltip>

          <!-- Dark Mode 切换（Plan3 3.6） -->
          <button class="theme-toggle-btn" :title="isDark ? '切换亮色模式' : '切换深色模式'" @click="toggleTheme()">
            <Icon :name="isDark ? 'sun' : 'moon'" :size="16" />
          </button>
          <span class="env-pill">
            <span class="env-dot"></span>
            {{ auth.username }} · 在线
          </span>
        </div>
      </header>
      <main class="app-content">
        <router-view />
      </main>
    </div>
  </div>
</template>

<style scoped>
.app-shell {
  display: flex;
  height: 100vh;
  background: #f3f5f9;
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
               "Microsoft YaHei", Roboto, Helvetica, Arial, sans-serif;
  color: #1f2937;
}

/* ===== 侧边栏 ===== */
.app-aside {
  width: 232px;
  flex-shrink: 0;
  background: linear-gradient(180deg, #0f172a 0%, #111c33 100%);
  color: #cbd5e1;
  display: flex;
  flex-direction: column;
  box-shadow: 1px 0 0 rgba(255,255,255,0.05);
  transition: width 180ms ease;
  overflow: hidden;
}
.app-shell.is-collapsed .app-aside { width: 64px; }
.app-shell.is-collapsed .brand-text,
.app-shell.is-collapsed .nav-label,
.app-shell.is-collapsed .nav-section-title,
.app-shell.is-collapsed .user-meta,
.app-shell.is-collapsed .logout-label { display: none; }
.app-shell.is-collapsed .app-brand {
  justify-content: center;
  padding: 14px 0 10px;
  gap: 0;
  position: relative;
}
.app-shell.is-collapsed .app-brand .collapse-btn {
  position: absolute;
  right: 6px;
  top: 50%;
  margin-top: -10px;
  width: 18px; height: 18px;
  transform: none;
  opacity: 0;
  transition: opacity 120ms ease, background 120ms ease;
}
.app-shell.is-collapsed .app-brand:hover .collapse-btn { opacity: 1; }
.app-shell.is-collapsed .app-brand .brand-logo {
  cursor: pointer;
  transition: transform 120ms ease, box-shadow 120ms ease;
}
.app-shell.is-collapsed .app-brand .brand-logo:hover {
  transform: scale(1.06);
  box-shadow: 0 4px 14px rgba(14,165,233,0.45);
}
.app-shell.is-collapsed .app-nav { padding: 14px 8px; }
.app-shell.is-collapsed .nav-item { justify-content: center; padding: 9px 0; gap: 0; }
.app-shell.is-collapsed .app-aside-foot { padding: 14px 8px; }
.app-shell.is-collapsed .user-chip { justify-content: center; padding: 8px 0; gap: 0; }
.app-shell.is-collapsed .logout-btn { padding: 8px 0; }
.app-shell.is-collapsed .collapse-btn { display: inline-flex; }

.app-brand {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 18px 20px;
  border-bottom: 1px solid rgba(255,255,255,0.06);
  position: relative;
}
.app-brand .collapse-btn {
  position: absolute;
  right: 10px;
  top: 50%;
  transform: translateY(-50%);
}
.brand-logo {
  width: 36px; height: 36px;
  border-radius: 8px;
  background: linear-gradient(135deg, #10b981 0%, #0ea5e9 100%);
  color: #fff;
  display: flex; align-items: center; justify-content: center;
  font-weight: 700;
  font-size: 18px;
  box-shadow: 0 4px 12px rgba(16,185,129,0.35);
  flex-shrink: 0;
}
.brand-text { flex: 1; min-width: 0; }
.brand-title { font-size: 16px; font-weight: 600; color: #f1f5f9; letter-spacing: 0.4px; }
.brand-sub { font-size: 11px; color: #64748b; margin-top: 2px; }

.collapse-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 24px; height: 24px;
  border: 1px solid rgba(255,255,255,0.1);
  background: rgba(255,255,255,0.04);
  color: #94a3b8;
  border-radius: 6px;
  cursor: pointer;
  transition: all 120ms ease;
  flex-shrink: 0;
}
.collapse-btn:hover {
  background: rgba(16,185,129,0.15);
  border-color: rgba(16,185,129,0.4);
  color: #10b981;
}

.app-nav { padding: 14px 12px; flex: 1; overflow-y: auto; }
.nav-section-title {
  font-size: 11px;
  color: #475569;
  letter-spacing: 1px;
  padding: 8px 12px 6px;
  text-transform: uppercase;
}
.nav-item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 9px 12px;
  margin-bottom: 2px;
  border-radius: 8px;
  color: #94a3b8;
  text-decoration: none;
  font-size: 14px;
  transition: background 120ms ease, color 120ms ease;
  cursor: pointer;
}
.nav-item:hover {
  background: rgba(255,255,255,0.04);
  color: #e2e8f0;
}
.nav-item.active {
  background: linear-gradient(90deg, rgba(16,185,129,0.18) 0%, rgba(16,185,129,0.05) 100%);
  color: #fff;
  box-shadow: inset 2px 0 0 #10b981;
}
.nav-icon { display: inline-flex; width: 18px; }
.nav-label { font-weight: 500; }

/* ===== 二级菜单 ===== */
.nav-group { margin-bottom: 2px; }

/* 分组标题：可点击展开，样式跟普通项对齐 */
.nav-group-head {
  width: 100%;
  background: transparent;
  border: none;
  font-family: inherit;
  text-align: left;
  position: relative;
}
.nav-group-head.open { color: #cbd5e1; }
.nav-group-caret {
  margin-left: auto;
  flex-shrink: 0;
  color: #64748b;
  transition: transform 160ms ease;
}

/* 分组下有激活项时，整组加高亮底 */
.nav-group-head.active {
  background: linear-gradient(90deg, rgba(16,185,129,0.14) 0%, rgba(16,185,129,0.04) 100%);
  color: #fff;
}

/* 二级容器：左侧缩进 + 竖线，视觉上从属于父级 */
.nav-sub {
  position: relative;
  padding-left: 14px;
  margin: 2px 0 4px;
}
.nav-sub::before {
  content: '';
  position: absolute;
  left: 6px;
  top: 4px;
  bottom: 4px;
  width: 1px;
  background: rgba(255,255,255,0.09);
}

/* 二级项：字号略小、默认色更淡 */
.nav-sub .nav-item {
  font-size: 13px;
  padding: 7px 12px;
  color: #7c8ba1;
}
.nav-sub .nav-item:hover {
  background: rgba(255,255,255,0.04);
  color: #e2e8f0;
}
.nav-sub .nav-item.active {
  background: rgba(16,185,129,0.12);
  color: #6ee7b7;
  box-shadow: inset 2px 0 0 #10b981;
}

/* 折叠态：隐藏二级与展开箭头 */
.app-shell.is-collapsed .nav-group-caret { display: none; }
.app-shell.is-collapsed .nav-sub { display: none !important; }

/* ===== 折叠态 hover 弹出二级 ===== */
.nav-flyout {
  position: absolute;
  left: 100%;
  top: 0;
  min-width: 168px;
  padding: 6px;
  border-radius: 10px;
  background: #16213a;
  border: 1px solid rgba(255,255,255,0.08);
  box-shadow: 0 10px 30px rgba(0,0,0,0.45);
  opacity: 0;
  visibility: hidden;
  transform: translateX(-6px);
  transition: opacity 140ms ease, transform 140ms ease, visibility 140ms;
  z-index: 40;
}
.nav-group { position: relative; }
.nav-group:hover .nav-flyout {
  opacity: 1;
  visibility: visible;
  transform: translateX(0);
}
.nav-flyout-title {
  font-size: 11px;
  color: #64748b;
  padding: 4px 10px 6px;
  letter-spacing: 0.5px;
}
.nav-flyout-item {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 7px 10px;
  border-radius: 7px;
  color: #94a3b8;
  font-size: 13px;
  text-decoration: none;
  cursor: pointer;
  transition: background 120ms ease, color 120ms ease;
}
.nav-flyout-item:hover {
  background: rgba(255,255,255,0.05);
  color: #e2e8f0;
}
.nav-flyout-item.active {
  background: rgba(16,185,129,0.14);
  color: #6ee7b7;
}

.app-aside-foot {
  padding: 14px;
  border-top: 1px solid rgba(255,255,255,0.06);
}
.user-chip {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 6px;
  border-radius: 8px;
  background: rgba(255,255,255,0.03);
  margin-bottom: 8px;
}
.avatar {
  width: 32px; height: 32px;
  border-radius: 50%;
  background: linear-gradient(135deg, #0ea5e9, #6366f1);
  color: #fff;
  display: flex; align-items: center; justify-content: center;
  font-weight: 600;
  font-size: 13px;
}
.user-meta { flex: 1; min-width: 0; }
.user-name {
  font-size: 13px;
  color: #e2e8f0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.user-role { font-size: 11px; color: #64748b; }

.logout-btn {
  width: 100%;
  display: flex; align-items: center; justify-content: center; gap: 6px;
  padding: 8px 12px;
  border-radius: 8px;
  border: 1px solid rgba(255,255,255,0.08);
  background: transparent;
  color: #94a3b8;
  font-size: 13px;
  cursor: pointer;
  transition: all 120ms ease;
}
.logout-btn:hover {
  background: rgba(248,113,113,0.12);
  border-color: rgba(248,113,113,0.3);
  color: #fca5a5;
}

/* ===== 主区 ===== */
.app-main { flex: 1; min-width: 0; display: flex; flex-direction: column; }

.app-header {
  height: 56px;
  background: #fff;
  border-bottom: 1px solid #e5e7eb;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 24px;
  box-shadow: 0 1px 2px rgba(0,0,0,0.02);
}
.app-breadcrumb {
  display: flex; align-items: center; gap: 8px;
  font-size: 13px;
}
.bc-link {
  color: #6b7280;
  text-decoration: none;
}
.bc-link:hover { color: #10b981; }
.bc-sep { color: #d1d5db; }
.bc-current { color: #111827; font-weight: 600; }

.app-header-right { display: flex; align-items: center; gap: 12px; }

/* ===== 全局「导出配置」按钮 ===== */
.export-btn {
  position: relative;
  display: inline-flex;
  align-items: center;
  gap: 7px;
  height: 32px;
  padding: 0 12px;
  border-radius: 8px;
  border: 1px solid #d1d5db;
  background: #fff;
  color: #374151;
  font-size: 13px;
  font-weight: 500;
  cursor: pointer;
  transition: all 140ms ease;
}
.export-btn:hover:not(:disabled) {
  border-color: #10b981;
  color: #059669;
  background: #ecfdf5;
}
.export-btn:disabled { opacity: 0.6; cursor: not-allowed; }

/* 有未导出改动：高亮 + 角标，视觉上"很显眼" */
.export-btn.is-dirty {
  border-color: #f59e0b;
  background: #fffbeb;
  color: #b45309;
  font-weight: 600;
  box-shadow: 0 0 0 3px rgba(245, 158, 11, 0.14);
}
.export-btn.is-dirty:hover:not(:disabled) {
  border-color: #d97706;
  background: #fef3c7;
  color: #92400e;
}
.export-btn.is-busy svg { animation: export-spin 900ms linear infinite; }
@keyframes export-spin {
  from { transform: rotate(0deg); }
  to   { transform: rotate(360deg); }
}

.export-badge {
  min-width: 17px;
  height: 17px;
  padding: 0 5px;
  border-radius: 999px;
  background: #f59e0b;
  color: #fff;
  font-size: 11px;
  font-weight: 700;
  line-height: 17px;
  text-align: center;
}

.export-tip { max-width: 260px; line-height: 1.5; }
.export-tip-title { font-weight: 600; margin-bottom: 3px; }
.export-tip-desc { font-size: 12px; opacity: 0.85; }
.export-tip-auto { margin-top: 4px; font-weight: 600; }

/* 自动导出已开启：按钮左侧加一个绿色小圆点（区别于"脏"状态的琥珀色） */
.export-btn.is-auto { border-color: #34d399; color: #047857; }
.export-btn.is-auto:hover:not(:disabled) {
  border-color: #10b981;
  background: #ecfdf5;
  color: #065f46;
}
.export-auto-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: #10b981;
  box-shadow: 0 0 0 3px rgba(16, 185, 129, 0.2);
  flex-shrink: 0;
}
/* 脏状态优先级高于自动导出状态（脏 = 更需要用户注意） */
.export-btn.is-dirty.export-btn.is-auto { border-color: #f59e0b; color: #b45309; }
.export-btn.is-dirty.export-btn.is-auto:hover:not(:disabled) {
  border-color: #d97706; background: #fef3c7; color: #92400e;
}
:global(.dark) .export-btn.is-auto {
  border-color: #10b981;
  color: #34d399;
}
:global(.dark) .export-btn.is-auto:hover:not(:disabled) {
  background: rgba(16, 185, 129, 0.12);
  color: #6ee7b7;
}
:global(.dark) .export-btn.is-dirty.export-btn.is-auto {
  border-color: #f59e0b;
  color: #fbbf24;
}

:global(.dark) .export-btn {
  background: #1e293b;
  border-color: #334155;
  color: #cbd5e1;
}
:global(.dark) .export-btn:hover:not(:disabled) {
  border-color: #10b981;
  color: #34d399;
  background: rgba(16, 185, 129, 0.12);
}
:global(.dark) .export-btn.is-dirty {
  border-color: #f59e0b;
  background: rgba(245, 158, 11, 0.14);
  color: #fbbf24;
}
:global(.dark) .export-btn.is-dirty:hover:not(:disabled) {
  border-color: #fbbf24;
  background: rgba(245, 158, 11, 0.22);
  color: #fcd34d;
}

.theme-toggle-btn {
  display: inline-flex; align-items: center; justify-content: center;
  width: 32px; height: 32px; border-radius: 8px; border: none;
  background: transparent; cursor: pointer; color: #6b7280;
  transition: background 120ms, color 120ms;
}
.theme-toggle-btn:hover { background: #f3f4f6; color: #1f2937; }
:global(.dark) .theme-toggle-btn:hover { background: #374151; color: #f9fafb; }
.env-pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 10px;
  border-radius: 999px;
  background: #ecfdf5;
  color: #065f46;
  font-size: 12px;
  font-weight: 500;
}
.env-dot {
  width: 6px; height: 6px;
  border-radius: 50%;
  background: #10b981;
  box-shadow: 0 0 0 3px rgba(16,185,129,0.2);
}

.app-content {
  flex: 1;
  overflow-y: auto;
  padding: 24px;
}
</style>
