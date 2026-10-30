<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Search, Refresh, CopyDocument, Delete } from '@element-plus/icons-vue'
import { api } from '../api/client'
import type { Logs } from '../api/types'
import { copyText } from '../lib/format'

const lines = ref<string[]>([])
const path = ref('')
const truncated = ref(false)
const q = ref('')
const loadError = ref('')
const live = ref(true)
const connected = ref(false)
let source: EventSource | undefined
let cursor = ''
let unavailable = false
let discardSnapshot = false

const visibleLines = computed(() => {
  const filter = q.value.trim()
  return filter ? lines.value.filter((line) => line.includes(filter)) : lines.value
})
const text = computed(() => visibleLines.value.join('\n'))
const status = computed(() => !live.value ? '已暂停' : connected.value ? '实时接收中' : loadError.value ? '连接异常' : '连接中')

function stop() {
  source?.close()
  source = undefined
  connected.value = false
}

function start() {
  stop()
  if (!live.value || unavailable || document.hidden) return
  const params = new URLSearchParams()
  if (cursor) params.set('cursor', cursor)
  const stream = new EventSource(`/admin/ops/api/logs/stream?${params}`)
  source = stream
  stream.onopen = () => {
    if (source !== stream) return
    connected.value = true
    loadError.value = ''
  }
  stream.addEventListener('logs', (event) => {
    if (source !== stream) return
    const message = event as MessageEvent<string>
    const data = JSON.parse(message.data) as Logs
    cursor = message.lastEventId
    path.value = data.path
    loadError.value = ''
    if (discardSnapshot) {
      discardSnapshot = false
      return
    }
    const next = [...lines.value, ...data.lines]
    let size = 0
    let keepFrom = next.length
    while (keepFrom > 0 && next.length - keepFrom < 1000) {
      const length = next[keepFrom - 1].length + 1
      if (size + length > 256 * 1024 && keepFrom < next.length) break
      size += length
      keepFrom--
    }
    lines.value = next.slice(keepFrom)
    truncated.value ||= data.truncated || keepFrom > 0
  })
  stream.addEventListener('warning', (event) => {
    if (source === stream) loadError.value = JSON.parse((event as MessageEvent<string>).data)
  })
  for (const name of ['unavailable', 'unauthorized']) {
    stream.addEventListener(name, (event) => {
      if (source !== stream) return
      loadError.value = JSON.parse((event as MessageEvent<string>).data)
      unavailable = true
      stop()
    })
  }
  stream.onerror = () => {
    if (source !== stream) return
    connected.value = false
    loadError.value = '日志连接中断，正在自动重连'
    if (stream.readyState === EventSource.CLOSED) {
      loadError.value = '日志连接已关闭，请重新连接或登录'
      void api.session().catch(() => {
        if (source === stream) loadError.value = '登录已失效，请重新登录'
      })
    }
  }
}

function reconnect() {
  unavailable = false
  live.value = true
  loadError.value = ''
  start()
}

function clearPage() {
  // 保留流和游标，后续追加与重连都不会重新载入已清空的历史。
  lines.value = []
  truncated.value = false
  if (!cursor) discardSnapshot = true
}

function onVisibility() {
  if (document.hidden) stop()
  else start()
}

async function copyAll() {
  if (await copyText(text.value)) ElMessage.success('日志已复制')
  else ElMessage.error('复制失败')
}

onMounted(() => {
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
          placeholder="过滤当前页面日志"
          clearable
          :prefix-icon="Search"
          style="width: 17rem"
        />
        <el-switch v-model="live" size="small" active-text="实时日志" @change="start" />
        <span class="faint">{{ status }}</span>
        <div class="spacer" />
        <span class="faint log-summary" :title="path">
          {{ visibleLines.length }} 行 · {{ path || '等待日志' }}
          <template v-if="truncated"> · 已保留最近日志</template>
        </span>
        <el-button :icon="CopyDocument" :disabled="!text" @click="copyAll">复制</el-button>
        <el-tooltip content="只清空当前页面，后续日志继续显示">
          <el-button :icon="Delete" @click="clearPage">清空页面</el-button>
        </el-tooltip>
        <el-button :icon="Refresh" @click="reconnect">重新连接</el-button>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="warning" :closable="false" show-icon />
    <pre class="log-view is-plain log-body">{{ text || (q.trim() ? '（没有匹配的日志）' : '（等待新日志）') }}</pre>
  </el-card>
</template>

<style scoped>
.log-card :deep(.el-card__body) {
  padding: 12px 16px 16px;
}

.log-summary {
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.log-body {
  max-height: calc(100vh - 260px);
  min-height: 320px;
  font-size: 11.5px;
  line-height: 1.65;
}
</style>
