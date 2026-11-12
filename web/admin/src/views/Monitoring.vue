<script setup lang="ts">
import { computed, defineAsyncComponent } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import UserMonitoring from './UserMonitoring.vue'
import { useI18n } from '../i18n'

const { t } = useI18n()
const Metrics = defineAsyncComponent(() => import('./Metrics.vue'))
const route = useRoute()
const router = useRouter()
const tab = computed({
  get: () => route.query.tab === 'host' ? 'host' : 'users',
  set: (value: string) => { void router.replace({ query: { ...route.query, tab: value } }) },
})
</script>

<template>
  <div>
    <el-tabs v-model="tab">
      <el-tab-pane name="users" :label="t('monitoring.users')" />
      <el-tab-pane name="host" :label="t('monitoring.host')" />
    </el-tabs>
    <UserMonitoring v-if="tab === 'users'" />
    <Metrics v-else />
  </div>
</template>
