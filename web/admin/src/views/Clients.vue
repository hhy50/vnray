<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Refresh, Search } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { Client } from '../api/types'
import { bytes, clock } from '../lib/format'

const router = useRouter()

const clients = ref<Client[]>([])
const platforms = ref<string[]>([])
const loading = ref(false)
const loadError = ref('')
const q = ref('')
const platform = ref('')
const busyId = ref(0)

let timer: number | undefined

const filtered = computed(() => {
  const needle = q.value.trim().toLowerCase()
  return clients.value.filter((c) => {
    if (platform.value && c.os !== platform.value) return false
    if (!needle) return true
    const hay = [c.name, c.remote, c.hostname, c.version, c.os, c.arch]
      .concat(c.interfaces.flatMap((i) => [i.name, i.mac, ...i.addrs]))
      .join('\u0000')
      .toLowerCase()
    return hay.includes(needle)
  })
})

const onlineCount = computed(() => clients.value.filter((c) => c.online).length)

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    const data = await api.clients()
    clients.value = data.clients
    platforms.value = data.platforms
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载失败'
  } finally {
    loading.value = false
  }
}

async function kick(client: Client) {
  try {
    await ElMessageBox.confirm(
      `断开 ${client.name || '该客户端'} 的会话？客户端若开了自动重连会重新接入。`,
      '断开连接',
      { type: 'warning', confirmButtonText: '断开', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  busyId.value = client.id
  try {
    await api.clientKick(client.id)
    ElMessage.success('已断开，等待客户端重连')
    await load(true)
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '断开失败')
  } finally {
    busyId.value = 0
  }
}

// 轮询只在页面可见时跑；标签页切走就停，回来立刻补一次。
function startPolling() {
  stopPolling()
  timer = window.setInterval(() => {
    if (!document.hidden) void load(true)
  }, 5000)
}

function stopPolling() {
  if (timer) window.clearInterval(timer)
  timer = undefined
}

function onVisibility() {
  if (!document.hidden) void load(true)
}

onMounted(() => {
  void load()
  startPolling()
  document.addEventListener('visibilitychange', onVisibility)
})

onBeforeUnmount(() => {
  stopPolling()
  document.removeEventListener('visibilitychange', onVisibility)
})

function ifaceText(c: Client): string {
  const first = c.interfaces[0]
  if (!first) return '—'
  return `${first.name} ${first.addrs[0] ?? ''}`.trim()
}

function ifaceDetail(c: Client): string {
  return c.interfaces
    .map((i) => `${i.name}${i.mac ? ' [' + i.mac + ']' : ''} ${i.addrs.join(', ')}`.trim())
    .join('\n')
}
</script>

<template>
  <el-card shadow="never" class="stack">
    <template #header>
      <div class="toolbar">
        <strong>在线 {{ onlineCount }}</strong>
        <span class="faint">共 {{ clients.length }} 个（含已注册离线）</span>
        <div class="spacer" />
        <el-input
          v-model="q"
          placeholder="搜索名称 / 地址 / 主机名 / 版本 / 网卡"
          clearable
          :prefix-icon="Search"
          style="width: 20rem"
        />
        <el-select v-model="platform" placeholder="全部平台" clearable style="width: 9rem">
          <el-option label="全部平台" value="" />
          <el-option v-for="p in platforms" :key="p" :label="p" :value="p" />
        </el-select>
        <el-button :icon="Refresh" :loading="loading" @click="load()">刷新</el-button>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-table
      v-else
      v-loading="loading"
      :data="filtered"
      row-key="id"
      size="small"
      empty-text="没有符合条件的客户端。客户端接入后会出现在这里，每 5 秒自动刷新。"
    >
      <el-table-column label="状态" width="86">
        <template #default="{ row }">
          <el-tag v-if="row.local" size="small" type="info">本机</el-tag>
          <el-tag v-else-if="row.online" size="small" type="success">在线</el-tag>
          <el-tag v-else size="small" type="info" effect="plain">离线</el-tag>
        </template>
      </el-table-column>

      <el-table-column label="ID" width="70">
        <template #default="{ row }">
          <span class="faint">{{ row.online ? row.id : '—' }}</span>
        </template>
      </el-table-column>

      <el-table-column label="客户端" min-width="170">
        <template #default="{ row }">
          <div>{{ row.name || '—' }}</div>
          <div v-if="row.version" class="faint">
            {{ row.version }} · {{ row.os }}/{{ row.arch }}
            <template v-if="row.hostname"> · {{ row.hostname }}</template>
          </div>
        </template>
      </el-table-column>

      <el-table-column label="地址" min-width="140">
        <template #default="{ row }"><span class="mono">{{ row.remote || '—' }}</span></template>
      </el-table-column>

      <el-table-column label="网卡" min-width="140" show-overflow-tooltip>
        <template #default="{ row }">
          <el-tooltip :content="ifaceDetail(row)" placement="top" :disabled="!row.interfaces.length">
            <span class="faint">{{ ifaceText(row) }}</span>
          </el-tooltip>
        </template>
      </el-table-column>

      <el-table-column label="接入" width="130">
        <template #default="{ row }"><span class="faint">{{ clock(row.connected_at) }}</span></template>
      </el-table-column>

      <el-table-column label="心跳" width="130">
        <template #default="{ row }"><span class="faint">{{ clock(row.last_heartbeat) }}</span></template>
      </el-table-column>

      <el-table-column label="延迟" width="86">
        <template #default="{ row }">
          <span class="faint">{{ row.last_heartbeat ? row.rtt_ms.toFixed(1) + ' ms' : '—' }}</span>
        </template>
      </el-table-column>

      <el-table-column label="累计流量" width="108">
        <template #default="{ row }">
          <div class="faint">↑ {{ bytes(row.up_bytes) }}</div>
          <div class="faint">↓ {{ bytes(row.down_bytes) }}</div>
        </template>
      </el-table-column>

      <el-table-column label="出站" min-width="120" show-overflow-tooltip>
        <template #default="{ row }">
          <span class="faint">{{ row.outbounds.length ? row.outbounds.join(', ') : '—' }}</span>
        </template>
      </el-table-column>

      <el-table-column label="操作" width="150" align="right">
        <template #default="{ row }">
          <el-button
            v-if="row.name && !row.local"
            link
            type="primary"
            @click="router.push('/clients/' + encodeURIComponent(row.name))"
          >配置</el-button>
          <el-button
            v-if="row.online && !row.local"
            link
            type="danger"
            :loading="busyId === row.id"
            @click="kick(row)"
          >断开</el-button>
        </template>
      </el-table-column>
    </el-table>

    <p class="page-hint">
      在线行来自当前会话；离线行是已注册但此刻没接进来的客户端。「本机」是服务端本机出口，常驻可用、不是会话。
      数据每 5 秒刷新，标签页不可见时暂停。
    </p>
  </el-card>
</template>
