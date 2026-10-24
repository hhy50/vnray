<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { Search } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'

const lines = ref<string[]>([])
const path = ref('')
const truncated = ref(false)
const q = ref('')
const loadError = ref('')
const auto = ref(true)
const loading = ref(false)
let timer: number | undefined

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
  <el-card shadow="never" class="stack">
    <template #header>
      <div class="toolbar">
        <el-input
          v-model="q"
          placeholder="过滤关键字"
          clearable
          :prefix-icon="Search"
          style="width: 18rem"
          @keyup.enter="load"
        />
        <el-button :loading="loading" @click="load">过滤</el-button>
        <el-checkbox v-model="auto">自动刷新（3 秒）</el-checkbox>
        <div class="spacer" />
        <small class="faint">
          来源 {{ path || '（未写文件）' }}<template v-if="truncated"> · 仅显示文件末尾约 256 KiB</template> · {{ lines.length }} 行
        </small>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />
    <pre v-else class="log-view">{{ lines.join('\n') }}</pre>
  </el-card>
</template>
