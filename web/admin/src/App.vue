<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import en from 'element-plus/es/locale/lang/en'
import { useUserStore } from './stores/user'
import { useI18n } from './i18n'

const route = useRoute()
const userStore = useUserStore()
const { t, locale } = useI18n()

const isStandalonePage = computed(() => {
  return route.name === 'login' || route.name === 'notfound'
})

// Element Plus 的内建文案（分页、日期面板、空状态等）跟随同一套语言。
const elementLocale = computed(() => (locale.value === 'zh-CN' ? zhCn : en))
</script>

<template>
  <el-config-provider :locale="elementLocale">
    <router-view v-if="isStandalonePage" />

    <div v-else-if="!userStore.loaded" class="app-loading">
      <el-icon class="is-loading" :size="32"><Loading /></el-icon>
      <p>{{ t('common.loading') }}</p>
    </div>

    <router-view v-else />
  </el-config-provider>
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
