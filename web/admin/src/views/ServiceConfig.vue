<script setup lang="ts">
// 服务端配置：只收运行期可热生效的参数，保存即落库；需要重启的项留在配置文件里。
import { computed, onMounted, ref } from 'vue'
import { RefreshLeft } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { ServiceConfig } from '../api/types'
import { useI18n } from '../i18n'

const { t } = useI18n()

const form = ref<ServiceConfig | null>(null)
const loadError = ref('')
const loading = ref(false)
const saving = ref(false)

// 快照用来判断"有没有改过"：只有脏表单才允许保存，避免误触把默认值写回库。
const snapshot = ref('')

const dirty = computed(() => !!form.value && JSON.stringify(form.value) !== snapshot.value)

async function load() {
  loading.value = true
  try {
    const data = await api.serviceConfig()
    form.value = data
    snapshot.value = JSON.stringify(data)
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
    await api.serviceConfigSave(form.value)
    snapshot.value = JSON.stringify(form.value)
    ElMessage.success(t('serviceConfig.saved'))
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.saveFailed'))
  } finally {
    saving.value = false
  }
}

function reset() {
  if (!snapshot.value) return
  form.value = JSON.parse(snapshot.value) as ServiceConfig
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

      <div class="config-layout">
        <div class="config-column config-column--main">
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
        </div>

        <div class="config-column config-column--side">
          <el-card shadow="never" class="config-card">
            <template #header>
              <div class="card-title">{{ t('serviceConfig.cardPublicHost') }}</div>
            </template>
            <el-form-item :label="t('serviceConfig.publicHostLabel')" class="w-md">
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
                <el-input v-model="form.traffic_flush_interval" placeholder="10s" class="mono" />
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
        </div>
      </div>

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

.config-layout {
  display: grid;
  grid-template-columns: minmax(0, 1.45fr) minmax(360px, 0.95fr);
  gap: 16px;
  align-items: start;
}

.config-column {
  display: flex;
  flex-direction: column;
  gap: 16px;
  min-width: 0;
}

@media (max-width: 1199px) {
  .config-layout {
    grid-template-columns: 1fr;
  }
}

.intro {
  margin: 0;
  padding: 0;
  border-top: none;
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

.w-md {
  max-width: 420px;
}

.mp-alert {
  margin-bottom: 16px;
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
