<script setup lang="ts">
// 服务端配置：只收运行期可热生效的参数，保存即落库；需要重启的项留在配置文件里。
// 下发给所有客户端的全局 Host 与 IP 段规则也在这里维护（原独立「客户端配置」页已并入）。
// 页面按「基础 / 客户端 / 其他」分三个标签，标签上的红点表示该页有未保存改动。
import { computed, onMounted, ref } from 'vue'
import { Delete, Plus, RefreshLeft } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { GlobalClientConfig, ServiceConfig } from '../api/types'
import { useI18n } from '../i18n'

const { t } = useI18n()

type ConfigTab = 'basic' | 'client' | 'others'

const activeTab = ref<ConfigTab>('basic')
const form = ref<ServiceConfig | null>(null)
const loadError = ref('')
const loading = ref(false)
const saving = ref(false)

// 全局客户端配置是独立的保存接口，脏检查与服务端配置分开。
const globalForm = ref<GlobalClientConfig | null>(null)
const globalSnapshot = ref('')

// 服务端配置快照：按字段对比，才能定位哪个标签页有改动。
const snapshotForm = ref<ServiceConfig | null>(null)

// 基础与其他两个标签页的字段清单合起来覆盖 ServiceConfig 的全部可编辑字段。
const BASIC_KEYS = [
  'public_host',
  'heartbeat_enabled',
  'heartbeat_interval',
  'heartbeat_timeout',
  'server_proxy_enabled',
  'server_proxy_public',
  'server_proxy_private',
  'server_proxy_routing',
] as const
const OTHERS_KEYS = [
  'keepalive_interval',
  'keepalive_timeout',
  'max_stream_window_kb',
  'accept_backlog',
  'stream_open_timeout',
  'log_level',
  'traffic_flush_interval',
  'log_rotate_max_size_mb',
  'log_rotate_max_backups',
  'log_rotate_max_age_days',
  'metrics_enabled',
  'metrics_interval',
  'metrics_retention',
] as const

function keysDirty(keys: readonly (keyof ServiceConfig)[]): boolean {
  if (!form.value || !snapshotForm.value) return false
  return keys.some((key) => form.value![key] !== snapshotForm.value![key])
}

const basicDirty = computed(() => keysDirty(BASIC_KEYS))
const othersDirty = computed(() => keysDirty(OTHERS_KEYS))
const globalDirty = computed(
  () => !!globalForm.value && JSON.stringify(globalForm.value) !== globalSnapshot.value,
)
const dirty = computed(() => basicDirty.value || othersDirty.value || globalDirty.value)

async function load() {
  loading.value = true
  try {
    const [data, global] = await Promise.all([api.serviceConfig(), api.globalClientConfig()])
    form.value = data
    snapshotForm.value = { ...data }
    globalForm.value = global.global_client
    globalSnapshot.value = JSON.stringify(global.global_client)
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : t('common.loadFailed')
  } finally {
    loading.value = false
  }
}

async function save() {
  if (!form.value) return
  saving.value = true
  try {
    const wasServiceDirty = basicDirty.value || othersDirty.value
    if (wasServiceDirty) await api.serviceConfigSave(form.value)
    if (globalForm.value && globalDirty.value) {
      await api.globalClientConfigSave({ global_client: globalForm.value })
    }
    snapshotForm.value = { ...form.value }
    globalSnapshot.value = JSON.stringify(globalForm.value)
    ElMessage.success(t(wasServiceDirty ? 'serviceConfig.saved' : 'clientConfig.saved'))
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.saveFailed'))
  } finally {
    saving.value = false
  }
}

function reset() {
  if (snapshotForm.value) {
    form.value = { ...snapshotForm.value }
  }
  if (globalSnapshot.value && globalForm.value) {
    globalForm.value = JSON.parse(globalSnapshot.value) as GlobalClientConfig
  }
}

onMounted(load)
</script>

<template>
  <div class="config-page">
    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-form v-else-if="form" v-loading="loading" class="config-form" label-position="top" @submit.prevent="save">
      <p class="page-hint intro">
        {{ t('serviceConfig.intro') }}
      </p>

      <el-tabs v-model="activeTab" class="config-tabs">
        <el-tab-pane name="basic">
          <template #label>
            <span class="tab-label">
              {{ t('serviceConfig.tabBasic') }}
              <span v-if="basicDirty" class="dirty-dot" />
            </span>
          </template>

          <div class="pane">
            <div class="pane-grid pane-grid--two">
              <el-card shadow="never" class="config-card">
                <template #header>
                  <div class="card-title">{{ t('serviceConfig.cardPublicHost') }}</div>
                </template>
                <el-form-item :label="t('serviceConfig.publicHostLabel')">
                  <el-input v-model="form.public_host" placeholder="vpn.example.com" />
                </el-form-item>
                <p class="field-tip">
                  {{ t('serviceConfig.publicHostTip') }}
                </p>
              </el-card>

              <el-card shadow="never" class="config-card">
                <template #header>
                  <div class="card-title">{{ t('serviceConfig.cardHeartbeat') }}</div>
                </template>
                <div class="form-row">
                  <el-form-item :label="t('serviceConfig.heartbeatEnabled')" class="switch-item">
                    <el-switch v-model="form.heartbeat_enabled" />
                  </el-form-item>
                  <el-form-item :label="t('serviceConfig.heartbeatInterval')">
                    <el-input v-model="form.heartbeat_interval" placeholder="5s" class="mono" />
                  </el-form-item>
                  <el-form-item :label="t('serviceConfig.heartbeatTimeout')">
                    <el-input v-model="form.heartbeat_timeout" placeholder="3s" class="mono" />
                  </el-form-item>
                </div>
                <p class="field-tip">{{ t('serviceConfig.heartbeatTip') }}</p>
              </el-card>
            </div>

            <el-card shadow="never" class="config-card">
              <template #header>
                <div class="card-title">{{ t('serviceConfig.cardServerProxy') }}</div>
              </template>
              <el-form-item :label="t('serviceConfig.enableServerProxy')">
                <el-switch v-model="form.server_proxy_enabled" />
              </el-form-item>
              <p class="field-tip">
                {{ t('serviceConfig.proxyTipBefore') }}<span class="mono">server:self</span>{{ t('serviceConfig.proxyTipAfter') }}
              </p>
              <div class="form-row">
                <el-form-item :label="t('serviceConfig.publicLabel')">
                  <el-input v-model="form.server_proxy_public" placeholder="192.168.31.*, fd00::/64" class="mono" />
                </el-form-item>
                <el-form-item :label="t('serviceConfig.privateLabel')">
                  <el-input v-model="form.server_proxy_private" class="mono" />
                </el-form-item>
              </div>
              <el-form-item :label="t('serviceConfig.routingLabel')">
                <el-input v-model="form.server_proxy_routing" type="textarea" :rows="9" class="code-area" spellcheck="false" />
              </el-form-item>
            </el-card>
          </div>
        </el-tab-pane>

        <el-tab-pane name="client">
          <template #label>
            <span class="tab-label">
              {{ t('serviceConfig.tabClient') }}
              <span v-if="globalDirty" class="dirty-dot" />
            </span>
          </template>

          <div class="pane">
            <p class="page-hint intro client-intro">
              {{ t('clientConfig.intro') }}
            </p>

            <div class="pane-grid pane-grid--two">
              <el-card shadow="never" class="config-card">
                <template #header>
                  <div class="card-head">
                    <span class="card-title">{{ t('clientConfig.cardHosts') }}</span>
                    <span class="faint">{{ t('clientConfig.count', { n: globalForm?.custom_hosts.length ?? 0 }) }}</span>
                    <div class="spacer" />
                    <el-button
                      size="small"
                      :icon="Plus"
                      :disabled="!globalForm"
                      @click="globalForm?.custom_hosts.push({ domain: '', ip: '' })"
                    >
                      {{ t('clientConfig.addHost') }}
                    </el-button>
                  </div>
                </template>
                <el-table
                  :data="globalForm?.custom_hosts"
                  size="small"
                  :empty-text="t('clientConfig.hostsEmpty')"
                >
                  <el-table-column :label="t('clientConfig.colDomain')" min-width="240">
                    <template #default="{ row }">
                      <el-input v-model="row.domain" placeholder="vpn.example.com" />
                    </template>
                  </el-table-column>
                  <el-table-column :label="t('clientConfig.colFixedIP')" min-width="200">
                    <template #default="{ row }">
                      <el-input v-model="row.ip" placeholder="192.168.60.1" class="mono" />
                    </template>
                  </el-table-column>
                  <el-table-column width="60" align="right">
                    <template #default="{ $index }">
                      <el-button
                        link
                        type="danger"
                        :icon="Delete"
                        :aria-label="t('clientConfig.deleteHost')"
                        @click="globalForm?.custom_hosts.splice($index, 1)"
                      />
                    </template>
                  </el-table-column>
                </el-table>
                <p class="field-tip">
                  {{ t('clientConfig.hostsTip') }}
                </p>
              </el-card>

              <el-card shadow="never" class="config-card">
                <template #header>
                  <div class="card-head">
                    <span class="card-title">{{ t('clientConfig.cardMaps') }}</span>
                    <span class="faint">{{ t('clientConfig.count', { n: globalForm?.ip_maps.length ?? 0 }) }}</span>
                    <div class="spacer" />
                    <el-button
                      size="small"
                      :icon="Plus"
                      :disabled="!globalForm"
                      @click="globalForm?.ip_maps.push({ from: '', to: '' })"
                    >
                      {{ t('clientConfig.addMap') }}
                    </el-button>
                  </div>
                </template>
                <el-table
                  :data="globalForm?.ip_maps"
                  size="small"
                  :empty-text="t('clientConfig.mapsEmpty')"
                >
                  <el-table-column :label="t('clientConfig.colFrom')" min-width="240">
                    <template #default="{ row }">
                      <el-input v-model="row.from" placeholder="186.12.12.*" class="mono" />
                    </template>
                  </el-table-column>
                  <el-table-column :label="t('clientConfig.colTo')" min-width="200">
                    <template #default="{ row }">
                      <el-input v-model="row.to" placeholder="192.168.60.*" class="mono" />
                    </template>
                  </el-table-column>
                  <el-table-column width="60" align="right">
                    <template #default="{ $index }">
                      <el-button
                        link
                        type="danger"
                        :icon="Delete"
                        :aria-label="t('clientConfig.deleteMap')"
                        @click="globalForm?.ip_maps.splice($index, 1)"
                      />
                    </template>
                  </el-table-column>
                </el-table>
                <p class="field-tip">
                  {{ t('clientConfig.mapsTipBefore') }}<span class="mono">*</span>{{ t('clientConfig.mapsTipMiddle') }}<span class="mono">*</span>{{ t('clientConfig.mapsTipAfter') }}
                </p>
              </el-card>
            </div>
          </div>
        </el-tab-pane>

        <el-tab-pane name="others">
          <template #label>
            <span class="tab-label">
              {{ t('serviceConfig.tabOthers') }}
              <span v-if="othersDirty" class="dirty-dot" />
            </span>
          </template>

          <div class="pane">
            <el-card shadow="never" class="config-card">
              <template #header>
                <div class="card-title">{{ t('serviceConfig.cardMultiplex') }}</div>
              </template>
              <el-alert
                type="warning"
                :closable="false"
                show-icon
                class="mp-alert"
                :title="t('serviceConfig.mpAlertTitle')"
                :description="t('serviceConfig.mpAlertDesc')"
              />
              <div class="form-row">
                <el-form-item :label="t('serviceConfig.keepaliveInterval')">
                  <el-input v-model="form.keepalive_interval" placeholder="10s" class="mono" />
                </el-form-item>
                <el-form-item :label="t('serviceConfig.keepaliveTimeout')">
                  <el-input v-model="form.keepalive_timeout" placeholder="30s" class="mono" />
                </el-form-item>
                <el-form-item :label="t('serviceConfig.streamWindow')">
                  <el-input-number v-model="form.max_stream_window_kb" :min="0" controls-position="right" style="width: 100%" />
                </el-form-item>
                <el-form-item :label="t('serviceConfig.acceptBacklog')">
                  <el-input-number v-model="form.accept_backlog" :min="0" controls-position="right" style="width: 100%" />
                </el-form-item>
                <el-form-item :label="t('serviceConfig.streamOpenTimeout')">
                  <el-input v-model="form.stream_open_timeout" placeholder="75s" class="mono" />
                </el-form-item>
              </div>
            </el-card>

            <div class="pane-grid pane-grid--side">
              <el-card shadow="never" class="config-card">
                <template #header>
                  <div class="card-title">{{ t('serviceConfig.cardLog') }}</div>
                </template>
                <div class="form-row">
                  <el-form-item :label="t('serviceConfig.logLevel')">
                    <el-select v-model="form.log_level">
                      <el-option label="debug" value="debug" />
                      <el-option label="info" value="info" />
                      <el-option label="warn" value="warn" />
                      <el-option label="error" value="error" />
                    </el-select>
                  </el-form-item>
                  <el-form-item :label="t('serviceConfig.flushInterval')">
                    <el-input v-model="form.traffic_flush_interval" placeholder="2s" class="mono" />
                  </el-form-item>
                </div>
                <div class="form-row">
                  <el-form-item :label="t('serviceConfig.rotateSize')">
                    <el-input-number v-model="form.log_rotate_max_size_mb" :min="1" controls-position="right" style="width: 100%" />
                  </el-form-item>
                  <el-form-item :label="t('serviceConfig.rotateBackups')">
                    <el-input-number v-model="form.log_rotate_max_backups" :min="0" controls-position="right" style="width: 100%" />
                  </el-form-item>
                  <el-form-item :label="t('serviceConfig.rotateAge')">
                    <el-input-number v-model="form.log_rotate_max_age_days" :min="0" controls-position="right" style="width: 100%" />
                  </el-form-item>
                </div>
                <p v-if="!form.log_to_file" class="field-tip">
                  {{ t('serviceConfig.logFileTip') }}
                </p>
              </el-card>

              <el-card shadow="never" class="config-card">
                <template #header>
                  <div class="card-title">{{ t('serviceConfig.cardMetrics') }}</div>
                </template>
                <el-form-item :label="t('serviceConfig.metricsEnabled')" class="switch-item">
                  <el-switch v-model="form.metrics_enabled" />
                </el-form-item>
                <p class="field-tip">{{ t('serviceConfig.metricsTip') }}</p>
                <div class="form-row">
                  <el-form-item :label="t('monitoring.interval')">
                    <el-input v-model="form.metrics_interval" placeholder="2s" class="mono" />
                  </el-form-item>
                  <el-form-item :label="t('monitoring.retention')">
                    <el-input v-model="form.metrics_retention" placeholder="1h" class="mono" />
                  </el-form-item>
                </div>
                <p class="field-tip">{{ t('monitoring.configHint') }}</p>
              </el-card>
            </div>
          </div>
        </el-tab-pane>
      </el-tabs>

      <div class="save-bar">
        <span class="faint">{{ dirty ? t('serviceConfig.dirty') : t('serviceConfig.clean') }}</span>
        <div class="spacer" />
        <el-button :icon="RefreshLeft" :disabled="!dirty" @click="reset">{{ t('common.undo') }}</el-button>
        <el-button type="primary" :loading="saving" :disabled="!dirty" @click="save">{{ t('serviceConfig.saveApply') }}</el-button>
      </div>
    </el-form>
  </div>
</template>

<style scoped>
.config-page {
  width: 100%;
  max-width: 1600px;
}

.config-form {
  display: flex;
  flex-direction: column;
  gap: 16px;
  padding-bottom: 8px;
}

.config-tabs :deep(.el-tabs__header) {
  margin-bottom: 16px;
}

.tab-label {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

/* 标签上的红点：该页存在未保存改动，切页也不丢线索 */
.dirty-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--el-color-danger);
}

.pane {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.pane-grid {
  display: grid;
  gap: 16px;
  align-items: start;
}

.pane-grid--two {
  grid-template-columns: repeat(2, minmax(0, 1fr));
}

.pane-grid--side {
  grid-template-columns: minmax(0, 1.6fr) minmax(300px, 1fr);
}

.config-card {
  min-width: 0;
}

@media (max-width: 1199px) {
  .pane-grid--two,
  .pane-grid--side {
    grid-template-columns: 1fr;
  }
}

.intro {
  margin: 0;
  padding: 0;
  border-top: none;
}

.client-intro {
  margin-bottom: -4px;
}

.config-card :deep(.el-card__body) {
  padding-top: 18px;
}

.card-title {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 14px;
  font-weight: 600;
  color: var(--el-text-color-primary);
}

.card-title::before {
  content: '';
  width: 3px;
  height: 14px;
  border-radius: 2px;
  background: var(--el-color-primary);
}

.field-tip {
  margin: -6px 0 14px;
  font-size: 12px;
  line-height: 1.75;
  color: var(--el-text-color-secondary);
}

.form-row {
  display: flex;
  flex-wrap: wrap;
  gap: 0 16px;
}

.form-row > .el-form-item {
  flex: 1 1 220px;
  max-width: 320px;
}

.form-row > .switch-item {
  flex: 0 0 auto;
  max-width: none;
}

.form-row .el-select {
  width: 100%;
}

.mp-alert {
  margin-bottom: 16px;
}

.card-head {
  display: flex;
  align-items: center;
  gap: 8px;
}

.code-area :deep(textarea) {
  font-family: var(--mono);
  font-size: 12px;
  line-height: 1.65;
}

/* 保存条吸附在内容区底部：长表单滚到哪都能提交 */
.save-bar {
  position: sticky;
  bottom: 0;
  z-index: 5;
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 4px;
  padding: 12px 16px;
  background: var(--el-bg-color);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 10px;
  box-shadow: 0 -6px 16px rgba(0, 0, 0, 0.05);
}
</style>
