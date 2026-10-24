<script setup lang="ts">
import { computed, onMounted, onUnmounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  Fold,
  Expand,
  FullScreen,
  SwitchButton,
  Moon,
  Sunny,
  UserFilled,
  Refresh,
} from '@element-plus/icons-vue'
import { useAppStore } from '../stores/app'
import { useUserStore } from '../stores/user'
import { api } from '../api/client'
import { ref } from 'vue'

const route = useRoute()
const router = useRouter()
const appStore = useAppStore()
const userStore = useUserStore()

const status = ref<{ version: string; uptime: number }>({ version: '', uptime: 0 })

// 侧边栏菜单：本地静态定义
const menuItems = [
  { key: 'clients', label: '客户端', icon: 'Monitor', path: '/clients' },
  { key: 'users', label: '用户', icon: 'User', path: '/users' },
  { key: 'portmaps', label: '端口映射', icon: 'Connection', path: '/portmaps' },
  { key: 'plugins', label: '插件', icon: 'Grid', path: '/plugins' },
  { key: 'keys', label: '认证密钥', icon: 'Key', path: '/keys' },
  { key: 'service-config', label: '服务配置', icon: 'Setting', path: '/service-config' },
  { key: 'logs', label: '日志', icon: 'Document', path: '/logs' },
]

const activePath = computed(() => {
  const hit = menuItems.find((item) => route.path === item.path || route.path.startsWith(item.path + '/'))
  return hit?.path ?? route.path
})

const pageTitle = computed(() => {
  return (route.meta.title as string | undefined) ?? ''
})

// 面包屑
const breadcrumbs = computed(() => {
  const crumbs: { title: string; path?: string }[] = [{ title: '首页', path: '/' }]
  const hit = menuItems.find((item) => route.path === item.path || route.path.startsWith(item.path + '/'))
  if (hit) {
    crumbs.push({ title: hit.label, path: hit.path })
  } else if (pageTitle.value) {
    crumbs.push({ title: pageTitle.value })
  }
  return crumbs
})

async function refreshStatus() {
  try {
    const s = await api.status()
    status.value = { version: s.version, uptime: s.uptime_seconds }
  } catch {
    /* 顶栏状态失败不打扰用户 */
  }
}

function onMenuSelect(index: string) {
  if (!index) return
  void router.push(index)
}

function onTabClick(tabPath: string) {
  void router.push(tabPath)
}

function onTabClose(tabPath: string) {
  appStore.closeTab(tabPath)
  if (appStore.activeTab !== route.path) {
    void router.push(appStore.activeTab)
  }
}

function refreshPage() {
  window.location.reload()
}

function toggleFullscreen() {
  if (!document.fullscreenElement) {
    void document.documentElement.requestFullscreen()
  } else {
    void document.exitFullscreen()
  }
}

async function doLogout() {
  await userStore.logout()
  router.replace('/login')
}

// 路由变化时添加标签页
watch(
  () => route.path,
  () => {
    if (route.name === 'login') return
    const title = (route.meta.title as string) || route.path
    appStore.addTab({ path: route.path, title, closable: route.path !== '/clients' })
  },
  { immediate: true },
)

// 状态轮询：登出/切到独立页时组件会卸载，必须清掉定时器，
// 否则旧 interval 会一直轮询并在再次登录时叠加。
let statusTimer: ReturnType<typeof setInterval> | null = null

onMounted(() => {
  void refreshStatus()
  statusTimer = setInterval(() => {
    if (!document.hidden) void refreshStatus()
  }, 30000)
})

onUnmounted(() => {
  if (statusTimer !== null) {
    clearInterval(statusTimer)
    statusTimer = null
  }
})
</script>

<template>
  <el-container class="admin-layout">
    <!-- 侧边栏 -->
    <el-aside :width="appStore.sidebarWidth + 'px'" class="sidebar">
      <div class="logo">
        <div class="logo-icon">V</div>
        <span v-if="!appStore.sidebarCollapsed" class="logo-text">vnray admin</span>
      </div>

      <el-menu
        :default-active="activePath"
        :collapse="appStore.sidebarCollapsed"
        class="sidebar-menu"
        @select="onMenuSelect"
      >
        <el-menu-item v-for="item in menuItems" :key="item.key" :index="item.path">
          <el-icon><component :is="item.icon" /></el-icon>
          <template #title>{{ item.label }}</template>
        </el-menu-item>
      </el-menu>

      <div v-if="!appStore.sidebarCollapsed" class="sidebar-footer">
        <span v-if="status.version">v{{ status.version }}</span>
        <span v-else>vnray 管理台</span>
      </div>
    </el-aside>

    <el-container class="main-container">
      <!-- 顶栏 -->
      <el-header height="50px" class="header">
        <div class="header-left">
          <el-icon class="toggle-btn" @click="appStore.toggleSidebar()">
            <Fold v-if="!appStore.sidebarCollapsed" />
            <Expand v-else />
          </el-icon>

          <el-breadcrumb separator="/" class="breadcrumb">
            <el-breadcrumb-item
              v-for="(crumb, idx) in breadcrumbs"
              :key="idx"
              :to="crumb.path ? { path: crumb.path } : undefined"
            >
              {{ crumb.title }}
            </el-breadcrumb-item>
          </el-breadcrumb>
        </div>

        <div class="header-right">
          <el-tooltip content="刷新">
            <el-icon class="header-icon" @click="refreshPage">
              <Refresh />
            </el-icon>
          </el-tooltip>
          <el-tooltip content="全屏">
            <el-icon class="header-icon" @click="toggleFullscreen">
              <FullScreen />
            </el-icon>
          </el-tooltip>
          <el-tooltip :content="appStore.darkMode ? '浅色模式' : '深色模式'">
            <el-icon class="header-icon" @click="appStore.toggleDark()">
              <Moon v-if="!appStore.darkMode" />
              <Sunny v-else />
            </el-icon>
          </el-tooltip>

          <el-dropdown trigger="click">
            <span class="user-info">
              <el-avatar :size="28" :icon="UserFilled" />
              <span class="username">{{ userStore.name }}</span>
            </span>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item :icon="SwitchButton" @click="doLogout">
                  退出登录
                </el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </div>
      </el-header>

      <!-- 标签页 -->
      <div class="tabs-bar">
        <el-tabs
          v-model="appStore.activeTab"
          type="card"
          class="page-tabs"
          @tab-click="(tab: any) => onTabClick(tab.props.name)"
          @tab-remove="onTabClose"
        >
          <el-tab-pane
            v-for="tab in appStore.tabs"
            :key="tab.path"
            :label="tab.title"
            :name="tab.path"
            :closable="tab.closable"
          />
        </el-tabs>
      </div>

      <!-- 主内容 -->
      <el-main class="main-content">
        <router-view v-slot="{ Component }">
          <transition name="fade" mode="out-in">
            <component :is="Component" />
          </transition>
        </router-view>
      </el-main>
    </el-container>
  </el-container>
</template>

<style scoped>
.admin-layout {
  height: 100vh;
  overflow: hidden;
}

.sidebar {
  background: var(--el-bg-color-page);
  border-right: 1px solid var(--el-border-color-lighter);
  display: flex;
  flex-direction: column;
  transition: width 0.3s;
  overflow: hidden;
}

.logo {
  height: 50px;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  border-bottom: 1px solid var(--el-border-color-lighter);
  flex-shrink: 0;
}

.logo-icon {
  width: 32px;
  height: 32px;
  border-radius: 6px;
  background: linear-gradient(135deg, #409eff, #67c23a);
  color: white;
  font-weight: bold;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 16px;
}

.logo-text {
  font-size: 16px;
  font-weight: 600;
  color: var(--el-text-color-primary);
}

.sidebar-menu {
  flex: 1;
  border-right: none;
  overflow-y: auto;
  overflow-x: hidden;
}

.sidebar-footer {
  height: 36px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 12px;
  color: var(--el-text-color-secondary);
  border-top: 1px solid var(--el-border-color-lighter);
  flex-shrink: 0;
}

.main-container {
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

.header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 16px;
  background: var(--el-bg-color);
  border-bottom: 1px solid var(--el-border-color-lighter);
  flex-shrink: 0;
}

.header-left {
  display: flex;
  align-items: center;
  gap: 16px;
}

.toggle-btn {
  font-size: 20px;
  cursor: pointer;
  color: var(--el-text-color-regular);
  transition: color 0.2s;
}
.toggle-btn:hover {
  color: var(--el-color-primary);
}

.breadcrumb {
  font-size: 14px;
}

.header-right {
  display: flex;
  align-items: center;
  gap: 16px;
}

.header-icon {
  font-size: 18px;
  cursor: pointer;
  color: var(--el-text-color-regular);
  transition: color 0.2s;
}
.header-icon:hover {
  color: var(--el-color-primary);
}

.user-info {
  display: flex;
  align-items: center;
  gap: 8px;
  cursor: pointer;
  color: var(--el-text-color-regular);
}

.username {
  font-size: 14px;
}

.tabs-bar {
  padding: 6px 12px 0;
  background: var(--el-bg-color);
  border-bottom: 1px solid var(--el-border-color-lighter);
  flex-shrink: 0;
}

.page-tabs {
  margin-bottom: 0;
}
.page-tabs :deep(.el-tabs__header) {
  margin-bottom: 0;
}
.page-tabs :deep(.el-tabs__nav) {
  height: 32px;
}
.page-tabs :deep(.el-tabs__item) {
  height: 32px;
  line-height: 32px;
  font-size: 13px;
}

.main-content {
  flex: 1;
  overflow-y: auto;
  background: var(--el-bg-color-page);
  padding: 16px;
}

.fade-enter-active,
.fade-leave-active {
  transition: opacity 0.2s;
}
.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}
</style>
