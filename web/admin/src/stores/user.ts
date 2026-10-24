import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import type { SessionUser } from '../api/types'
import { api } from '../api/client'

export const useUserStore = defineStore('user', () => {
  const loaded = ref(false)
  const error = ref('')
  const user = ref<SessionUser | null>(null)
  const csrf = ref('')

  const username = computed(() => user.value?.username || '')
  const name = computed(() => user.value?.name || '')

  async function loadSession() {
    const s = await api.session()
    user.value = s.user
    csrf.value = s.csrf ?? ''
    loaded.value = true
    error.value = ''
  }

  async function login(username: string, password: string) {
    const res = await api.login(username, password)
    csrf.value = res.csrf
    try {
      await loadSession()
    } catch (e) {
      // 登录成功但 session 加载失败，清理状态并抛出
      user.value = null
      csrf.value = ''
      loaded.value = false
      throw e
    }
  }

  async function logout() {
    try {
      await api.logout()
    } catch {
      /* 忽略登出错误 */
    }
    reset()
  }

  function reset() {
    user.value = null
    csrf.value = ''
    loaded.value = false
    error.value = ''
  }

  return {
    loaded,
    error,
    user,
    csrf,
    username,
    name,
    loadSession,
    login,
    logout,
    reset,
  }
})
