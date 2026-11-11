<script setup lang="ts">
// 性能指标：内存读数轮询展示，不落库、无历史；长期曲线交给外部采集器。
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { Refresh } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { MetricsSnapshot, MetricsSeries } from '../api/types'
import { useI18n } from '../i18n'
import { bytes, uptime } from '../lib/format'

const { t } = useI18n()

const snap = ref<MetricsSnapshot | null>(null)
const disabled = ref(false)
const loadError = ref('')
const loading = ref(false)
const autoRefresh = ref(true)
const filter = ref('')

async function load() {
  loading.value = true
  try {
    snap.value = await api.metrics()
    disabled.value = false
    loadError.value = ''
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      disabled.value = true
      loadError.value = ''
    } else {
      loadError.value = err instanceof ApiError ? err.message : t('common.loadFailed')
    }
  } finally {
    loading.value = false
  }
}

// 采集关闭时不轮询，开关打开后靠手动或重新进入页面恢复。
let timer: ReturnType<typeof setInterval> | null = null

function stopTimer() {
  if (timer !== null) {
    clearInterval(timer)
    timer = null
  }
}

function startTimer() {
  stopTimer()
  timer = setInterval(() => {
    if (!document.hidden && !disabled.value) void load()
  }, 2000)
}

onMounted(() => {
  void load().then(() => {
    if (autoRefresh.value && !disabled.value) startTimer()
  })
})

onUnmounted(stopTimer)

function onAutoRefreshChange(on: boolean) {
  if (on && !disabled.value) startTimer()
  else stopTimer()
}

const gauges = computed<MetricsSeries[]>(() =>
  (snap.value?.metrics ?? []).filter(
    (m) =>
      m.type !== 'histogram' &&
      (!filter.value || m.name.toLowerCase().includes(filter.value.toLowerCase())),
  ),
)

const histograms = computed<MetricsSeries[]>(() =>
  (snap.value?.metrics ?? []).filter(
    (m) =>
      m.type === 'histogram' &&
      (!filter.value || m.name.toLowerCase().includes(filter.value.toLowerCase())),
  ),
)

const isSeconds = (name: string) => name.endsWith('_seconds')

// 分位值从累积桶线性插值估算；最后一桶是 +Inf，插值上界取前一个有限上界。
function quantile(m: MetricsSeries, q: number): number | null {
  const buckets = m.buckets ?? []
  const total = buckets.reduce((acc, b) => acc + b.count, 0)
  if (!total) return null
  const target = q * total
  let prevLe = 0
  let prevCum = 0
  for (const b of buckets) {
    const cum = prevCum + b.count
    if (cum >= target) {
      if (!Number.isFinite(b.le)) return prevLe
      const frac = b.count > 0 ? (target - prevCum) / b.count : 0
      return prevLe + frac * (b.le - prevLe)
    }
    prevLe = b.le
    prevCum = cum
  }
  return null
}

function fmtSeconds(v: number | null): string {
  if (v === null) return '—'
  if (v >= 1) return `${v.toFixed(2)} s`
  if (v >= 0.001) return `${(v * 1000).toFixed(1)} ms`
  return `${(v * 1e6).toFixed(0)} µs`
}

function fmtSeriesValue(m: MetricsSeries): string {
  const v = m.value ?? 0
  if (isSeconds(m.name)) return fmtSeconds(v)
  return v.toLocaleString()
}

function fmtSum(m: MetricsSeries): string {
  const v = m.sum ?? 0
  if (isSeconds(m.name)) return fmtSeconds(v)
  return v.toLocaleString(undefined, { maximumFractionDigits: 2 })
}

function fmtPause(ns: number): string {
  if (!ns) return '—'
  if (ns < 1000) return `${ns} ns`
  return `${(ns / 1e6).toFixed(2)} ms`
}

function fmtGcCpu(f: number): string {
  if (!f) return '0%'
  return `${(f * 100).toFixed(2)}%`
}

const runtimeCards = computed(() => {
  const r = snap.value?.runtime
  if (!r) return []
  return [
    { label: t('metrics.goroutines'), value: r.goroutines.toLocaleString() },
    { label: t('metrics.threads'), value: r.threads.toLocaleString() },
    { label: t('metrics.heapAlloc'), value: bytes(r.heap_alloc) },
    { label: t('metrics.sysMem'), value: bytes(r.sys_mem) },
    { label: t('metrics.gcPause'), value: fmtPause(r.gc_pause_last_ns) },
    { label: t('metrics.gcCpu'), value: fmtGcCpu(r.gc_cpu_fraction) },
  ]
})

const uptimeText = computed(() =>
  snap.value ? uptime(snap.value.uptime_seconds) : '',
)
</script>

<template>
  <div class="metrics-page">
    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-alert v-else-if="disabled" :title="t('metrics.disabled')" type="info" :closable="false" show-icon>
      <p>{{ t('metrics.disabledTip') }}</p>
    </el-alert>

    <template v-else>
      <div class="toolbar">
        <span class="uptime">{{ t('metrics.uptime', { value: uptimeText }) }}</span>
        <el-input
          v-model="filter"
          :placeholder="t('metrics.filter')"
          clearable
          class="filter-input"
        />
        <span class="spacer" />
        <span class="auto-label">{{ t('metrics.autoRefresh') }}</span>
        <el-switch v-model="autoRefresh" @change="(v: any) => onAutoRefreshChange(!!v)" />
        <el-button :icon="Refresh" :loading="loading" @click="load">{{ t('metrics.refresh') }}</el-button>
      </div>

      <div class="stat-cards">
        <div v-for="card in runtimeCards" :key="card.label" class="stat-card">
          <div class="stat-value">{{ card.value }}</div>
          <div class="stat-label">{{ card.label }}</div>
        </div>
      </div>

      <h3 class="section-title">{{ t('metrics.sectionCounters') }}</h3>
      <el-table :data="gauges" size="small" class="metrics-table">
        <el-table-column :label="t('metrics.colName')" min-width="320">
          <template #default="{ row }">
            <span class="mono">{{ row.name }}</span>
          </template>
        </el-table-column>
        <el-table-column :label="t('metrics.colType')" width="100">
          <template #default="{ row }">
            <el-tag size="small" :type="row.type === 'counter' ? 'info' : 'success'">{{ row.type }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column :label="t('metrics.colValue')" width="160" align="right">
          <template #default="{ row }">
            <span class="mono">{{ fmtSeriesValue(row) }}</span>
          </template>
        </el-table-column>
      </el-table>

      <h3 class="section-title">{{ t('metrics.sectionHistograms') }}</h3>
      <el-table :data="histograms" size="small" class="metrics-table" row-key="name">
        <el-table-column type="expand">
          <template #default="{ row }">
            <div class="bucket-list">
              <div v-for="(b, i) in row.buckets" :key="i" class="bucket-row">
                <span class="mono bucket-le">≤ {{ Number.isFinite(b.le) ? fmtSeconds(b.le) : '+Inf' }}</span>
                <el-progress
                  :percentage="row.count ? Math.min(100, (b.count / row.count) * 100) : 0"
                  :stroke-width="10"
                  :show-text="false"
                  class="bucket-bar"
                />
                <span class="mono bucket-count">{{ b.count.toLocaleString() }}</span>
              </div>
            </div>
          </template>
        </el-table-column>
        <el-table-column :label="t('metrics.colName')" min-width="300">
          <template #default="{ row }">
            <span class="mono">{{ row.name }}</span>
          </template>
        </el-table-column>
        <el-table-column :label="t('metrics.colCount')" width="110" align="right">
          <template #default="{ row }">
            <span class="mono">{{ (row.count ?? 0).toLocaleString() }}</span>
          </template>
        </el-table-column>
        <el-table-column :label="t('metrics.colSum')" width="110" align="right">
          <template #default="{ row }">
            <span class="mono">{{ fmtSum(row) }}</span>
          </template>
        </el-table-column>
        <el-table-column :label="'p50'" width="100" align="right">
          <template #default="{ row }">
            <span class="mono">{{ isSeconds(row.name) ? fmtSeconds(quantile(row, 0.5)) : quantile(row, 0.5) }}</span>
          </template>
        </el-table-column>
        <el-table-column :label="'p95'" width="100" align="right">
          <template #default="{ row }">
            <span class="mono">{{ isSeconds(row.name) ? fmtSeconds(quantile(row, 0.95)) : quantile(row, 0.95) }}</span>
          </template>
        </el-table-column>
        <el-table-column :label="'p99'" width="100" align="right">
          <template #default="{ row }">
            <span class="mono">{{ isSeconds(row.name) ? fmtSeconds(quantile(row, 0.99)) : quantile(row, 0.99) }}</span>
          </template>
        </el-table-column>
      </el-table>

      <p class="footnote">{{ t('metrics.footnote') }}</p>
    </template>
  </div>
</template>

<style scoped>
.metrics-page {
  width: 100%;
  max-width: 1400px;
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
}

.uptime {
  font-size: 12.5px;
  color: var(--el-text-color-secondary);
  padding: 3px 10px;
  border-radius: 999px;
  background: var(--el-fill-color-light);
  white-space: nowrap;
}

.filter-input {
  width: 260px;
}

.spacer {
  flex: 1;
}

.auto-label {
  font-size: 12.5px;
  color: var(--el-text-color-regular);
}

.stat-cards {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: 12px;
}

.stat-card {
  background: var(--el-bg-color);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 10px;
  padding: 14px 16px;
}

.stat-value {
  font-size: 20px;
  font-weight: 600;
  color: var(--el-text-color-primary);
  font-family: var(--mono);
}

.stat-label {
  margin-top: 4px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}

.section-title {
  margin: 8px 0 0;
  font-size: 14px;
  font-weight: 600;
  color: var(--el-text-color-primary);
}

.metrics-table {
  width: 100%;
}

.mono {
  font-family: var(--mono);
  font-size: 12px;
}

.bucket-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 8px 16px 12px 48px;
}

.bucket-row {
  display: flex;
  align-items: center;
  gap: 12px;
}

.bucket-le {
  width: 110px;
  flex-shrink: 0;
  text-align: right;
  color: var(--el-text-color-secondary);
}

.bucket-bar {
  flex: 1;
  max-width: 420px;
}

.bucket-count {
  width: 90px;
  text-align: right;
}

.footnote {
  margin: 4px 0 0;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
</style>
