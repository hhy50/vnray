<script setup lang="ts">
// 404：独立布局，给出返回上一页与回首页两个出口。
import { useRouter, useRoute } from 'vue-router'
import { computed } from 'vue'
import { HomeFilled, ArrowLeft, Search } from '@element-plus/icons-vue'
import { useI18n } from '../i18n'

const router = useRouter()
const route = useRoute()
const { t } = useI18n()

const currentPath = computed(() => route.fullPath)

function goBack() {
  router.back()
}

function goHome() {
  router.push('/')
}
</script>

<template>
  <div class="notfound-page">
    <div class="notfound-card">
      <div class="icon-wrapper">
        <el-icon :size="80" color="#909399"><Search /></el-icon>
      </div>
      <h1 class="title">404</h1>
      <h2 class="subtitle">{{ t('notFound.subtitle') }}</h2>
      <p class="desc">
        {{ t('notFound.desc') }}
      </p>
      <div class="path-display">
        <span class="path-label">{{ t('notFound.currentPath') }}</span>
        <span class="path-value mono">{{ currentPath }}</span>
      </div>
      <div class="actions">
        <el-button :icon="ArrowLeft" @click="goBack">{{ t('notFound.back') }}</el-button>
        <el-button type="primary" :icon="HomeFilled" @click="goHome">{{ t('notFound.home') }}</el-button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.notfound-page {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
  padding: 40px 20px;
  background: var(--el-bg-color-page);
}

.notfound-card {
  width: 100%;
  max-width: 480px;
  text-align: center;
  padding: 48px 40px;
  background: var(--el-bg-color);
  border-radius: 12px;
  box-shadow: 0 4px 24px rgba(0, 0, 0, 0.06);
}

.icon-wrapper {
  margin-bottom: 16px;
  opacity: 0.6;
}

.title {
  font-size: 72px;
  font-weight: 700;
  margin: 0 0 8px;
  background: linear-gradient(135deg, #606266, #909399);
  -webkit-background-clip: text;
  -webkit-text-fill-color: transparent;
  background-clip: text;
  line-height: 1;
}

.subtitle {
  font-size: 18px;
  font-weight: 500;
  color: var(--el-text-color-primary);
  margin: 0 0 12px;
}

.desc {
  font-size: 14px;
  color: var(--el-text-color-secondary);
  margin: 0 0 20px;
}

.path-display {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 8px 16px;
  background: var(--el-fill-color-light);
  border-radius: 8px;
  margin-bottom: 28px;
  font-size: 13px;
}

.path-label {
  color: var(--el-text-color-secondary);
}

.path-value {
  color: var(--el-text-color-primary);
  font-family: Menlo, Monaco, Consolas, monospace;
  font-size: 12px;
  max-width: 300px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.actions {
  display: flex;
  justify-content: center;
  gap: 12px;
}
</style>
