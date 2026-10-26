<script setup lang="ts">
/**
 * 设置中心（Settings Hub）
 *
 * 为什么把设置类页面收进一个带标签页的容器：
 *   侧边栏一度有 11 个菜单项（导航 8 + 系统 3），其中大半是低频的设置页。
 *   菜单一多，"常用的"就被"不常用的"淹没 —— 找仪表盘反而变慢了。
 *
 * 做法：侧边栏只留真正的高频入口，剩余设置项以标签页形式收进「设置」。
 *   - 通用 / 账号 / 备份 / 变更历史 / 快捷键 / 图标库
 *
 * 关键设计：**不复制页面**，直接复用原组件。
 *   每个标签页挂的就是原来那个 .vue 文件（懒加载），
 *   所以行为、样式、后续改动全都跟拆开时一模一样 —— 合并只是换了容器。
 *
 * URL 同步：当前标签写进 ?tab=，所以：
 *   - 可以直接分享 /settings?tab=backup 这样的链接
 *   - 旧的独立路由（/settings/backup 等）重定向过来，书签不会失效
 */
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Icon from '@/components/Icon.vue'

const route = useRoute()
const router = useRouter()

interface TabDef {
  key: string
  label: string
  icon: string
  desc: string
  comp: () => Promise<any>
}

const TABS: TabDef[] = [
  {
    key: 'general',
    label: '通用',
    icon: 'settings',
    desc: 'Web 控制台自身的运行行为，不参与采集配置。',
    comp: () => import('@/pages/settings/General.vue'),
  },
  {
    key: 'account',
    label: '账号',
    icon: 'user',
    desc: '当前登录账号与密码修改。',
    comp: () => import('@/pages/settings/Account.vue'),
  },
  {
    key: 'backup',
    label: '配置备份',
    icon: 'download',
    desc: '导出 / 恢复整份配置（SQLite + 附件）。',
    comp: () => import('@/pages/settings/Backup.vue'),
  },
  {
    key: 'history',
    label: '变更历史',
    icon: 'clock',
    desc: '每次配置写入的自动快照，可回滚到任意历史版本。',
    comp: () => import('@/pages/settings/History.vue'),
  },
  {
    key: 'shortcuts',
    label: '快捷键',
    icon: 'command',
    desc: '全局与页面级快捷键清单（与实际注册表同源）。',
    comp: () => import('@/pages/system/Shortcuts.vue'),
  },
  {
    key: 'icons',
    label: '图标库',
    icon: 'feather',
    desc: '内置图标浏览与复制，供开发取用。',
    comp: () => import('@/pages/system/Icons.vue'),
  },
]

const tabKeys = TABS.map(t => t.key)

/** 当前标签：只认白名单里的 key，避免手改 URL 跳到不存在的面板。 */
const activeTab = ref('general')
watch(
  () => route.query.tab,
  (q) => {
    const v = Array.isArray(q) ? q[0] : q
    activeTab.value = v && tabKeys.includes(v) ? v : 'general'
  },
  { immediate: true },
)

const currentTab = computed(() => TABS.find(t => t.key === activeTab.value) || TABS[0])

function onTabChange(key: string | number) {
  // 用 replace 而不是 push：切标签不该往浏览器历史里塞一堆记录，
  // 否则用户按「后退」会逐个标签往回退，体验很差。
  router.replace({ path: '/settings', query: { tab: String(key) } })
}
</script>

<template>
  <div class="settings-hub">
    <div class="hub-header">
      <h2 class="page-title">设置</h2>
      <p class="page-desc">
        Web 控制台与配置管理的全部设置项。
        <span class="hub-current">{{ currentTab.desc }}</span>
      </p>
    </div>

    <el-tabs
      :model-value="activeTab"
      class="hub-tabs"
      tab-position="top"
      @tab-change="onTabChange"
    >
      <el-tab-pane
        v-for="t in TABS"
        :key="t.key"
        :name="t.key"
        :label="t.label"
        lazy
      >
        <template #label>
          <span class="tab-label">
            <Icon :name="t.icon as any" :size="15" />
            <span>{{ t.label }}</span>
          </span>
        </template>

        <!-- 每个标签页挂的就是原来那个独立页面组件 -->
        <component :is="t.comp" />
      </el-tab-pane>
    </el-tabs>
  </div>
</template>

<style scoped>
.settings-hub { max-width: 1100px; margin: 0 auto; }

.hub-header { margin-bottom: 12px; }
.page-title { font-size: 20px; font-weight: 600; margin: 0 0 4px; color: #0f172a; }
.page-desc { font-size: 13px; color: #64748b; margin: 0; line-height: 1.6; }
.hub-current {
  display: inline-block;
  margin-left: 6px;
  padding-left: 8px;
  border-left: 2px solid #e2e8f0;
  color: #94a3b8;
}

/* 标签栏样式：默认 el-tabs 的下划线偏细，这里加重一点让层级更清楚 */
.hub-tabs :deep(.el-tabs__header) { margin-bottom: 18px; }
.hub-tabs :deep(.el-tabs__item) {
  font-size: 14px;
  font-weight: 500;
  color: #64748b;
}
.hub-tabs :deep(.el-tabs__item.is-active) { color: #10b981; font-weight: 600; }
.hub-tabs :deep(.el-tabs__active-bar) { background-color: #10b981; height: 2px; }

.tab-label { display: inline-flex; align-items: center; gap: 6px; }

:global(.dark) .page-title { color: #f1f5f9; }
:global(.dark) .page-desc { color: #94a3b8; }
:global(.dark) .hub-current { border-left-color: #334155; }
</style>
