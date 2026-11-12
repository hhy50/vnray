<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { Refresh, Monitor, Cpu, DataLine } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { MetricsSnapshot } from '../api/types'
import MetricCard from '../components/MetricCard.vue'
import { useI18n } from '../i18n'
import { bytes, uptime } from '../lib/format'

const { t } = useI18n()
const snap = ref<MetricsSnapshot | null>(null)
const disabled = ref(false)
const warming = ref(false)
const loadError = ref('')
const loading = ref(false)
const autoRefresh = ref(true)
const filter = ref('')
const history = ref<{ rx: number[]; tx: number[] }>({ rx: [], tx: [] })
let timer: ReturnType<typeof setTimeout> | undefined
let controller: AbortController | undefined
let disposed = false

function schedule() {
  clearTimeout(timer)
  if (autoRefresh.value && !disabled.value && !disposed) timer = setTimeout(() => {
    if (document.hidden) schedule()
    else void load()
  }, Math.max(1000, (snap.value?.interval_seconds ?? 2) * 1000))
}

async function load() {
  if (loading.value || disposed) return
  clearTimeout(timer)
  loading.value = true
  controller = new AbortController()
  const timeout = setTimeout(() => controller?.abort(), 10000)
  try {
    const [next, monitoring] = await Promise.all([
      api.metrics(controller.signal),
      api.monitoring('1h', controller.signal, true).catch(() => null),
    ])
    if (disposed) return
    if (monitoring) {
      history.value = {
        rx: monitoring.history.map(s => s.server.receive_bytes_per_second).filter((v): v is number => v != null),
        tx: monitoring.history.map(s => s.server.transmit_bytes_per_second).filter((v): v is number => v != null),
      }
    } else if (next.sampled_at !== snap.value?.sampled_at) {
      for (const [key, value] of [['rx', next.server.receive_bytes_per_second], ['tx', next.server.transmit_bytes_per_second]] as const) {
        history.value[key] = value == null ? [] : [...history.value[key], value].slice(-60)
      }
    }
    snap.value = next
    disabled.value = false
    warming.value = false
    loadError.value = ''
  } catch (err) {
    if (disposed) return
    disabled.value = err instanceof ApiError && err.status === 404
    warming.value = err instanceof ApiError && err.status === 503
    loadError.value = disabled.value || warming.value ? '' : err instanceof ApiError ? err.message : t('common.loadFailed')
    if (disabled.value || warming.value) {
      snap.value = null
      history.value = { rx: [], tx: [] }
    }
  } finally {
    clearTimeout(timeout)
    loading.value = false
    controller = undefined
    schedule()
  }
}

function onVisible() { if (!document.hidden && autoRefresh.value && !disabled.value) void load() }
watch(autoRefresh, schedule)
onMounted(() => { void load(); document.addEventListener('visibilitychange', onVisible) })
onUnmounted(() => { disposed = true; clearTimeout(timer); controller?.abort(); document.removeEventListener('visibilitychange', onVisible) })

const percent = (v: number | null | undefined) => v == null ? '—' : `${v.toFixed(1)}%`
const size = (v: number | null | undefined) => v == null ? '—' : bytes(v)
const speed = (v: number | null | undefined) => v == null ? '—' : `${bytes(v)}/s`
const count = (v: number | undefined) => v == null ? '—' : v.toLocaleString()
const series = computed(() => new Map((snap.value?.metrics ?? []).map(m => [m.name, m.value ?? 0])))
const metric = (name: string) => series.value.get(name)
const memoryPercent = computed(() => {
  const s = snap.value?.server
  return s?.memory_total && s.memory_used != null ? s.memory_used / s.memory_total * 100 : null
})
const sampledAt = computed(() => snap.value ? new Date(snap.value.sampled_at).toLocaleTimeString() : '—')
const raw = computed(() => (snap.value?.metrics ?? []).filter(m => m.name.toLowerCase().includes(filter.value.toLowerCase())))
const allocationCount = computed(() => snap.value ? snap.value.pipe_allocations + snap.value.transfer.buffer_allocations : undefined)
const allocatedBytes = computed(() => snap.value ? snap.value.pipe_allocated_bytes + snap.value.transfer.buffer_allocated_bytes : undefined)
</script>

<template>
  <div class="metrics-page">
    <div class="toolbar">
      <div class="page-title"><h2>{{ t('metrics.title') }}</h2><span>{{ t('metrics.subtitle') }}</span></div>
      <div class="refresh-controls">
        <span class="auto-label">{{ t('metrics.autoRefresh') }}</span>
        <el-switch v-model="autoRefresh" :aria-label="t('metrics.autoRefresh')" />
        <el-button :icon="Refresh" :loading="loading" @click="load">{{ t('metrics.refresh') }}</el-button>
      </div>
    </div>
    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />
    <el-alert v-if="disabled" :title="t('metrics.disabled')" :description="t('metrics.disabledTip')" type="info" :closable="false" show-icon />
    <el-alert v-else-if="warming" :title="t('metrics.warming')" type="info" :closable="false" show-icon />
    <el-skeleton v-if="!snap && !disabled && !warming && !loadError" :rows="8" animated />

    <template v-if="snap">
      <div class="sample-strip">
        <span class="sample-status"><i />{{ t('metrics.sampledAt', { value: sampledAt }) }}</span>
        <span>{{ t('metrics.uptime', { value: uptime(snap.uptime_seconds) }) }}</span>
        <span>{{ t('monitoring.cadence', { n: snap.interval_seconds }) }}</span>
      </div>

      <section class="metric-section">
        <header class="section-header"><div class="section-icon"><el-icon><Monitor /></el-icon></div><div><h3>{{ t('metrics.server') }}</h3><p>{{ t('metrics.serverHint') }}</p></div></header>
        <div class="cards four">
          <MetricCard :label="t('metrics.hostCPU')" :value="percent(snap.server.cpu_percent)" :percent="snap.server.cpu_percent" :detail="t('metrics.hostCPUHint')" />
          <MetricCard :label="t('metrics.hostMemory')" :value="size(snap.server.memory_used)" :percent="memoryPercent" :detail="`${t('metrics.total')} ${size(snap.server.memory_total)}`" />
          <MetricCard :label="t('metrics.hostReceive')" :value="speed(snap.server.receive_bytes_per_second)" :history="history.rx" :detail="t('metrics.networkHint', { n: snap.server.interfaces })" tone="green" />
          <MetricCard :label="t('metrics.hostTransmit')" :value="speed(snap.server.transmit_bytes_per_second)" :history="history.tx" :detail="t('metrics.networkHint', { n: snap.server.interfaces })" tone="green" />
        </div>
      </section>

      <section class="metric-section">
        <header class="section-header"><div class="section-icon green"><el-icon><Cpu /></el-icon></div><div><h3>{{ t('metrics.process') }}</h3><p>{{ t('metrics.processHint') }}</p></div></header>
        <div class="cards three">
          <MetricCard :label="t('metrics.processCPU')" :value="percent(snap.process.cpu_percent)" :detail="t('metrics.processCPUHint')" tone="green" />
          <MetricCard :label="t('metrics.residentMemory')" :value="size(snap.process.resident_bytes)" :detail="t('metrics.residentHint')" tone="green" />
          <MetricCard :label="t('metrics.heapAlloc')" :value="size(snap.process.heap_alloc)" :detail="`${t('metrics.goMemory')} ${size(snap.process.go_memory)}`" tone="green" />
          <MetricCard :label="t('metrics.goroutines')" :value="count(snap.process.goroutines)" :detail="`GOMAXPROCS ${snap.process.gomaxprocs}`" tone="green" />
          <MetricCard :label="t('metrics.gcCount')" :value="count(snap.process.gc_count)" :detail="`${t('metrics.gcPause')} ${snap.process.gc_pause_p99_seconds == null ? '—' : (snap.process.gc_pause_p99_seconds * 1000).toFixed(3) + ' ms'}`" tone="green" />
          <MetricCard :label="t('metrics.gcCpu')" :value="percent(snap.process.gc_cpu_percent)" :detail="`${t('metrics.nextGC')} ${size(snap.process.next_gc)}`" tone="green" />
        </div>
      </section>

      <section class="metric-section">
        <header class="section-header"><div class="section-icon amber"><el-icon><DataLine /></el-icon></div><div><h3>{{ t('metrics.operations') }}</h3><p>{{ t('metrics.operationsHint') }}</p></div></header>
        <div class="cards four">
          <MetricCard :label="t('metrics.buffered')" :value="size(metric('pipe_buffered_bytes'))" :detail="t('metrics.bufferedHint')" tone="amber" />
          <MetricCard :label="t('metrics.allocations')" :value="count(allocationCount)" :detail="`${t('metrics.allocatedTotal')} ${size(allocatedBytes)}`" tone="amber" />
          <MetricCard :label="t('metrics.handshakes')" :value="count(metric('client_handshakes_total'))" :detail="`${t('metrics.failures')} ${count(metric('client_handshake_failures_total'))}`" tone="amber" />
          <MetricCard :label="t('metrics.heartbeats')" :value="count(metric('client_heartbeats_total'))" :detail="`${t('metrics.failures')} ${count(metric('client_heartbeat_failures_total'))}`" tone="amber" />
          <MetricCard :label="t('metrics.requests')" :value="count(snap.transfer.requests)" :detail="t('metrics.requestsHint')" tone="amber" />
          <MetricCard :label="t('metrics.active')" :value="count(snap.transfer.active)" :detail="`${t('metrics.sessions')} ${count(metric('tcp_active_sessions'))}`" tone="amber" />
          <MetricCard :label="t('metrics.upload')" :value="size(snap.transfer.upload_bytes)" :detail="t('metrics.uploadHint')" tone="amber" />
          <MetricCard :label="t('metrics.download')" :value="size(snap.transfer.download_bytes)" :detail="t('metrics.downloadHint')" tone="amber" />
        </div>
      </section>

      <el-collapse class="raw-details">
        <el-collapse-item :title="t('metrics.rawDetails')" name="raw">
          <el-input v-model="filter" :placeholder="t('metrics.filter')" :aria-label="t('metrics.filter')" clearable class="filter-input" />
          <el-table :data="raw" size="small" row-key="name">
            <el-table-column prop="name" :label="t('metrics.colName')" min-width="280" />
            <el-table-column prop="type" :label="t('metrics.colType')" width="110" />
            <el-table-column :label="t('metrics.colValue')" min-width="120" align="right">
              <template #default="{ row }">{{ row.type === 'histogram' ? count(row.count) : count(row.value ?? 0) }}</template>
            </el-table-column>
          </el-table>
        </el-collapse-item>
      </el-collapse>
      <p class="footnote">{{ t('metrics.footnote') }}</p>
    </template>
  </div>
</template>

<style scoped>
.metrics-page { max-width: 1440px; display: flex; flex-direction: column; gap: 24px; padding-bottom: 24px; }
.toolbar { display: flex; align-items: center; justify-content: space-between; gap: 16px; }
.page-title h2 { margin: 0 0 7px; font-size: 22px; font-weight: 650; }
.page-title span, .auto-label { font-size: 12px; color: var(--el-text-color-secondary); }
.refresh-controls { display: flex; align-items: center; gap: 12px; flex-shrink: 0; }
.sample-strip { display: flex; flex-wrap: wrap; gap: 12px 24px; padding: 12px 16px; border-radius: 9px; background: var(--el-fill-color-light); color: var(--el-text-color-secondary); font-size: 12px; }
.sample-status { display: flex; align-items: center; gap: 8px; }
.sample-status i { width: 6px; height: 6px; border-radius: 50%; background: var(--el-color-success); }
.section-header { display: flex; align-items: center; gap: 12px; margin-bottom: 14px; }
.section-header h3 { margin: 0; font-size: 15px; font-weight: 600; }
.section-header p { margin: 5px 0 0; font-size: 12px; color: var(--el-text-color-secondary); }
.section-icon { width: 36px; height: 36px; display: grid; place-items: center; border-radius: 10px; background: var(--el-color-primary-light-9); color: var(--el-color-primary); font-size: 19px; flex-shrink: 0; }
.section-icon.green { background: var(--el-color-success-light-9); color: var(--el-color-success); }
.section-icon.amber { background: var(--el-color-warning-light-9); color: var(--el-color-warning); }
.cards { display: grid; gap: 12px; }
.four { grid-template-columns: repeat(4, minmax(0, 1fr)); }
.three { grid-template-columns: repeat(3, minmax(0, 1fr)); }
.filter-input { max-width: 360px; margin: 8px 0 16px; }
.footnote { font-size: 12px; line-height: 1.8; color: var(--el-text-color-secondary); margin: 0; }
.raw-details { border-radius: 8px; }
@media (max-width: 1100px) { .four { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 760px) { .three { grid-template-columns: repeat(2, minmax(0, 1fr)); } .toolbar { align-items: flex-start; flex-direction: column; } }
@media (max-width: 440px) { .cards { grid-template-columns: 1fr; } }
</style>
