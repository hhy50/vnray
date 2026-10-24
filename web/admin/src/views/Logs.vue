<script setup lang="ts">
// 日志：读服务端日志文件末尾若干行，支持关键字过滤与自动刷新。
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Search, Refresh, CopyDocument } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import { copyText } from '../lib/format'

const lines = ref<string[]>([])
const path = ref('')
const truncated = ref(false)
const q = ref('')
const loadError = ref('')
const auto = ref(true)
const loading = ref(false)
let timer: number | undefined

const text = computed(() => lines.value.join('\n'))

async function load() {
  loading.value = true
  try {
    const data = await api.logs({ q: q.value.trim(), lines: 300 })
    lines.value = data.lines
    path.value = data.path
    truncated.value = data.truncated
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载失败'
  } finally {
    loading.value = false
  }
}

// 轮询只在标签页可见且开启自动刷新时跑，否则纯属浪费请求。
function start() {
  stop()
  timer = window.setInterval(() => {
    if (!document.hidden && auto.value) void load()
  }, 3000)
}

function stop() {
  if (timer) window.clearInterval(timer)
  timer = undefined
}

function onVisibility() {
  if (!document.hidden) void load()
}

async function copyAll() {
  if (await copyText(text.value)) ElMessage.success('日志已复制')
  else ElMessage.error('复制失败')
}

onMounted(() => {
  void load()
  start()
  document.addEventListener('visibilitychange', onVisibility)
})

onBeforeUnmount(() => {
  stop()
  document.removeEventListener('visibilitychange', onVisibility)
})
</script>

<template>
  <el-card shadow="never" class="stack log-card">
    <template #header>
      <div class="toolbar">
        <el-input
          v-model="q"
          placeholder="过滤关键字"
          clearable
          :prefix-icon="Search"
          style="width: 17rem"
          @keyup.enter="load"
        />
        <el-button :loading="loading" @click="load">过滤</el-button>
        <el-switch v-model="auto" size="small" active-text="自动刷新 3s" />
        <div class="spacer" />
        <span class="faint">
          {{ lines.length }} 行 · {{ path || '未写文件' }}
          <template v-if="truncated"> · 仅末尾 256 KiB</template>
        </span>
        <el-button :icon="CopyDocument" @click="copyAll">复制</el-button>
        <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />
    <pre v-else class="log-view is-plain log-body">{{ text || '（没有匹配的行）' }}</pre>
  </el-card>
</template>

<style scoped>
.log-card :deep(.el-card__body) {
  padding: 12px 16px 16px;
}

.log-body {
  max-height: calc(100vh - 260px);
  min-height: 320px;
  font-size: 11.5px;
  line-height: 1.65;
}
</style>
