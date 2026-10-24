import { defineStore } from 'pinia'
import { ref, computed } from 'vue'

export interface TabItem {
  path: string
  title: string
  closable: boolean
}

export const useAppStore = defineStore('app', () => {
  const sidebarCollapsed = ref(false)
  const tabs = ref<TabItem[]>([
    { path: '/clients', title: '客户端', closable: false },
  ])
  const activeTab = ref('/clients')
  const darkMode = ref(false)

  const sidebarWidth = computed(() => (sidebarCollapsed.value ? 64 : 220))

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
    if (darkMode.value) {
      document.documentElement.classList.add('dark')
    } else {
      document.documentElement.classList.remove('dark')
    }
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
