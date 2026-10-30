<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
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
  Monitor,
  User,
  Connection,
  Grid,
  Key,
  Setting,
  SetUp,
  Document,
} from '@element-plus/icons-vue'
import { useAppStore } from '../stores/app'
import { useUserStore } from '../stores/user'
import { api } from '../api/client'

const route = useRoute()
const router = useRouter()
const appStore = useAppStore()
const userStore = useUserStore()

const status = ref({ version: '', uptime: 0, clients: 0 })

// 菜单与 router.ts 一一对应；图标用组件引用，避免运行时按字符串解析。
const menuItems = [
  { label: '客户端', icon: Monitor, path: '/clients' },
  { label: '客户端配置', icon: SetUp, path: '/client-config' },
  { label: '用户', icon: User, path: '/users' },
  { label: '端口映射', icon: Connection, path: '/portmaps' },
  { label: '插件', icon: Grid, path: '/plugins' },
  { label: '认证密钥', icon: Key, path: '/keys' },
  { label: '服务配置', icon: Setting, path: '/service-config' },
  { label: '日志', icon: Document, path: '/logs' },
]

const activePath = computed(() => {
  const hit = menuItems.find((item) => route.path === item.path || route.path.startsWith(item.path + '/'))
  return hit?.path ?? route.path
})

const pageTitle = computed(() => (route.meta.title as string | undefined) ?? '')

const breadcrumbs = computed(() => {
  const crumbs: { title: string; path?: string }[] = [{ title: '首页', path: '/clients' }]
  const hit = menuItems.find((item) => route.path === item.path || route.path.startsWith(item.path + '/'))
  if (hit) {
    crumbs.push({ title: hit.label, path: hit.path })
  } else if (pageTitle.value) {
    crumbs.push({ title: pageTitle.value })
  }
  return crumbs
})

function formatUptime(sec: number): string {
  if (!sec) return '刚刚'
  if (sec < 60) return '刚刚'
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d) return `${d}天${h}小时`
  if (h) return `${h}小时${m}分`
  return `${m}分`
}

async function refreshStatus() {
  try {
    const s = await api.status()
    status.value = { version: s.version, uptime: s.uptime_seconds, clients: s.client_count }
  } catch {
    /* 顶栏状态失败不打扰用户 */
  }
}

function onMenuSelect(index: string) {
  if (!index) return
  void router.push(index)
}

function onTabClick(tabPath: string) {
  if (tabPath !== route.path) void router.push(tabPath)
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

// 路由变化时补齐标签页；同一路径重复进入不重复添加。
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
    <el-aside :width="appStore.sidebarWidth + 'px'" class="sidebar">
      <div class="logo">
        <div class="logo-icon">V</div>
        <div v-if="!appStore.sidebarCollapsed" class="logo-text">
          <strong>vnray</strong>
          <span>流量调度管理台</span>
        </div>
      </div>

      <el-menu
        :default-active="activePath"
        :collapse="appStore.sidebarCollapsed"
        :collapse-transition="false"
        class="sidebar-menu"
        @select="onMenuSelect"
      >
        <el-menu-item v-for="item in menuItems" :key="item.path" :index="item.path">
          <el-icon><component :is="item.icon" /></el-icon>
          <template #title>{{ item.label }}</template>
        </el-menu-item>
      </el-menu>

      <div v-if="!appStore.sidebarCollapsed" class="sidebar-footer">
        <span class="dot" :class="{ 'is-on': status.clients > 0 }" />
        <span>{{ status.clients }} 个客户端在线</span>
        <span v-if="status.version" class="ver">{{ status.version }}</span>
      </div>
    </el-aside>

    <el-container class="main-container">
      <el-header height="56px" class="header">
        <div class="header-left">
          <el-icon class="toggle-btn" @click="appStore.toggleSidebar()">
            <Fold v-if="!appStore.sidebarCollapsed" />
            <Expand v-else />
          </el-icon>

          <el-breadcrumb separator="/" class="breadcrumb">
            <el-breadcrumb-item
              v-for="(crumb, idx) in breadcrumbs"
              :key="idx"
              :to="crumb.path && idx < breadcrumbs.length - 1 ? { path: crumb.path } : undefined"
            >
              {{ crumb.title }}
            </el-breadcrumb-item>
          </el-breadcrumb>
        </div>

        <div class="header-right">
          <span class="runtime" title="服务已运行时间">已运行 {{ formatUptime(status.uptime) }}</span>

          <el-tooltip content="刷新页面" placement="bottom">
            <el-icon class="header-icon" @click="refreshPage"><Refresh /></el-icon>
          </el-tooltip>
          <el-tooltip content="全屏" placement="bottom">
            <el-icon class="header-icon" @click="toggleFullscreen"><FullScreen /></el-icon>
          </el-tooltip>
          <el-tooltip :content="appStore.darkMode ? '浅色模式' : '深色模式'" placement="bottom">
            <el-icon class="header-icon" @click="appStore.toggleDark()">
              <Sunny v-if="appStore.darkMode" />
              <Moon v-else />
            </el-icon>
          </el-tooltip>

          <el-dropdown trigger="click">
            <span class="user-info">
              <el-avatar :size="26" :icon="UserFilled" />
              <span class="username">{{ userStore.name || userStore.username }}</span>
            </span>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item :icon="SwitchButton" @click="doLogout">退出登录</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </div>
      </el-header>

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
  background: var(--el-bg-color-page);
}

.sidebar {
  background: var(--el-bg-color);
  border-right: 1px solid var(--el-border-color-lighter);
  display: flex;
  flex-direction: column;
  transition: width 0.24s ease;
  overflow: hidden;
}

.logo {
  height: 56px;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding: 0 14px;
  border-bottom: 1px solid var(--el-border-color-lighter);
  flex-shrink: 0;
}

.logo-icon {
  width: 30px;
  height: 30px;
  border-radius: 8px;
  background: linear-gradient(135deg, #409eff, #36cfc9);
  color: #fff;
  font-weight: 700;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 15px;
  box-shadow: 0 2px 6px rgba(64, 158, 255, 0.35);
  flex-shrink: 0;
}

.logo-text {
  display: flex;
  flex-direction: column;
  line-height: 1.25;
  overflow: hidden;
}

.logo-text strong {
  font-size: 15px;
  letter-spacing: 0.3px;
  color: var(--el-text-color-primary);
}

.logo-text span {
  font-size: 11px;
  color: var(--el-text-color-secondary);
  white-space: nowrap;
}

.sidebar-menu {
  flex: 1;
  border-right: none;
  padding: 8px;
  overflow-y: auto;
  overflow-x: hidden;
}

.sidebar-menu :deep(.el-menu-item) {
  height: 40px;
  line-height: 40px;
  margin-bottom: 2px;
  border-radius: 8px;
  font-size: 13.5px;
}

.sidebar-menu :deep(.el-menu-item.is-active) {
  background: var(--el-color-primary-light-9);
  font-weight: 600;
}

.sidebar-footer {
  height: 38px;
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 0 12px;
  font-size: 11.5px;
  color: var(--el-text-color-secondary);
  border-top: 1px solid var(--el-border-color-lighter);
  flex-shrink: 0;
}

.sidebar-footer .ver {
  margin-left: auto;
  font-family: var(--mono);
}

.dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--el-border-color-darker);
}

.dot.is-on {
  background: var(--el-color-success);
  box-shadow: 0 0 0 3px rgba(103, 194, 58, 0.18);
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
  gap: 14px;
  min-width: 0;
}

.toggle-btn {
  font-size: 18px;
  cursor: pointer;
  color: var(--el-text-color-regular);
  transition: color 0.2s;
}

.toggle-btn:hover {
  color: var(--el-color-primary);
}

.breadcrumb {
  font-size: 13px;
}

.header-right {
  display: flex;
  align-items: center;
  gap: 14px;
}

.runtime {
  font-size: 12px;
  color: var(--el-text-color-secondary);
  padding: 3px 10px;
  border-radius: 999px;
  background: var(--el-fill-color-light);
}

.header-icon {
  font-size: 17px;
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
  outline: none;
}

.username {
  font-size: 13px;
  max-width: 10rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.tabs-bar {
  padding: 8px 12px 0;
  background: var(--el-bg-color);
  border-bottom: 1px solid var(--el-border-color-lighter);
  flex-shrink: 0;
}

.page-tabs :deep(.el-tabs__header) {
  margin-bottom: 0;
  border-bottom: none;
}

.page-tabs :deep(.el-tabs__nav) {
  border: none;
  gap: 6px;
}

.page-tabs :deep(.el-tabs__item) {
  height: 30px;
  line-height: 30px;
  font-size: 12.5px;
  padding: 0 12px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px 6px 0 0;
  background: var(--el-fill-color-lighter);
  color: var(--el-text-color-regular);
}

.page-tabs :deep(.el-tabs__item.is-active) {
  background: var(--el-color-primary-light-9);
  border-color: var(--el-color-primary-light-7);
  color: var(--el-color-primary);
  font-weight: 600;
}

.main-content {
  flex: 1;
  overflow-y: auto;
  background: var(--el-bg-color-page);
  padding: 16px 20px 24px;
}

.fade-enter-active,
.fade-leave-active {
  transition: opacity 0.16s ease;
}

.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}
</style>
