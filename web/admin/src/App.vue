<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { useUserStore } from './stores/user'

const route = useRoute()
const userStore = useUserStore()

const isStandalonePage = computed(() => {
  return route.name === 'login' || route.name === 'notfound'
})
</script>

<template>
  <router-view v-if="isStandalonePage" />

  <div v-else-if="!userStore.loaded" class="app-loading">
    <el-icon class="is-loading" :size="32"><Loading /></el-icon>
    <p>加载中...</p>
  </div>

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
