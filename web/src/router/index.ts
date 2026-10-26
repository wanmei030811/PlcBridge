import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const routes = [
  { path: '/login', name: 'login', component: () => import('@/pages/Login.vue'), meta: { public: true } },
  {
    path: '/',
    component: () => import('@/layouts/Default.vue'),
    children: [
      { path: '', redirect: '/dashboard' },
      { path: 'dashboard', name: 'dashboard', component: () => import('@/pages/Dashboard.vue') },
      { path: 'channels', name: 'channels', component: () => import('@/pages/channels/List.vue') },
      { path: 'devices', name: 'devices', component: () => import('@/pages/devices/List.vue') },
      { path: 'devices/:id', name: 'device-detail', component: () => import('@/pages/devices/Detail.vue') },
      { path: 'targets', redirect: '/devices' },
      { path: 'tokens', name: 'tokens', component: () => import('@/pages/Tokens.vue') },
      { path: 'settings/account', name: 'account', component: () => import('@/pages/settings/Account.vue') },
      { path: 'settings/backup', name: 'backup', component: () => import('@/pages/settings/Backup.vue') },
      { path: 'settings/history', name: 'config-history', component: () => import('@/pages/settings/History.vue') },
      { path: 'settings/general', name: 'settings-general', component: () => import('@/pages/settings/General.vue') },
      { path: 'system/icons', name: 'system-icons', component: () => import('@/pages/system/Icons.vue') },
      { path: 'system/shortcuts', name: 'system-shortcuts', component: () => import('@/pages/system/Shortcuts.vue') }
    ]
  },
  // catch-all 404
  { path: '/:pathMatch(.*)*', redirect: '/dashboard' }
]

const router = createRouter({
  history: createWebHistory(),
  routes
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  if (!auth.initialized) {
    await auth.refresh()
  }
  if (!to.meta.public && !auth.username) {
    // 未登录时重定向，统一用 next 参数，值要 encodeURIComponent
    const next = encodeURIComponent(to.fullPath)
    return { name: 'login', query: { next } }
  }
  if (to.name === 'login' && auth.username) {
    return { path: '/' }
  }
})

export default router
