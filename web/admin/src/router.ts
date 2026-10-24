import { createRouter, createWebHistory } from 'vue-router'
import type { RouteRecordRaw } from 'vue-router'
import { useUserStore } from './stores/user'

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('./views/Login.vue'),
    meta: { title: '登录', public: true },
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
        meta: { title: '客户端' },
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
        meta: { title: '用户' },
      },
      {
        path: 'portmaps',
        name: 'portmaps',
        component: () => import('./views/PortMaps.vue'),
        meta: { title: '端口映射' },
      },
      {
        path: 'plugins',
        name: 'plugins',
        component: () => import('./views/Plugins.vue'),
        meta: { title: '插件' },
      },
      {
        path: 'keys',
        name: 'keys',
        component: () => import('./views/Keys.vue'),
        meta: { title: '认证密钥' },
      },
      {
        path: 'service-config',
        name: 'service-config',
        component: () => import('./views/ServiceConfig.vue'),
        meta: { title: '服务配置' },
      },
      {
        path: 'logs',
        name: 'logs',
        component: () => import('./views/Logs.vue'),
        meta: { title: '日志' },
      },
    ],
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'notfound',
    component: () => import('./views/NotFound.vue'),
    meta: { title: '页面不存在' },
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

router.afterEach((to) => {
  const title = (to.meta.title as string | undefined) ?? ''
  document.title = title ? `${title} · vnray 管理台` : 'vnray 管理台'
})
