<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{
  label: string
  value: string
  detail?: string
  percent?: number | null
  history?: number[]
  tone?: 'blue' | 'green' | 'amber'
}>()

const points = computed(() => {
  const values = props.history ?? []
  if (values.length < 2) return ''
  const max = Math.max(1, ...values)
  return values.map((v, i) => `${(i / (values.length - 1)) * 200},${32 - (v / max) * 30}`).join(' ')
})
</script>

<template>
  <article class="metric-card" :class="tone ?? 'blue'">
    <div class="metric-label">{{ label }}</div>
    <div class="metric-value">{{ value }}</div>
    <div class="metric-detail">{{ detail || '\u00a0' }}</div>
    <div v-if="percent != null" class="meter" role="meter" :aria-label="label" :aria-valuenow="percent" :aria-valuemin="0" :aria-valuemax="100">
      <div :style="{ width: `${Math.min(100, Math.max(0, percent))}%` }" />
    </div>
    <svg v-else-if="points" class="sparkline" viewBox="0 0 200 34" preserveAspectRatio="none" aria-hidden="true">
      <polyline :points="points" fill="none" stroke="currentColor" stroke-width="1.8" vector-effect="non-scaling-stroke" />
    </svg>
  </article>
</template>

<style scoped>
.metric-card { --accent: var(--el-color-primary); position: relative; min-width: 0; overflow: hidden; padding: 18px 20px 20px; border: 1px solid var(--el-border-color-lighter); border-radius: 12px; background: var(--el-bg-color); }
.green { --accent: var(--el-color-success); }
.amber { --accent: var(--el-color-warning); }
.metric-label { color: var(--el-text-color-secondary); font-size: 12px; }
.metric-value { margin-top: 10px; font: 600 24px/1.3 var(--mono); color: var(--el-text-color-primary); letter-spacing: -0.6px; overflow-wrap: anywhere; }
.metric-detail { min-height: 16px; margin-top: 8px; color: var(--el-text-color-secondary); font-size: 11px; }
.meter { height: 4px; background: var(--el-fill-color); border-radius: 4px; margin-top: 16px; overflow: hidden; }
.meter > div { height: 100%; background: var(--accent); transition: width .3s; border-radius: inherit; }
.sparkline { color: var(--accent); width: 100%; height: 28px; margin-top: 8px; }
</style>
