<script setup lang="ts">
import { computed, defineAsyncComponent, onMounted, onUnmounted, ref, watch } from 'vue'
import { FullScreen, Refresh, Search } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { Monitoring } from '../api/types'
import { bytes } from '../lib/format'
import { useI18n } from '../i18n'

const { t } = useI18n()
const UserMetricChart = defineAsyncComponent(() => import('../components/UserMetricChart.vue'))
const data = ref<Monitoring | null>(null)
const mode = ref('list')
const window = ref('1h')
const search = ref('')
const page = ref(1)
const loading = ref(false)
const error = ref('')
const disabled = ref(false)
const warming = ref(false)
const autoRefresh = ref(true)
const expanded = ref<number | null>(null)
const sort = ref<{ prop: 'tcp' | 'streams' | 'requests'; order: string | null }>({ prop: 'tcp', order: null })
let timer: ReturnType<typeof setTimeout> | undefined
let controller: AbortController | undefined
let disposed = false
let reload = false
const users = computed(() => {
  const rows = (data.value?.users ?? []).filter(u => `${u.name} ${u.protocol}`.toLowerCase().includes(search.value.toLowerCase()))
  if (sort.value.order) rows.sort((a, b) => (a[sort.value.prop] - b[sort.value.prop]) * (sort.value.order === 'ascending' ? 1 : -1))
  return rows
})
const visible = computed(() => users.value.slice((page.value - 1) * 12, page.value * 12))
const selected = computed(() => data.value?.users.find(u => u.id === expanded.value))
const dialog = computed({ get: () => !!selected.value, set: (open: boolean) => { if (!open) expanded.value = null } })
const speed = (value: number | null) => value == null ? '—' : `${bytes(value)}/s`
const sampled = computed(() => data.value ? new Date(data.value.sampled_at).toLocaleString() : '—')

function schedule() {
  clearTimeout(timer)
  if (autoRefresh.value && !disposed) timer = setTimeout(() => {
    if (document.hidden) schedule()
    else void load()
  }, Math.max(1000, (data.value?.interval_seconds ?? 2) * 1000))
}

async function load() {
  if (disposed) return
  if (loading.value) { reload = true; return }
  clearTimeout(timer)
  loading.value = true
  controller = new AbortController()
  const timeout = setTimeout(() => controller?.abort(), 15000)
  try {
    const next = await api.monitoring(window.value, controller.signal, mode.value === 'grid' || expanded.value != null)
    if (disposed) return
    data.value = next
    disabled.value = warming.value = false
    error.value = ''
    page.value = Math.min(page.value, Math.max(1, Math.ceil(users.value.length / 12)))
  } catch (err) {
    if (disposed) return
    disabled.value = err instanceof ApiError && err.status === 404
    warming.value = err instanceof ApiError && err.status === 503 && err.message.includes('warming up')
    error.value = disabled.value || warming.value ? '' : err instanceof Error ? err.message : t('common.loadFailed')
    if (disabled.value || warming.value) data.value = null
  } finally {
    clearTimeout(timeout)
    controller = undefined
    loading.value = false
    if (reload && !disposed) { reload = false; void load() }
    else schedule()
  }
}

function onVisible() { if (!document.hidden && autoRefresh.value) void load() }
function onSort(value: { prop: string; order: string | null }) {
  if (value.prop === 'tcp' || value.prop === 'streams' || value.prop === 'requests') sort.value = { prop: value.prop, order: value.order }
  else sort.value.order = null
  page.value = 1
}
watch(search, () => { page.value = 1 })
watch(window, () => { void load() })
watch([mode, expanded], () => { void load() })
watch(autoRefresh, schedule)
onMounted(() => { void load(); document.addEventListener('visibilitychange', onVisible) })
onUnmounted(() => { disposed = true; clearTimeout(timer); controller?.abort(); document.removeEventListener('visibilitychange', onVisible) })
</script>

<template>
  <div class="monitor-page">
    <div class="toolbar">
      <div><h2>{{ t('monitoring.users') }}</h2><p>{{ t('monitoring.subtitle') }}</p></div>
      <div class="controls">
        <el-switch v-model="autoRefresh" :active-text="t('metrics.autoRefresh')" />
        <el-button :icon="Refresh" :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
      </div>
    </div>
    <el-alert v-if="error" :title="t('monitoring.stale')" :description="error" type="error" :closable="false" show-icon />
    <el-alert v-if="disabled" :title="t('metrics.disabled')" :description="t('metrics.disabledTip')" type="info" :closable="false" show-icon />
    <el-alert v-else-if="warming" :title="t('metrics.warming')" type="info" :closable="false" show-icon />
    <el-skeleton v-if="!data && loading && !disabled && !warming" :rows="8" animated />
    <template v-if="data">
      <el-alert v-if="!data.history_stored" :title="t('monitoring.storageError')" type="warning" :closable="false" show-icon />
      <div class="sample-strip">
        <span>{{ t('metrics.sampledAt', { value: sampled }) }}</span>
        <span>{{ t('monitoring.cadence', { n: data.interval_seconds }) }}</span>
        <span>{{ t('monitoring.retained', { n: +(data.retention_seconds / 3600).toFixed(2) }) }}</span>
      </div>
      <div class="filters">
        <el-input v-model="search" :prefix-icon="Search" clearable :placeholder="t('monitoring.search')" class="search" />
        <div class="controls">
          <el-select v-model="window" :aria-label="t('monitoring.window')" style="width: 140px">
            <el-option v-for="item in ['5m', '15m', '1h', '6h', '24h', '168h']" :key="item" :value="item" :label="item" />
          </el-select>
          <el-radio-group v-model="mode" :aria-label="t('monitoring.layout')">
            <el-radio-button value="list">{{ t('monitoring.list') }}</el-radio-button>
            <el-radio-button value="grid">{{ t('monitoring.grid') }}</el-radio-button>
          </el-radio-group>
        </div>
      </div>
      <el-table v-if="mode === 'list'" :data="visible" row-key="id" stripe @sort-change="onSort">
        <el-table-column prop="name" :label="t('monitoring.user')" min-width="160" show-overflow-tooltip>
          <template #default="{ row }"><el-button link type="primary" @click="expanded = row.id">{{ row.name }}</el-button></template>
        </el-table-column>
        <el-table-column prop="protocol" :label="t('monitoring.protocol')" width="95" />
        <el-table-column prop="tcp" :label="t('monitoring.tcp')" width="110" sortable="custom" />
        <el-table-column prop="streams" :label="t('monitoring.streams')" width="110" sortable="custom" />
        <el-table-column prop="requests" :label="t('monitoring.requests')" width="135" sortable="custom" />
        <el-table-column :label="t('monitoring.upRate')" min-width="125"><template #default="{ row }">{{ speed(row.up_rate) }}</template></el-table-column>
        <el-table-column :label="t('monitoring.downRate')" min-width="125"><template #default="{ row }">{{ speed(row.down_rate) }}</template></el-table-column>
        <el-table-column :label="t('monitoring.up')" min-width="120"><template #default="{ row }">{{ bytes(row.up) }}</template></el-table-column>
        <el-table-column :label="t('monitoring.down')" min-width="120"><template #default="{ row }">{{ bytes(row.down) }}</template></el-table-column>
      </el-table>
      <div v-else-if="visible.length" class="chart-grid">
        <article v-for="user in visible" :key="user.id" class="chart-card">
          <header><div><strong>{{ user.name }}</strong><el-tag size="small" type="info">{{ user.protocol }}</el-tag></div>
            <el-button :icon="FullScreen" circle :aria-label="`${t('monitoring.expand')} ${user.name}`" :title="t('monitoring.expand')" @click="expanded = user.id" />
          </header>
          <div class="card-values"><span>TCP <b>{{ user.tcp }}</b></span><span>Streams <b>{{ user.streams }}</b></span><span>↑ {{ speed(user.up_rate) }}</span><span>↓ {{ speed(user.down_rate) }}</span></div>
          <UserMetricChart :user="user" :history="data.history" />
        </article>
      </div>
      <el-empty v-else :description="t('monitoring.empty')" />
      <el-pagination v-if="users.length > 12" v-model:current-page="page" :page-size="12" :total="users.length" layout="total, prev, pager, next" />
      <p class="hint">{{ t('monitoring.definition') }}</p>
    </template>
    <el-dialog v-model="dialog" :title="`${selected?.name ?? ''} · ${selected?.protocol ?? ''}`" width="min(1100px, 94vw)" destroy-on-close>
      <UserMetricChart v-if="selected && data" :user="selected" :history="data.history" large />
      <p class="hint">{{ t('monitoring.definition') }}</p>
    </el-dialog>
  </div>
</template>

<style scoped>
.monitor-page { display: flex; flex-direction: column; gap: 18px; }
.toolbar, .filters, .controls, .chart-card header, .chart-card header > div { display: flex; align-items: center; gap: 12px; }
.toolbar, .filters, .chart-card header { justify-content: space-between; }
h2 { margin: 0; font-size: 20px; }
.toolbar p, .hint { margin: 8px 0 0; color: var(--el-text-color-secondary); font-size: 12px; line-height: 1.8; }
.sample-strip, .card-values { display: flex; flex-wrap: wrap; gap: 20px; font-size: 12px; color: var(--el-text-color-secondary); }
.sample-strip { padding: 12px 16px; border-radius: 8px; background: var(--el-fill-color-light); }
.search { max-width: 300px; }
.chart-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 18px; }
.chart-card { min-width: 0; padding: 18px 14px 8px; border: 1px solid var(--el-border-color-lighter); border-radius: 12px; background: var(--el-bg-color); }
.chart-card header { margin: 0 6px 14px; }
.chart-card header > div { min-width: 0; }
.chart-card strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.card-values { margin: 0 6px 18px; gap: 16px; font-family: var(--mono); }
@media (min-width: 1800px) { .chart-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); } }
@media (max-width: 1000px) { .chart-grid { grid-template-columns: 1fr; } }
@media (max-width: 650px) { .toolbar, .filters { flex-direction: column; align-items: stretch; } .controls { flex-wrap: wrap; } .search { max-width: none; } }
</style>
