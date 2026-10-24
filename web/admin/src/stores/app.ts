import { defineStore } from 'pinia'
import { ref, computed, watch } from 'vue'

export interface TabItem {
  path: string
  title: string
  closable: boolean
}

const THEME_KEY = 'vnray.theme'

// 主题存在 localStorage：刷新后 element 变量与自定义样式同时生效，不会闪回浅色。
const storedDark = typeof localStorage !== 'undefined' && localStorage.getItem(THEME_KEY) === 'dark'

export const useAppStore = defineStore('app', () => {
  const sidebarCollapsed = ref(false)
  const tabs = ref<TabItem[]>([{ path: '/clients', title: '客户端', closable: false }])
  const activeTab = ref('/clients')
  const darkMode = ref(storedDark)

  const sidebarWidth = computed(() => (sidebarCollapsed.value ? 64 : 220))

  watch(
    darkMode,
    (dark) => {
      document.documentElement.classList.toggle('dark', dark)
      localStorage.setItem(THEME_KEY, dark ? 'dark' : 'light')
    },
    { immediate: true },
  )

  function toggleSidebar() {
    sidebarCollapsed.value = !sidebarCollapsed.value
  }

  function addTab(tab: TabItem) {
    if (!tabs.value.find((t) => t.path === tab.path)) {
      tabs.value.push(tab)
    }
    activeTab.value = tab.path
  }

  function closeTab(path: string) {
    const idx = tabs.value.findIndex((t) => t.path === path)
    if (idx === -1) return
    const tab = tabs.value[idx]
    if (!tab.closable) return
    tabs.value.splice(idx, 1)
    if (activeTab.value === path) {
      const next = tabs.value[idx] || tabs.value[idx - 1]
      if (next) {
        activeTab.value = next.path
      }
    }
  }

  function setActiveTab(path: string) {
    activeTab.value = path
  }

  function toggleDark() {
    darkMode.value = !darkMode.value
  }

  return {
    sidebarCollapsed,
    sidebarWidth,
    tabs,
    activeTab,
    darkMode,
    toggleSidebar,
    addTab,
    closeTab,
    setActiveTab,
    toggleDark,
  }
})
