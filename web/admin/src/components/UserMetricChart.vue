<script setup lang="ts">
import { computed } from 'vue'
import { use } from 'echarts/core'
import { LineChart } from 'echarts/charts'
import { CanvasRenderer } from 'echarts/renderers'
import { GridComponent, TooltipComponent, LegendComponent, DataZoomComponent } from 'echarts/components'
import VChart from 'vue-echarts'
import type { MonitorUser, MonitorSample } from '../api/types'
import { useI18n } from '../i18n'
import { bytes } from '../lib/format'

use([LineChart, CanvasRenderer, GridComponent, TooltipComponent, LegendComponent, DataZoomComponent])
const props = defineProps<{ user: MonitorUser; history: MonitorSample[]; large?: boolean }>()
const { t } = useI18n()
const definitions = computed(() => [
  { key: 'tcp' as const, name: t('monitoring.tcp'), axis: 0 },
  { key: 'streams' as const, name: t('monitoring.streams'), axis: 0 },
  { key: 'requests' as const, name: t('monitoring.requests'), axis: 1 },
  { key: 'up_rate' as const, name: t('monitoring.upRate'), axis: 2 },
  { key: 'down_rate' as const, name: t('monitoring.downRate'), axis: 2 },
])
const option = computed(() => {
  const samples = props.history.map(s => ({ at: s.sampled_at, user: s.users.find(u => u.id === props.user.id) }))
  return {
    animation: false,
    color: ['#3b82f6', '#8b5cf6', '#f59e0b', '#10b981', '#ef6464'],
    textStyle: { color: '#8290a6' },
    legend: { top: 0, type: 'scroll', textStyle: { color: '#8290a6', fontSize: 11 } },
    tooltip: { trigger: 'axis', confine: true, renderMode: 'richText' },
    grid: { top: 62, bottom: props.large ? 68 : 32, left: 48, right: 106 },
    xAxis: { type: 'time', axisLabel: { hideOverlap: true }, splitLine: { show: false } },
    yAxis: [
      { type: 'value', minInterval: 1, name: t('monitoring.connections'), splitLine: { lineStyle: { color: '#8290a622' } } },
      { type: 'value', minInterval: 1, name: t('monitoring.requests'), splitLine: { show: false } },
      { type: 'value', offset: 50, name: 'B/s', splitLine: { show: false }, axisLabel: { formatter: (v: number) => bytes(v) } },
    ],
    dataZoom: props.large ? [{ type: 'slider', bottom: 8, height: 20 }, { type: 'inside' }] : [],
    series: definitions.value.map(d => ({
      name: d.name, type: 'line', yAxisIndex: d.axis, showSymbol: samples.length === 1,
      symbolSize: 5, connectNulls: false, lineStyle: { width: 2 },
      data: samples.map(s => [s.at, s.user?.[d.key] ?? null]),
      tooltip: { valueFormatter: (v: number | null) => v == null ? '—' : d.axis === 2 ? `${bytes(v)}/s` : v.toLocaleString() },
    })),
  }
})
</script>

<template>
  <VChart v-if="history.length" :option="option" autoresize class="user-chart" :class="{ large }" :aria-label="`${user.name} ${t('monitoring.chart')}`" />
  <el-empty v-else :description="t('monitoring.noHistory')" :image-size="64" />
</template>

<style scoped>
.user-chart { height: 270px; width: 100%; }
.user-chart.large { height: min(62vh, 620px); min-height: 300px; }
</style>
