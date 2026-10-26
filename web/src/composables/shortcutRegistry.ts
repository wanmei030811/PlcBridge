/**
 * 全局快捷键清单（单一数据源）
 *
 * 为什么要有这个文件：
 *   之前快捷键只在 useGlobalShortcuts.ts 里以数组字面量的形式存在，
 *   想在界面上"列出所有快捷键"就只能把那段代码再抄一遍 ——
 *   抄的结果必然和实际行为漂移（加了新快捷键忘了同步文档）。
 *
 * 现在这个文件是**唯一**的快捷键定义处：
 *   - buildGlobalShortcuts() 产出运行时 handler（带实际行为）
 *   - GLOBAL_SHORTCUT_META  产出展示元数据（带说明文案）
 *   - 两者由同一组 id 关联，useGlobalShortcuts 会在注册时断言 id 一一对应，
 *     对不上就直接抛错 —— 漂移在开发期就暴露，而不是等用户看文档时发现。
 *
 * 页面级快捷键（某个组件自己注册的）通过 registerPageShortcut 上报，
 * 同样会出现在「快捷键」页面里。
 */
import type { ShortcutHandler, ShortcutMeta } from './useGlobalShortcuts'

export type { ShortcutMeta }

/** 快捷键分组（页面展示顺序）。 */
export const SHORTCUT_GROUPS = [
  '导航',
  '配置',
  '页面操作',
  '通用',
] as const

export type ShortcutGroup = typeof SHORTCUT_GROUPS[number]

/**
 * 全局快捷键定义表。
 *
 * 每项包含 id / 组合键 / 说明 / 行为，四者在一起 ——
 * 这样加一个快捷键只需要改这一个文件。
 */
export interface GlobalShortcutDef {
  id: string
  /** 主键名（大小写不敏感） */
  key: string
  /** 组合键描述，如 'Ctrl+Shift+E' */
  label: string
  /** 功能说明 */
  desc: string
  group: ShortcutGroup
  /** 是否要求 Ctrl 修饰 */
  ctrl?: boolean
  /** 是否要求 Shift 修饰 */
  shift?: boolean
  /** 是否要求 Alt 修饰 */
  alt?: boolean
  /** 是否要求 Meta(cmd) 修饰 */
  meta?: boolean
  /** 生效范围说明（如"仅设备详情页"） */
  scope?: string
  /** 实际行为 */
  action: (ctx: GlobalShortcutContext) => void
}

/**
 * 快捷键行为执行上下文。
 *
 * 用 context 对象而不是直接闭包捕获 router，是因为这份定义表是
 * **纯数据**（不依赖 Vue 实例），才能被「快捷键」页面安全地 import
 * 而不触发 router 的初始化副作用。
 */
export interface GlobalShortcutContext {
  /** 跳转到指定路由 */
  navigate: (path: string) => void
  /** 当前路由路径（用于判断是否在某个页面） */
  currentPath: () => string
  /** 派发一个自定义事件（页面自行监听） */
  emit: (name: string) => void
  /** 重新加载页面 */
  reload: () => void
  /** 聚焦当前页面的搜索框 */
  focusSearch: () => boolean
}

/** 全局快捷键定义表（单一数据源）。 */
export const GLOBAL_SHORTCUT_DEFS: GlobalShortcutDef[] = [
  // ===== 导航 =====
  {
    id: 'nav.devices',
    key: 'n',
    label: 'Ctrl+N',
    desc: '跳转到设备列表',
    group: '导航',
    ctrl: true,
    action: (ctx) => ctx.navigate('/devices'),
  },
  {
    id: 'nav.channels',
    key: 'c',
    label: 'Ctrl+Shift+C',
    desc: '跳转到通道列表',
    group: '导航',
    ctrl: true,
    shift: true,
    action: (ctx) => ctx.navigate('/channels'),
  },
  {
    id: 'nav.dashboard',
    key: 'd',
    label: 'Ctrl+D',
    desc: '跳转到仪表盘',
    group: '导航',
    ctrl: true,
    action: (ctx) => ctx.navigate('/dashboard'),
  },

  // ===== 配置 =====
  {
    id: 'config.export',
    key: 'e',
    label: 'Ctrl+Shift+E',
    desc: '导出配置（写 config.json，plcbridge 1s 内自动热重载）',
    group: '配置',
    ctrl: true,
    shift: true,
    action: (ctx) => ctx.emit('plcbridge:export-config'),
  },
  {
    id: 'config.save',
    key: 's',
    label: 'Ctrl+S',
    desc: '保存当前页面（由页面自行实现保存逻辑）',
    group: '配置',
    ctrl: true,
    action: (ctx) => ctx.emit('plcbridge:save'),
  },

  // ===== 页面操作 =====
  {
    id: 'page.search',
    key: 'k',
    label: 'Ctrl+K',
    desc: '聚焦当前页面的搜索框',
    group: '页面操作',
    ctrl: true,
    action: (ctx) => { ctx.focusSearch() },
  },
  {
    id: 'page.back',
    key: 'Escape',
    label: 'Esc',
    desc: '返回设备列表',
    group: '页面操作',
    scope: '仅设备详情页',
    action: (ctx) => {
      // 只在设备详情页拦截 Escape；其它页面让浏览器/Element Plus 正常处理
      // （例如关对话框），不要抢。
      if (/^\/devices\/\d+/.test(ctx.currentPath())) {
        ctx.navigate('/devices')
      }
    },
  },
  {
    id: 'common.refresh',
    key: 'r',
    label: 'Ctrl+R',
    desc: '刷新当前页面',
    group: '通用',
    ctrl: true,
    action: (ctx) => ctx.reload(),
  },
]

/**
 * 页面级快捷键（动态注册）。
 *
 * 某些页面想加自己的快捷键时，用 useShortcutScope 注册并填 meta，
 * 就会自动出现在「快捷键」页面 —— 不用再改这个文件。
 */
export interface PageShortcutMeta extends ShortcutMeta {
  /** 与 ShortcutHandler.key + 修饰键组合成唯一标识 */
  id: string
}

/** 已注册的页面级快捷键（模块级，供页面渲染）。 */
const pageShortcuts = new Map<string, PageShortcutMeta & { handler: (e: KeyboardEvent) => void }>()

/** 页面级快捷键变化时通知订阅者（页面用来触发重渲染）。 */
type PageShortcutListener = () => void
const pageShortcutListeners = new Set<PageShortcutListener>()

/** 通知所有订阅者页面级快捷键列表已变化。 */
export function notifyPageShortcutsChanged() {
  pageShortcutListeners.forEach(fn => fn())
}

/** 订阅页面级快捷键变化，返回取消订阅函数。 */
export function subscribePageShortcuts(fn: PageShortcutListener): () => void {
  pageShortcutListeners.add(fn)
  return () => { pageShortcutListeners.delete(fn) }
}

/**
 * 注册页面级快捷键的元数据。
 *
 * 由 useShortcutScope 内部调用；页面一般不直接用。
 */
export function registerPageShortcut(
  id: string,
  meta: PageShortcutMeta,
  handler: (e: KeyboardEvent) => void,
) {
  pageShortcuts.set(id, { ...meta, handler })
  notifyPageShortcutsChanged()
}

/** 注销页面级快捷键的元数据。 */
export function unregisterPageShortcut(id: string) {
  if (pageShortcuts.delete(id)) {
    notifyPageShortcutsChanged()
  }
}

/** 取所有已注册的页面级快捷键（含 handler，用于文档展示）。 */
export function getPageShortcuts() {
  return [...pageShortcuts.values()]
}

/**
 * 把定义表转成运行时 handler 列表。
 *
 * 用一个工厂函数而不是直接写数组字面量，是为了让「快捷键」页面
 * 拿到的 meta 和运行时注册的 handler **来自同一次遍历** —— 结构上保证一致。
 */
export function buildGlobalShortcuts(ctx: GlobalShortcutContext): ShortcutHandler[] {
  return GLOBAL_SHORTCUT_DEFS.map(def => ({
    key: def.key,
    label: def.label,
    ctrl: def.ctrl,
    shift: def.shift,
    alt: def.alt,
    meta: def.meta,
    handler: () => def.action(ctx),
  }))
}

/** 把定义表转成展示元数据（供页面渲染）。 */
export function buildGlobalShortcutMetas(): ShortcutMeta[] {
  return GLOBAL_SHORTCUT_DEFS.map(def => ({
    label: def.label,
    desc: def.desc,
    group: def.group,
    scope: def.scope,
  }))
}
