import { watch } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import type { RouteRecordRaw } from 'vue-router'
import { useUserStore } from './stores/user'
import { activeLocale, t } from './i18n'
import type { MessageKey } from './i18n'

// meta.titleKey 存词条键而不是译文：标题栏与标签页随语言切换一起更新。
declare module 'vue-router' {
  interface RouteMeta {
    titleKey?: MessageKey
    public?: boolean
  }
}

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('./views/Login.vue'),
    meta: { titleKey: 'nav.login', public: true },
  },
  {
    path: '/',
    redirect: '/clients',
  },
  {
    path: '/',
    component: () => import('./layouts/AdminLayout.vue'),
    children: [
      {
        path: 'clients',
        name: 'clients',
        component: () => import('./views/Clients.vue'),
        meta: { titleKey: 'nav.clients' },
      },
      {
        // 旧深链兜底：客户端配置已改成列表里的弹窗，不再有独立页面。
        path: 'clients/:name',
        redirect: { name: 'clients' },
      },
      {
        path: 'users',
        name: 'users',
        component: () => import('./views/Users.vue'),
        meta: { titleKey: 'nav.users' },
      },
      {
        path: 'portmaps',
        name: 'portmaps',
        component: () => import('./views/PortMaps.vue'),
        meta: { titleKey: 'nav.portmaps' },
      },
      {
        // 出站规则改成用户页的「规则列表」弹窗，旧深链兜底到用户页。
        path: 'rules',
        redirect: { name: 'users' },
      },
      {
        path: 'plugins',
        name: 'plugins',
        component: () => import('./views/Plugins.vue'),
        meta: { titleKey: 'nav.plugins' },
      },
      {
        path: 'keys',
        name: 'keys',
        component: () => import('./views/Keys.vue'),
        meta: { titleKey: 'nav.keys' },
      },
      {
        path: 'service-config',
        name: 'service-config',
        component: () => import('./views/ServiceConfig.vue'),
        meta: { titleKey: 'nav.serviceConfig' },
      },
      {
        path: 'client-config',
        name: 'client-config',
        component: () => import('./views/ClientConfig.vue'),
        meta: { titleKey: 'nav.clientConfig' },
      },
      {
        path: 'logs',
        name: 'logs',
        component: () => import('./views/Logs.vue'),
        meta: { titleKey: 'nav.logs' },
      },
    ],
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'notfound',
    component: () => import('./views/NotFound.vue'),
    meta: { titleKey: 'nav.notFound' },
  },
]

export const router = createRouter({
  history: createWebHistory('/admin/ui/'),
  routes,
})

// 全局守卫：未登录跳登录页
router.beforeEach(async (to) => {
  if (to.meta.public) return true

  const userStore = useUserStore()

  // 首次进入时加载 session
  if (!userStore.loaded) {
    try {
      await userStore.loadSession()
    } catch {
      // 加载失败（401 等），重定向到登录页
      return { name: 'login', query: { redirect: to.fullPath } }
    }
  }

  // 加载完了但没有用户信息 → 未登录
  if (!userStore.user) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }

  return true
})

function applyTitle(titleKey: MessageKey | undefined): void {
  document.title = titleKey ? `${t(titleKey)} · ${t('app.title')}` : t('app.title')
}

router.afterEach((to) => {
  applyTitle(to.meta.titleKey)
})

// 切语言后标题栏不会自己重算，这里跟着当前路由刷新一次。
watch(activeLocale, () => {
  applyTitle(router.currentRoute.value.meta.titleKey)
})
