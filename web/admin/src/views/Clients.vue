<script setup lang="ts">
// 客户端列表：会话状态与流量概览，配置在弹窗里就地编辑（不跳页）。
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Refresh, Search } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { Client } from '../api/types'
import { bytes, clock, shortName } from '../lib/format'
import ClientConfigDialog from '../components/ClientConfigDialog.vue'
import { useI18n } from '../i18n'

const { t } = useI18n()

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
    loadError.value = err instanceof ApiError ? err.message : t('common.loadFailed')
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
      t('clients.kickConfirm', { name: shortName(client.name) }),
      t('clients.kickTitle'),
      { type: 'warning', confirmButtonText: t('clients.disconnect'), cancelButtonText: t('common.cancel') },
    )
  } catch {
    return
  }
  busyId.value = client.id
  try {
    await api.clientKick(client.id)
    ElMessage.success(t('clients.kicked'))
    await load(true)
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('clients.kickFailed'))
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
  const parts = [
    t('clients.connectedAt', { value: c.connected_at ?? '—' }),
    t('clients.heartbeatAt', { value: c.last_heartbeat ?? '—' }),
  ]
  return parts.join('\n')
}
</script>

<template>
  <el-card shadow="never" class="stack">
    <template #header>
      <div class="toolbar">
        <strong>{{ t('clients.online', { n: onlineCount }) }}</strong>
        <span class="faint">{{ t('clients.total', { n: clients.length }) }}</span>
        <div class="spacer" />
        <el-input
          v-model="q"
          :placeholder="t('clients.searchPlaceholder')"
          clearable
          :prefix-icon="Search"
          style="width: 19rem"
        />
        <el-select v-model="platform" :placeholder="t('clients.allPlatforms')" clearable style="width: 8.5rem">
          <el-option :label="t('clients.allPlatforms')" value="" />
          <el-option v-for="p in platforms" :key="p" :label="p" :value="p" />
        </el-select>
        <el-button :icon="Refresh" :loading="loading" @click="load()">{{ t('common.refresh') }}</el-button>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-table
      v-else
      v-loading="loading"
      :data="filtered"
      row-key="id"
      size="small"
      :empty-text="t('clients.empty')"
    >
      <el-table-column :label="t('clients.colStatus')" width="78">
        <template #default="{ row }">
          <el-tag v-if="row.local" size="small" type="info">{{ t('common.local') }}</el-tag>
          <el-tag v-else-if="row.online" size="small" type="success">{{ t('common.online') }}</el-tag>
          <el-tag v-else size="small" type="info" effect="plain">{{ t('common.offline') }}</el-tag>
        </template>
      </el-table-column>

      <el-table-column :label="t('clients.colClient')" min-width="200">
        <template #default="{ row }">
          <div class="client-name" :title="row.name || t('clients.unnamed')">
            <span v-if="row.name" class="client-short">{{ shortName(row.name) }}</span>
            <span v-else class="faint">{{ t('clients.unnamed') }}</span>
            <span v-if="row.online && !row.local" class="faint">#{{ row.id }}</span>
          </div>
          <div class="faint" :title="row.hostname">
            {{ row.version || t('clients.unknownVersion') }} · {{ row.os }}/{{ row.arch }}
          </div>
        </template>
      </el-table-column>

      <el-table-column :label="t('clients.colAddress')" min-width="132">
        <template #default="{ row }"><span class="mono">{{ row.remote || '—' }}</span></template>
      </el-table-column>

      <el-table-column :label="t('clients.colIface')" min-width="124" show-overflow-tooltip>
        <template #default="{ row }">
          <el-tooltip :content="ifaceDetail(row)" placement="top" :disabled="!row.interfaces.length">
            <span class="faint mono">{{ ifaceText(row) }}</span>
          </el-tooltip>
        </template>
      </el-table-column>

      <el-table-column :label="t('clients.colHeartbeat')" width="104">
        <template #default="{ row }">
          <el-tooltip :content="timeDetail(row)" placement="top">
            <span class="faint">{{ clock(row.last_heartbeat) }}</span>
          </el-tooltip>
        </template>
      </el-table-column>

      <el-table-column :label="t('clients.colRtt')" width="82">
        <template #default="{ row }">
          <span class="faint">{{ row.last_heartbeat ? row.rtt_ms.toFixed(1) + ' ms' : '—' }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('clients.colTraffic')" width="100">
        <template #default="{ row }">
          <div class="faint">↑ {{ bytes(row.up_bytes) }}</div>
          <div class="faint">↓ {{ bytes(row.down_bytes) }}</div>
        </template>
      </el-table-column>

      <el-table-column :label="t('clients.colOutbound')" min-width="110" show-overflow-tooltip>
        <template #default="{ row }">
          <span class="faint">{{ row.outbounds.length ? row.outbounds.join(', ') : '—' }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('clients.colActions')" width="126" align="right" fixed="right">
        <template #default="{ row }">
          <el-button v-if="row.name && !row.local" link type="primary" @click="openConfig(row)">
            {{ t('common.configure') }}
          </el-button>
          <el-button
            v-if="row.online && !row.local"
            link
            type="danger"
            :loading="busyId === row.id"
            @click="kick(row)"
          >
            {{ t('clients.disconnect') }}
          </el-button>
        </template>
      </el-table-column>
    </el-table>
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
