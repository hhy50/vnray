<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { useUserStore } from './stores/user'

const route = useRoute()
const userStore = useUserStore()

// 登录页、404 不需要等待 session 加载
const isStandalonePage = computed(() => {
  return route.name === 'login' || route.name === 'notfound'
})
</script>

<template>
  <!-- 独立页面：登录 / 404 -->
  <router-view v-if="isStandalonePage" />

  <!-- 加载中 -->
  <div v-else-if="!userStore.loaded" class="app-loading">
    <el-icon class="is-loading" :size="32"><Loading /></el-icon>
    <p>加载中...</p>
  </div>

  <!-- 布局由路由树决定：/ 下的子路由以 AdminLayout 为父组件，这里只渲染路由出口 -->
  <router-view v-else />
</template>

<style scoped>
.app-loading {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  height: 100vh;
  color: var(--el-text-color-secondary);
  gap: 12px;
}
</style>
