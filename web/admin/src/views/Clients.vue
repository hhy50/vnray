<script setup lang="ts">
// 客户端列表：会话状态与流量概览，配置在弹窗里就地编辑（不跳页）。
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Refresh, Search } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { Client } from '../api/types'
import { bytes, clock, shortName } from '../lib/format'
import ClientConfigDialog from '../components/ClientConfigDialog.vue'

const clients = ref<Client[]>([])
const platforms = ref<string[]>([])
const loading = ref(false)
const loadError = ref('')
const q = ref('')
const platform = ref('')
const busyId = ref(0)

// 客户端配置以弹窗呈现：列表页保持不动，关掉弹窗即回到原上下文。
const configOpen = ref(false)
const configName = ref('')

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

function openConfig(client: Client) {
  configName.value = client.name
  configOpen.value = true
}

async function kick(client: Client) {
  try {
    await ElMessageBox.confirm(
      `断开 ${shortName(client.name)} 的会话？开了自动重连的客户端会重新接入。`,
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

// 轮询只在页面可见时跑，避免后台标签页空转。
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

function timeDetail(c: Client): string {
  const parts = [`接入 ${c.connected_at ?? '—'}`, `心跳 ${c.last_heartbeat ?? '—'}`]
  return parts.join('\n')
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
          style="width: 19rem"
        />
        <el-select v-model="platform" placeholder="全部平台" clearable style="width: 8.5rem">
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
      <el-table-column label="状态" width="78">
        <template #default="{ row }">
          <el-tag v-if="row.local" size="small" type="info">本机</el-tag>
          <el-tag v-else-if="row.online" size="small" type="success">在线</el-tag>
          <el-tag v-else size="small" type="info" effect="plain">离线</el-tag>
        </template>
      </el-table-column>

      <el-table-column label="客户端" min-width="200">
        <template #default="{ row }">
          <div class="client-name" :title="row.name || '未命名客户端'">
            <span v-if="row.name" class="client-short">{{ shortName(row.name) }}</span>
            <span v-else class="faint">未命名客户端</span>
            <span v-if="row.online && !row.local" class="faint">#{{ row.id }}</span>
          </div>
          <div class="faint" :title="row.hostname">
            {{ row.version || '未知版本' }} · {{ row.os }}/{{ row.arch }}
          </div>
        </template>
      </el-table-column>

      <el-table-column label="地址" min-width="132">
        <template #default="{ row }"><span class="mono">{{ row.remote || '—' }}</span></template>
      </el-table-column>

      <el-table-column label="网卡" min-width="124" show-overflow-tooltip>
        <template #default="{ row }">
          <el-tooltip :content="ifaceDetail(row)" placement="top" :disabled="!row.interfaces.length">
            <span class="faint mono">{{ ifaceText(row) }}</span>
          </el-tooltip>
        </template>
      </el-table-column>

      <el-table-column label="心跳" width="104">
        <template #default="{ row }">
          <el-tooltip :content="timeDetail(row)" placement="top">
            <span class="faint">{{ clock(row.last_heartbeat) }}</span>
          </el-tooltip>
        </template>
      </el-table-column>

      <el-table-column label="延迟" width="82">
        <template #default="{ row }">
          <span class="faint">{{ row.last_heartbeat ? row.rtt_ms.toFixed(1) + ' ms' : '—' }}</span>
        </template>
      </el-table-column>

      <el-table-column label="累计流量" width="100">
        <template #default="{ row }">
          <div class="faint">↑ {{ bytes(row.up_bytes) }}</div>
          <div class="faint">↓ {{ bytes(row.down_bytes) }}</div>
        </template>
      </el-table-column>

      <el-table-column label="出站" min-width="110" show-overflow-tooltip>
        <template #default="{ row }">
          <span class="faint">{{ row.outbounds.length ? row.outbounds.join(', ') : '—' }}</span>
        </template>
      </el-table-column>

      <el-table-column label="操作" width="126" align="right" fixed="right">
        <template #default="{ row }">
          <el-button v-if="row.name && !row.local" link type="primary" @click="openConfig(row)">
            配置
          </el-button>
          <el-button
            v-if="row.online && !row.local"
            link
            type="danger"
            :loading="busyId === row.id"
            @click="kick(row)"
          >
            断开
          </el-button>
        </template>
      </el-table-column>
    </el-table>

    <p class="page-hint">
      在线行来自当前会话，每 5 秒刷新（标签页不可见时暂停）；离线行是注册过但此刻没接进来的客户端。
      「本机」指服务端自身的出口，常驻可用、不是会话。
    </p>
  </el-card>

  <ClientConfigDialog v-model="configOpen" :name="configName" @saved="load(true)" />
</template>

<style scoped>
.client-name {
  display: flex;
  align-items: baseline;
  gap: 6px;
  min-width: 0;
}

.client-short {
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
