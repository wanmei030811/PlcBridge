<script setup lang="ts">
import { computed, ref, onMounted, watch, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { getDevice, type Device } from '@/api/devices'
import Icon from '@/components/Icon.vue'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()

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

interface NavItem { path: string; label: string; icon: string; group: 'main' | 'system' }
const navItems: NavItem[] = [
  { path: '/dashboard', label: '仪表盘', icon: 'grid', group: 'main' },
  { path: '/devices', label: '设备', icon: 'cpu', group: 'main' },
  { path: '/channels', label: '通道', icon: 'share', group: 'main' },
  { path: '/tokens', label: 'API Token', icon: 'key', group: 'main' },
  { path: '/settings/account', label: '账号 / 密码', icon: 'user', group: 'main' },
  { path: '/system/icons', label: '图标库', icon: 'feather', group: 'system' }
]

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
    '/system/icons': '图标库'
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
  return out
})

async function onLogout() {
  await auth.logout()
  router.replace('/login')
}
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
        <div class="nav-section-title">导航</div>
        <el-tooltip
          v-for="item in navItems.filter(i => i.group === 'main')"
          :key="item.path"
          :content="item.label"
          placement="right"
          :disabled="!collapsed"
        >
          <router-link
            :to="item.path"
            class="nav-item"
            :class="{ active: route.path === item.path || route.path.startsWith(item.path + '/') }"
          >
            <span class="nav-icon">
              <Icon :name="item.icon as any" :size="18" />
            </span>
            <span class="nav-label">{{ item.label }}</span>
          </router-link>
        </el-tooltip>

        <div class="nav-section-title" style="margin-top:14px;">系统</div>
        <el-tooltip
          v-for="item in navItems.filter(i => i.group === 'system')"
          :key="item.path"
          :content="item.label"
          placement="right"
          :disabled="!collapsed"
        >
          <router-link
            :to="item.path"
            class="nav-item"
            :class="{ active: route.path === item.path || route.path.startsWith(item.path + '/') }"
          >
            <span class="nav-icon">
              <Icon :name="item.icon as any" :size="18" />
            </span>
            <span class="nav-label">{{ item.label }}</span>
          </router-link>
        </el-tooltip>
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
