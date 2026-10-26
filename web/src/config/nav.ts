/**
 * 侧边栏导航配置（两级菜单）。
 *
 * 分组原则：
 *   一级只放"天天点"的东西（仪表盘 + 三个功能域），
 *   低频的运维 / 开发工具收进二级，把一级菜单压到 4 项。
 *
 * 为什么做成数据而不是写死在模板里：
 *   面包屑要按同样的层级推导（当前页属于哪个域 → 域名 / 页名），
 *   两处各写一份必然会漂移，所以共用这一份定义。
 */

export interface NavLeaf {
  /** 路由路径；外部链接用 href */
  path: string
  label: string
  icon: string
  /** 外部链接（如 Swagger），存在时渲染 <a target="_blank"> */
  href?: string
}

export interface NavGroup {
  /** 分组 key，用于展开状态持久化 */
  key: string
  label: string
  icon: string
  children: NavLeaf[]
}

/** 顶层直达项（不需要二级） */
export const navLeaves: NavLeaf[] = [
  { path: '/dashboard', label: '仪表盘', icon: 'grid' },
]

/** 二级分组 */
export const navGroups: NavGroup[] = [
  {
    key: 'collect',
    label: '采集配置',
    icon: 'cpu',
    children: [
      { path: '/devices', label: '设备', icon: 'server' },
      { path: '/channels', label: '通道', icon: 'share' },
    ],
  },
  {
    key: 'system',
    label: '系统',
    icon: 'settings',
    children: [
      { path: '/settings/general', label: '通用设置', icon: 'sliders' },
      { path: '/settings/account', label: '账号 / 密码', icon: 'user' },
      { path: '/tokens', label: 'API Token', icon: 'key' },
      { path: '/settings/history', label: '变更历史', icon: 'clock' },
      { path: '/settings/backup', label: '配置备份', icon: 'download' },
    ],
  },
  {
    key: 'dev',
    label: '开发者',
    icon: 'code',
    children: [
      { path: '/swagger', label: 'API 文档', icon: 'book-open', href: '/swagger/' },
      { path: '/system/shortcuts', label: '快捷键', icon: 'command' },
      { path: '/system/icons', label: '图标库', icon: 'feather' },
    ],
  },
]

/** 路径是否命中某个叶子项（兼容 /devices/:id 这类子路径） */
export function isLeafActive(path: string, leafPath: string): boolean {
  return path === leafPath || path.startsWith(leafPath + '/')
}

/** 找出当前路径所属的分组；不在任何分组里时返回 null */
export function findGroupOf(path: string): NavGroup | null {
  for (const g of navGroups) {
    for (const c of g.children) {
      if (isLeafActive(path, c.path)) return g
    }
  }
  return null
}
