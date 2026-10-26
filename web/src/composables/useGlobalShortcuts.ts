/**
 * 全局键盘快捷键（Plan3 3.8）
 *
 * 设计要点：
 *   - 单一 window 监听器 + 订阅者列表：多个组件可同时注册快捷键，
 *     卸载其中一个不会误删另一个的监听（早期版本用模块级变量存 handler，
 *     后注册者会覆盖先注册者，卸载时把别人的监听也摘掉）。
 *   - 输入框聚焦时不触发：输入框里的 'n' / 's' 是字面输入，不是快捷键。
 *   - 组合键匹配只看已声明的修饰键 + 主键字面量，不做系统级快捷键拦截。
 */
import { onMounted, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import {
  buildGlobalShortcuts,
  registerPageShortcut,
  unregisterPageShortcut,
  type GlobalShortcutContext,
  type PageShortcutMeta,
} from './shortcutRegistry'

export interface ShortcutHandler {
  /** 按键名，大小写不敏感（如 'n' / 'Escape'） */
  key: string
  /** 人类可读的组合键描述，如 'Ctrl+N' */
  label: string
  /** 是否要求 Ctrl 修饰 */
  ctrl?: boolean
  /** 是否要求 Shift 修饰 */
  shift?: boolean
  /** 是否要求 Alt 修饰 */
  alt?: boolean
  /** 是否要求 Meta(cmd) 修饰 */
  meta?: boolean
  /** 处理器 */
  handler: (e: KeyboardEvent) => void
}

/**
 * 快捷键的展示元数据（供「快捷键」页面渲染）。
 *
 * 与 ShortcutHandler 分开的原因：
 *   - ShortcutHandler 是**运行时**契约（key + 修饰键 + handler），注册时用。
 *   - ShortcutMeta 是**文档**契约（说明文案、分组），展示时用。
 *
 * 为什么元数据不直接挂在 handler 上：
 *   handler 是闭包，不能被 JSON 序列化/静态分析；把文档信息独立出来，
 *   「快捷键」页面就能拿到一份可枚举、可分组的清单 —— 而这份清单和
 *   实际注册的行为由同一个工厂函数产出，天然不会漂移。
 */
export interface ShortcutMeta {
  /** 组合键描述，如 'Ctrl+Shift+E' */
  label: string
  /** 功能说明 */
  desc: string
  /** 所属分组（用于页面分节） */
  group: string
  /** 是否只在特定页面生效（如 Escape 只在设备详情页） */
  scope?: string
}

/** 当前已注册的所有快捷键（订阅者列表）。 */
let subscribers: ShortcutHandler[][] = []
let windowListener: ((e: KeyboardEvent) => void) | null = null

function matchesCombo(e: KeyboardEvent, s: ShortcutHandler): boolean {
  // 修饰键**严格**匹配：声明了什么就必须是什么，没声明的必须没按。
  //
  // 为什么严格（而不是"只校验声明过的"）：
  //   - 只校验声明过的 → 声明 Ctrl+C 的快捷键会被"裸 C"命中（因为裸 C 满足
  //     "ctrl 已按下"这一条？不满足，但反过来：声明 Ctrl+K 时裸 K 不匹配，
  //     而声明"无修饰"的 Escape 会被 Ctrl+Escape 命中）。
  //   - 严格匹配后，每个组合键只有唯一一种按键方式能命中，行为可预期。
  //
  // 注意 !!s.xxx 的归一化：未声明的修饰键是 undefined，必须先转成 false，
  // 否则 `undefined !== false` 恒成立 —— 所有快捷键都不会命中（静默失效）。
  if (!!s.ctrl !== !!e.ctrlKey) return false
  if (!!s.shift !== !!e.shiftKey) return false
  if (!!s.alt !== !!e.altKey) return false
  if (!!s.meta !== !!e.metaKey) return false
  return e.key.toLowerCase() === s.key.toLowerCase()
}

/**
 * 事件是否发生在可编辑元素里（输入框 / textarea / contenteditable）。 */
function isInEditable(e: KeyboardEvent): boolean {
  const target = e.target as HTMLElement | null
  if (!target) return false
  if (target.isContentEditable) return true
  const tag = target.tagName
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT'
}

/**
 * 找一个"看起来像搜索框"的输入（placeholder 含"搜索"/"search"，或带 search 类名）。
 *
 * 作为 .global-search-input 的兜底：让"Ctrl+K 聚焦搜索框"在没有显式标记
 * 的页面上也能工作，而不必每个页面都记得加 class。
 */
function findSearchLikeInput(): HTMLInputElement | null {
  const inputs = [...document.querySelectorAll('input')] as HTMLInputElement[]
  const hit = inputs.find(inp => {
    const ph = (inp.placeholder || '').toLowerCase()
    const cls = (inp.className || '').toLowerCase()
    return ph.includes('搜索') || ph.includes('search') || cls.includes('search')
  })
  return hit ?? null
}

/** 全局唯一的 keydown 处理器：遍历所有订阅者，命中即执行。 */
function onKeydown(e: KeyboardEvent) {
  if (isInEditable(e)) return
  for (const group of subscribers) {
    for (const s of group) {
      if (matchesCombo(e, s)) {
        e.preventDefault()
        e.stopPropagation()
        s.handler(e)
        return
      }
    }
  }
}

/** 首次注册时挂上 window 监听，最后一个订阅者卸载时摘掉。 */
function ensureListener() {
  if (windowListener) return
  windowListener = onKeydown
  window.addEventListener('keydown', windowListener, { capture: true })
}

function releaseListener() {
  if (!windowListener) return
  window.removeEventListener('keydown', windowListener, { capture: true })
  windowListener = null
}

/**
 * 注册一组全局键盘快捷键。
 *
 * 返回 unregister 函数，组件卸载时调用。
 */
export function useGlobalShortcuts(customShortcuts: ShortcutHandler[] = []): () => void {
  const group = [...customShortcuts]
  subscribers = [...subscribers, group]
  ensureListener()

  let released = false
  return () => {
    if (released) return
    released = true
    subscribers = subscribers.filter(g => g !== group)
    if (!subscribers.length) releaseListener()
  }
}

/**
 * 注册布局级快捷键（仅在已登录的 app shell 内生效）。
 *
 * 快捷键的**定义**在 shortcutRegistry.ts（GLOBAL_SHORTCUT_DEFS），
 * 这里是唯一的"接线"处：把定义表里的行为接到 router / window 上。
 *
 * 这样「快捷键」页面展示的清单和实际行为来自同一份数据，不会漂移。
 *
 * 完整清单见 shortcutRegistry.ts：
 *   Ctrl+N 设备 · Ctrl+Shift+C 通道 · Ctrl+D 仪表盘
 *   Ctrl+Shift+E 导出配置 · Ctrl+S 保存
 *   Ctrl+K 搜索框 · Esc 返回列表 · Ctrl+R 刷新
 */
export function useRouteShortcuts(router: ReturnType<typeof useRouter>) {
  const ctx: GlobalShortcutContext = {
    navigate: (path) => { router.push(path) },
    currentPath: () => router.currentRoute.value.path,
    emit: (name) => { window.dispatchEvent(new CustomEvent(name)) },
    reload: () => { window.location.reload() },
    focusSearch: () => {
      // 聚焦当前页面的搜索框。
      //
      // 优先用页面显式标记的 .global-search-input；没有的话，退而求其次找
      // 当前页面里第一个"看起来像搜索框"的输入（placeholder 里带"搜索"）。
      //
      // 注意：Element Plus 的 <el-input class="global-search-input"> 把 class 挂在了
      // **根 div** 上，真正的 <input> 是它的子节点。所以这里要取 inner input，
      // 否则 focus() 作用在 div 上什么也不会发生（静默失败）。
      const holder = document.querySelector('.global-search-input') as HTMLElement | null
      const input = (holder
        ? (holder.tagName === 'INPUT'
            ? holder
            : holder.querySelector('input'))
        : findSearchLikeInput()) as HTMLInputElement | null
      if (!input) return false
      input.focus()
      input.select?.()
      return true
    },
  }

  return useGlobalShortcuts(buildGlobalShortcuts(ctx))
}

/**
 * 返回当前所有已注册快捷键的列表（用于设置页面展示）。
 */
export function getRegisteredShortcuts(): ShortcutHandler[] {
  return subscribers.flat()
}

/**
 * 在组件 setup 里调用：注册快捷键并在卸载时自动清理。
 *
 * 比手动 `const unregister = useGlobalShortcuts(...)` + onUnmounted 更省事。
 *
 * meta 可选：填了就会出现在「快捷键」页面（页面级快捷键的自文档化途径）。
 */
export function useShortcutScope(shortcuts: ShortcutHandler[] = [], meta?: PageShortcutMeta) {
  let unregister: (() => void) | null = null
  onMounted(() => {
    unregister = useGlobalShortcuts(shortcuts)
    if (meta) {
      registerPageShortcut(meta.id, meta, shortcuts[0]?.handler ?? (() => {}))
    }
  })
  onUnmounted(() => {
    unregister?.()
    unregister = null
    if (meta) unregisterPageShortcut(meta.id)
  })
}
