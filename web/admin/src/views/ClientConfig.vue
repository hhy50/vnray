<script setup lang="ts">
// 客户端配置：只维护下发给所有远程客户端的全局 Host 与 IP 段规则。
// 单台客户端的独立规则在「客户端」页的弹窗里编辑，两者不共用保存按钮。
import { computed, onMounted, ref } from 'vue'
import { Delete, Plus, RefreshLeft } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { GlobalClientConfig } from '../api/types'
import { useI18n } from '../i18n'

const { t } = useI18n()

const form = ref<GlobalClientConfig | null>(null)
const loadError = ref('')
const loading = ref(false)
const saving = ref(false)

// 快照用来判断"有没有改过"：只有脏表单才允许保存，避免误触把全局规则整体覆盖。
const snapshot = ref('')

const dirty = computed(() => !!form.value && JSON.stringify(form.value) !== snapshot.value)

const hostCount = computed(() => form.value?.custom_hosts.length ?? 0)
const mapCount = computed(() => form.value?.ip_maps.length ?? 0)

async function load() {
  loading.value = true
  try {
    const data = await api.clientConfig()
    form.value = data.global_client
    snapshot.value = JSON.stringify(data.global_client)
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
    await api.clientConfigSave({ global_client: form.value })
    snapshot.value = JSON.stringify(form.value)
    ElMessage.success(t('clientConfig.saved'))
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.saveFailed'))
  } finally {
    saving.value = false
  }
}

function reset() {
  if (!snapshot.value) return
  form.value = JSON.parse(snapshot.value) as GlobalClientConfig
}

onMounted(load)
</script>

<template>
  <div class="client-config-page">
    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-form
      v-else-if="form"
      v-loading="loading"
      class="config-form"
      label-position="top"
      @submit.prevent="save"
    >
      <p class="page-hint intro">
        {{ t('clientConfig.intro') }}
      </p>

      <div class="config-layout">
        <el-card shadow="never" class="config-card">
          <template #header>
            <div class="card-head">
              <span class="card-title">{{ t('clientConfig.cardHosts') }}</span>
            <span class="faint">{{ t('clientConfig.count', { n: hostCount }) }}</span>
            <div class="spacer" />
            <el-button size="small" :icon="Plus" @click="form.custom_hosts.push({ domain: '', ip: '' })">
              {{ t('clientConfig.addHost') }}
            </el-button>
          </div>
        </template>
        <el-table :data="form.custom_hosts" size="small" :empty-text="t('clientConfig.hostsEmpty')">
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
                @click="form.custom_hosts.splice($index, 1)"
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
            <span class="faint">{{ t('clientConfig.count', { n: mapCount }) }}</span>
            <div class="spacer" />
            <el-button size="small" :icon="Plus" @click="form.ip_maps.push({ from: '', to: '' })">
              {{ t('clientConfig.addMap') }}
            </el-button>
          </div>
        </template>
        <el-table :data="form.ip_maps" size="small" :empty-text="t('clientConfig.mapsEmpty')">
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
                @click="form.ip_maps.splice($index, 1)"
              />
            </template>
          </el-table-column>
        </el-table>
        <p class="field-tip">
          {{ t('clientConfig.mapsTipBefore') }}<span class="mono">*</span>{{ t('clientConfig.mapsTipMiddle') }}<span class="mono">*</span>{{ t('clientConfig.mapsTipAfter') }}
        </p>
        </el-card>
      </div>

      <div class="save-bar">
        <span class="faint">{{ dirty ? t('serviceConfig.dirty') : t('serviceConfig.clean') }}</span>
        <div class="spacer" />
        <el-button :icon="RefreshLeft" :disabled="!dirty" @click="reset">{{ t('common.undo') }}</el-button>
        <el-button type="primary" :loading="saving" :disabled="!dirty" @click="save">
          {{ t('clientConfig.savePush') }}
        </el-button>
      </div>
    </el-form>
  </div>
</template>

<style scoped>
.client-config-page {
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
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 16px;
  align-items: start;
}

.config-card {
  min-width: 0;
}

@media (max-width: 1439px) {
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
  padding-top: 12px;
}

.card-head {
  display: flex;
  align-items: center;
  gap: 8px;
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
  margin: 12px 0 0;
  font-size: 12px;
  line-height: 1.75;
  color: var(--el-text-color-secondary);
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
