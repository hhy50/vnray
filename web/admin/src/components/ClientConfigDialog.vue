<script setup lang="ts">
// 客户端下发配置弹窗：从「客户端」列表就地打开，不再占用一个路由页面。
import { computed, ref, watch } from 'vue'
import { Delete, Plus, MagicStick, CopyDocument } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { ClientDetail, HostRow, IPMapRow } from '../api/types'
import { copyText, shortName } from '../lib/format'
import { useI18n } from '../i18n'

const { t } = useI18n()

const props = defineProps<{ modelValue: boolean; name: string }>()
const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  saved: []
}>()

const visible = computed({
  get: () => props.modelValue,
  set: (v: boolean) => emit('update:modelValue', v),
})

const detail = ref<ClientDetail | null>(null)
const loading = ref(false)
const loadError = ref('')
const tab = ref('config')

const logLevel = ref('info')
const serverConfig = ref('{}')
const hosts = ref<HostRow[]>([])
const ipMaps = ref<IPMapRow[]>([])
const concurrency = ref(0)
const saving = ref('')

const logLevels = [
  { value: 'info', label: 'info' },
  { value: 'debug', label: 'debug' },
  { value: 'warn', label: 'warn' },
  { value: 'error', label: 'error' },
]

// server_config 是自由 JSON，键由服务端定义；这里只保证展示时是排版开的多行。
function prettyJson(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}

async function load() {
  if (!props.name) return
  loading.value = true
  try {
    const data = await api.clientDetail(props.name)
    detail.value = data
    logLevel.value = data.log_level || 'info'
    serverConfig.value = prettyJson(data.server_config || '{}')
    hosts.value = data.hosts.map((h) => ({ ...h }))
    ipMaps.value = data.ip_maps.map((m) => ({ ...m }))
    concurrency.value = data.concurrency
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : t('common.loadFailed')
  } finally {
    loading.value = false
  }
}

// 每次打开都重新取一遍：列表是 5 秒轮询的，缓存的详情可能已经过期。
watch(
  () => [props.modelValue, props.name],
  ([open]) => {
    if (open) {
      tab.value = 'config'
      detail.value = null
      void load()
    }
  },
  { immediate: true },
)

function formatJson() {
  try {
    serverConfig.value = JSON.stringify(JSON.parse(serverConfig.value), null, 2)
    ElMessage.success(t('clientDialog.formatted'))
  } catch {
    ElMessage.error(t('clientDialog.invalidJson'))
  }
}

// 失焦时静默排版一次：粘进来的压缩 JSON 也能变成缩进格式。
function tidyJson() {
  serverConfig.value = prettyJson(serverConfig.value)
}

async function saveConfig() {
  saving.value = 'config'
  try {
    await api.clientConfigSave(props.name, { config: serverConfig.value, log_level: logLevel.value })
    // 并发上限与 Host/映射共用下发接口；从本 tab 保存时一并提交，避免两处各存一半。
    const res = await api.clientHostsSave(props.name, {
      hosts: hosts.value.filter((h) => h.domain || h.ip),
      ip_maps: ipMaps.value.filter((m) => m.from || m.to),
      concurrency: concurrency.value,
    })
    if (res.pushed) ElMessage.success(t('clientDialog.pushed'))
    else ElMessage.warning(t('clientDialog.offlineWarn'))
    // 保存成功即收起弹窗，列表由父组件刷新，避免停在弹窗上看旧值。
    visible.value = false
    emit('saved')
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.saveFailed'))
  } finally {
    saving.value = ''
  }
}

async function saveHosts() {
  saving.value = 'hosts'
  try {
    const res = await api.clientHostsSave(props.name, {
      hosts: hosts.value.filter((h) => h.domain || h.ip),
      ip_maps: ipMaps.value.filter((m) => m.from || m.to),
      concurrency: concurrency.value,
    })
    if (res.pushed) {
      ElMessage.success(t('clientDialog.pushed'))
    } else {
      ElMessage.warning(t('clientDialog.offlineWarn'))
    }
    visible.value = false
    emit('saved')
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.saveFailed'))
  } finally {
    saving.value = ''
  }
}

async function copyReported() {
  if (!detail.value?.reported_json) return
  if (await copyText(detail.value.reported_json)) ElMessage.success(t('common.copied'))
  else ElMessage.error(t('common.copyFailed'))
}

const isConfigTab = computed(() => tab.value === 'config')
</script>

<template>
  <el-dialog
    v-model="visible"
    width="min(960px, 94vw)"
    top="6vh"
    :close-on-click-modal="false"
    destroy-on-close
    class="client-config-dialog"
  >
    <template #header>
      <div class="dialog-head">
        <div class="head-line">
          <span class="dialog-title">{{ shortName(name) }}</span>
          <el-tag v-if="detail" size="small" :type="detail.online ? 'success' : 'info'" effect="plain">
            {{ detail.online ? t('common.online') : t('common.offline') }}
          </el-tag>
          <span v-if="detail?.remote" class="mono faint">{{ detail.remote }}</span>
        </div>
        <div class="head-sub faint" :title="name">
          <span class="mono">{{ name }}</span>
          <template v-if="detail?.has_stored"> · {{ t('clientDialog.recentlyUpdated', { value: detail.updated_at }) }}</template>
        </div>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />
    <el-skeleton v-else-if="loading && !detail" :rows="6" animated />

    <el-tabs v-else-if="detail" v-model="tab" class="cfg-tabs">
      <el-tab-pane :label="t('clientDialog.tabConfig')" name="config">
        <el-alert
          v-if="detail.stored_raw"
          :title="t('clientDialog.invalidJsonWarn')"
          type="warning"
          :closable="false"
          show-icon
          style="margin-bottom: 12px"
        />

        <el-form label-position="top">
          <el-form-item :label="t('clientDialog.logLevelLabel')">
            <el-select v-model="logLevel" style="width: 12rem">
              <el-option
                v-for="l in logLevels"
                :key="l.value"
                :label="l.value === 'info' ? t('users.logLevelDefault') : l.label"
                :value="l.value"
              />
            </el-select>
          </el-form-item>

          <el-form-item :label="t('clientDialog.concurrencyLabel')">
            <el-input-number v-model="concurrency" :min="0" controls-position="right" style="width: 12rem" />
          </el-form-item>

          <div class="json-head">
            <span class="json-title">server_config（JSON）</span>
            <div class="spacer" />
            <el-button link :icon="MagicStick" @click="formatJson">{{ t('clientDialog.format') }}</el-button>
          </div>

          <el-input
            v-model="serverConfig"
            type="textarea"
            :rows="14"
            class="code-area"
            spellcheck="false"
            @blur="tidyJson"
          />

          <p class="page-hint">
            {{ t('clientDialog.configHint') }}
          </p>
        </el-form>
      </el-tab-pane>

      <el-tab-pane :label="t('clientDialog.tabHosts')" name="hosts">
        <p class="page-hint">
          {{ t('clientDialog.hostsHint') }}
        </p>
        <div class="section">
          <div class="section-head">
            <span class="muted">{{ t('clientDialog.hostsSection') }}</span>
            <div class="spacer" />
            <el-button size="small" :icon="Plus" @click="hosts.push({ domain: '', ip: '' })">
              {{ t('clientDialog.addRecord') }}
            </el-button>
          </div>
          <el-table :data="hosts" size="small" :empty-text="t('clientDialog.hostsEmpty')">
            <el-table-column :label="t('clientConfig.colDomain')" min-width="220">
              <template #default="{ row }">
                <el-input v-model="row.domain" placeholder="example.com" />
              </template>
            </el-table-column>
            <el-table-column :label="t('clientConfig.colFixedIP')" min-width="180">
              <template #default="{ row }">
                <el-input v-model="row.ip" placeholder="192.168.1.10" class="mono" />
              </template>
            </el-table-column>
            <el-table-column width="56" align="right">
              <template #default="{ $index }">
                <el-button link type="danger" :icon="Delete" @click="hosts.splice($index, 1)" />
              </template>
            </el-table-column>
          </el-table>
        </div>

        <div class="section">
          <div class="section-head">
            <span class="muted">{{ t('clientDialog.mapsSection') }}</span>
            <div class="spacer" />
            <el-button size="small" :icon="Plus" @click="ipMaps.push({ from: '', to: '' })">
              {{ t('clientConfig.addMap') }}
            </el-button>
          </div>
          <el-table :data="ipMaps" size="small" :empty-text="t('clientDialog.mapsEmpty')">
            <el-table-column :label="t('clientConfig.colFrom')" min-width="200">
              <template #default="{ row }">
                <el-input v-model="row.from" placeholder="123.123.*.*" class="mono" />
              </template>
            </el-table-column>
            <el-table-column :label="t('clientConfig.colTo')" min-width="200">
              <template #default="{ row }">
                <el-input v-model="row.to" placeholder="192.168.*.*" class="mono" />
              </template>
            </el-table-column>
            <el-table-column width="56" align="right">
              <template #default="{ $index }">
                <el-button link type="danger" :icon="Delete" @click="ipMaps.splice($index, 1)" />
              </template>
            </el-table-column>
          </el-table>
        </div>

        <p class="page-hint">
          {{ t('clientDialog.hostsTip') }}
        </p>
      </el-tab-pane>

      <el-tab-pane :label="t('clientDialog.tabReported')" name="reported">
        <div v-if="detail.reported_json" class="section">
          <div class="section-head">
            <span class="muted">{{ t('clientDialog.reportedSection') }}</span>
            <div class="spacer" />
            <el-button size="small" :icon="CopyDocument" @click="copyReported">{{ t('common.copy') }}</el-button>
          </div>
          <pre class="log-view">{{ detail.reported_json }}</pre>
        </div>
        <el-empty
          v-else
          :description="t('clientDialog.reportedEmpty')"
          :image-size="72"
        />
      </el-tab-pane>
    </el-tabs>

    <template #footer>
      <div class="dialog-footer">
        <span class="faint">
          {{ tab === 'reported' ? t('clientDialog.footerReported') : isConfigTab ? t('clientDialog.footerConfig') : t('clientDialog.footerHosts') }}
        </span>
        <div class="spacer" />
        <el-button @click="visible = false">{{ t('common.close') }}</el-button>
        <el-button
          v-if="detail && tab !== 'reported'"
          type="primary"
          :loading="!!saving"
          @click="isConfigTab ? saveConfig() : saveHosts()"
        >
          {{ isConfigTab ? t('clientDialog.saveConfig') : t('clientDialog.savePush') }}
        </el-button>
      </div>
    </template>
  </el-dialog>
</template>

<style scoped>
.dialog-head {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding-right: 24px;
}

.head-line {
  display: flex;
  align-items: center;
  gap: 8px;
}

.dialog-title {
  font-size: 16px;
  font-weight: 600;
  color: var(--el-text-color-primary);
}

.head-sub {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.cfg-tabs :deep(.el-tabs__header) {
  margin-bottom: 14px;
}

.json-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}

.json-title {
  font-size: 14px;
  color: var(--el-text-color-regular);
}

.code-area :deep(textarea) {
  font-family: var(--mono);
  font-size: 12px;
  line-height: 1.65;
}

.section {
  margin-bottom: 18px;
}

.dialog-footer {
  display: flex;
  align-items: center;
  gap: 8px;
}
</style>

<style>
/* 弹窗挂在 body 上，scoped 选择器够不到内部容器 */
.client-config-dialog .el-dialog__body {
  max-height: 68vh;
  overflow-y: auto;
  padding-top: 8px;
}
</style>
