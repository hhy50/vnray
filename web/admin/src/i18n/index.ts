// i18n 运行时：按 key 取词条 + 切换语言时让模板重渲染。
// 不引入 vue-i18n——本项目只需要「两级词条 + {name} 插值」，标准库级别的实现
// 换来零新增依赖；复数、日期格式等能力由各调用点自行处理。
import { readonly, ref } from 'vue'
import zhCN from './locales/zh-CN'
import type { Messages } from './locales/zh-CN'
import enUS from './locales/en-US'

// LocaleCode 是可选语言；新增语言时同时补 availableLocales 与 catalogs。
export type LocaleCode = 'zh-CN' | 'en-US'

// LocaleStorageKey 是用户显式选择语言的持久化位置。
export const LocaleStorageKey = 'vnray.locale'

// availableLocales 是语言切换器展示的候选（label 用各自语言书写）。
export const availableLocales: { code: LocaleCode; label: string }[] = [
  { code: 'zh-CN', label: '中文' },
  { code: 'en-US', label: 'English' },
]

// MessageKey 是词条键的联合类型：写成 "命名空间.键"，拼错时编辑器直接报错。
type NamespaceKeys = {
  [N in keyof Messages]: `${N & string}.${keyof Messages[N] & string}`
}[keyof Messages]

// MessageKey 见上；导出给需要显式标注键的调用点（如路由 meta）。
export type MessageKey = NamespaceKeys

type Params = Record<string, string | number>

const catalogs: Record<LocaleCode, Messages> = {
  'zh-CN': zhCN,
  'en-US': enUS,
}

// fallback 是缺词条时的兜底语言，也是首次进入的默认语言。
const fallback: LocaleCode = 'zh-CN'

function isLocale(value: unknown): value is LocaleCode {
  return value === 'zh-CN' || value === 'en-US'
}

// detectLocale 优先用用户上次的选择，其次按浏览器语言猜（中文环境之外默认英文）。
function detectLocale(): LocaleCode {
  const saved = typeof localStorage === 'undefined' ? null : localStorage.getItem(LocaleStorageKey)
  if (isLocale(saved)) return saved
  const nav = typeof navigator === 'undefined' ? '' : navigator.language.toLowerCase()
  if (!nav) return fallback
  return nav.startsWith('zh') ? 'zh-CN' : 'en-US'
}

const locale = ref<LocaleCode>(detectLocale())

// lookup 按点号逐级取词条；中途不是对象或末级不是字符串都视为缺失。
function lookup(catalog: Messages, key: string): string | undefined {
  let node: unknown = catalog
  for (const part of key.split('.')) {
    if (typeof node !== 'object' || node === null) return undefined
    node = (node as Record<string, unknown>)[part]
  }
  return typeof node === 'string' ? node : undefined
}

function interpolate(text: string, params?: Params): string {
  if (!params) return text
  return text.replace(/\{(\w+)\}/g, (whole, name: string) =>
    name in params ? String(params[name]) : whole,
  )
}

// t 读取 locale ref，所以在渲染期调用即可随语言切换自动重渲染。
export function t(key: MessageKey, params?: Params): string {
  const text = lookup(catalogs[locale.value], key) ?? lookup(catalogs[fallback], key)
  if (text === undefined) {
    // 缺词条是开发期问题：线上静默回落键名，避免控制台噪声。
    if (import.meta.env.DEV) console.warn(`[i18n] missing message: ${key}`)
    return key
  }
  return interpolate(text, params)
}

// getLocale 给非组件上下文用（格式化工具、请求头）。
export function getLocale(): LocaleCode {
  return locale.value
}

// activeLocale 是只读的当前语言，供非组件模块（路由、请求层）watch。
export const activeLocale = readonly(locale)

// setLocale 切换语言并持久化；html lang 同步更新，屏幕阅读器与拼写检查才跟得上。
export function setLocale(code: LocaleCode): void {
  locale.value = code
  if (typeof localStorage !== 'undefined') localStorage.setItem(LocaleStorageKey, code)
  if (typeof document !== 'undefined') document.documentElement.lang = code
}

// toggleLocale 在两种语言间切换。
export function toggleLocale(): void {
  setLocale(locale.value === 'zh-CN' ? 'en-US' : 'zh-CN')
}

if (typeof document !== 'undefined') document.documentElement.lang = locale.value

// useI18n 是组件里的入口；locale 只读，切换必须走 setLocale/toggleLocale。
export function useI18n() {
  return { t, locale: activeLocale, setLocale, toggleLocale, availableLocales }
}
