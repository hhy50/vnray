<script setup lang="ts">
// 端口映射：服务端远程端口 → 客户端本地目标，保存即热更新监听器。
import { onMounted, reactive, ref } from 'vue'
import { Plus, Refresh } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { PortMap } from '../api/types'
import { rate, shortName } from '../lib/format'
import { useI18n } from '../i18n'

const { t } = useI18n()

const rows = ref<PortMap[]>([])
const clients = ref<{ name: string; label: string }[]>([])
const loadError = ref('')
const loading = ref(false)

const editing = ref<number | undefined>(undefined)
const showForm = ref(false)
const saving = ref(false)
const advancedOpen = ref<string[]>([])
const form = reactive({
  client: '',
  remote_port: 0,
  local_host: '127.0.0.1',
  local_port: 0,
  limit_up_kb: 0,
  limit_down_kb: 0,
  tls_enabled: false,
  cert_pem: '',
  key_pem: '',
  remark: '',
  enabled: true,
})

async function load() {
  loading.value = true
  try {
    const data = await api.portmaps()
    rows.value = data.rows
    clients.value = data.clients
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : t('common.loadFailed')
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = undefined
  form.client = ''
  form.remote_port = 0
  form.local_host = '127.0.0.1'
  form.local_port = 0
  form.limit_up_kb = 0
  form.limit_down_kb = 0
  form.tls_enabled = false
  form.cert_pem = ''
  form.key_pem = ''
  advancedOpen.value = []
  form.remark = ''
  form.enabled = true
  showForm.value = true
}

function openEdit(m: PortMap) {
  editing.value = m.id
  form.client = m.client
  form.remote_port = m.remote_port
  form.local_host = m.local_host
  form.local_port = m.local_port
  form.limit_up_kb = m.limit_up_kb
  form.limit_down_kb = m.limit_down_kb
  form.tls_enabled = m.tls_enabled
  form.cert_pem = m.cert_pem ?? ''
  form.key_pem = m.key_pem ?? ''
  advancedOpen.value = m.tls_enabled ? ['advanced'] : []
  form.remark = m.remark
  form.enabled = m.enabled
  showForm.value = true
}

async function save() {
  if (!form.client) {
    ElMessage.warning(t('portmaps.needClient'))
    return
  }
  if (!form.remote_port || !form.local_port) {
    ElMessage.warning(t('portmaps.needPorts'))
    return
  }
  if (form.tls_enabled && (!editing.value || form.cert_pem || form.key_pem) && (!form.cert_pem || !form.key_pem)) {
    ElMessage.warning(editing.value ? t('portmaps.needCertChange') : t('portmaps.needCert'))
    advancedOpen.value = ['advanced']
    return
  }
  if (!form.tls_enabled) {
    form.cert_pem = ''
    form.key_pem = ''
  }
  saving.value = true
  try {
    await api.portMapSave({ ...form }, editing.value)
    showForm.value = false
    ElMessage.success(t('portmaps.saved'))
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.saveFailed'))
  } finally {
    saving.value = false
  }
}

async function toggle(m: PortMap) {
  try {
    await api.portMapToggle(m.id)
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.opFailed'))
  }
}

async function remove(m: PortMap) {
  try {
    await ElMessageBox.confirm(
      t('portmaps.deleteConfirm', { remote: m.remote_port, local: `${m.local_host}:${m.local_port}` }),
      t('portmaps.deleteTitle'),
      { type: 'warning', confirmButtonText: t('common.delete'), cancelButtonText: t('common.cancel') },
    )
  } catch {
    return
  }
  try {
    await api.portMapDelete(m.id)
    ElMessage.success(t('common.deleted'))
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.deleteFailed'))
  }
}

onMounted(load)
</script>

<template>
  <el-card shadow="never" class="stack">
    <template #header>
      <div class="toolbar">
        <strong>{{ t('portmaps.count', { n: rows.length }) }}</strong>
        <div class="spacer" />
        <el-button :icon="Refresh" :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
        <el-button type="primary" :icon="Plus" @click="openCreate">{{ t('portmaps.add') }}</el-button>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-table
      v-else
      v-loading="loading"
      :data="rows"
      size="small"
      row-key="id"
      :empty-text="t('portmaps.empty')"
    >
      <el-table-column :label="t('portmaps.colRemote')" width="92">
        <template #default="{ row }"><span class="mono port">{{ row.remote_port }}</span></template>
      </el-table-column>

      <el-table-column :label="t('portmaps.colClient')" min-width="168">
        <template #default="{ row }">
          <div class="client-cell" :title="row.client">
            <span class="dot" :class="row.client_online ? 'on' : 'off'" />
            <span class="mono ellipsis">{{ shortName(row.client) }}</span>
          </div>
        </template>
      </el-table-column>

      <el-table-column :label="t('portmaps.colLocal')" min-width="176">
        <template #default="{ row }">
          <span class="mono">{{ row.local_host }}:{{ row.local_port }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('portmaps.colLimits')" width="112">
        <template #default="{ row }">
          <span class="faint">{{ rate(row.limit_up_kb) }} / {{ rate(row.limit_down_kb) }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('portmaps.colStatus')" width="126">
        <template #default="{ row }">
          <el-tag v-if="!row.enabled" size="small" type="info" effect="plain">{{ t('portmaps.disabledTag') }}</el-tag>
          <el-tag v-else-if="row.listening" size="small" type="success">{{ t('portmaps.running') }}</el-tag>
          <el-tooltip v-else :content="t('portmaps.notListeningTip')" placement="top">
            <el-tag size="small" type="danger" effect="plain">{{ t('portmaps.notListening') }}</el-tag>
          </el-tooltip>
        </template>
      </el-table-column>

      <el-table-column :label="t('portmaps.colRemark')" min-width="130" show-overflow-tooltip>
        <template #default="{ row }"><span class="faint">{{ row.remark || '—' }}</span></template>
      </el-table-column>

      <el-table-column :label="t('portmaps.colUpdated')" width="146">
        <template #default="{ row }"><span class="faint">{{ row.updated_at }}</span></template>
      </el-table-column>

      <el-table-column :label="t('portmaps.colActions')" width="156" align="right" fixed="right">
        <template #default="{ row }">
          <el-button link type="primary" @click="openEdit(row)">{{ t('common.edit') }}</el-button>
          <el-button link @click="toggle(row)">{{ row.enabled ? t('common.off') : t('common.on') }}</el-button>
          <el-button link type="danger" @click="remove(row)">{{ t('common.delete') }}</el-button>
        </template>
      </el-table-column>
    </el-table>
  </el-card>

  <el-dialog
    v-model="showForm"
    :title="editing ? t('portmaps.editTitle') : t('portmaps.addTitle')"
    width="min(600px, 94vw)"
    :close-on-click-modal="false"
  >
    <el-form label-position="top">
      <div class="form-row">
        <el-form-item :label="t('portmaps.remotePort')" style="flex: 1">
          <el-input-number
            v-model="form.remote_port"
            :min="1"
            :max="65535"
            controls-position="right"
            style="width: 100%"
          />
        </el-form-item>
        <el-form-item :label="t('portmaps.localPort')" style="flex: 1">
          <el-input-number
            v-model="form.local_port"
            :min="1"
            :max="65535"
            controls-position="right"
            style="width: 100%"
          />
        </el-form-item>
      </div>
      <el-form-item :label="t('portmaps.colClient')">
        <el-select v-model="form.client" filterable allow-create default-first-option placeholder="namespace:group:name">
          <el-option v-for="c in clients" :key="c.name" :label="c.label" :value="c.name" />
        </el-select>
      </el-form-item>
      <el-form-item :label="t('portmaps.localHost')">
        <el-input v-model="form.local_host" class="mono" />
      </el-form-item>
      <div class="form-row">
        <el-form-item :label="t('portmaps.upLimit')" style="flex: 1">
          <el-input-number v-model="form.limit_up_kb" :min="0" controls-position="right" style="width: 100%" />
        </el-form-item>
        <el-form-item :label="t('portmaps.downLimit')" style="flex: 1">
          <el-input-number v-model="form.limit_down_kb" :min="0" controls-position="right" style="width: 100%" />
        </el-form-item>
      </div>
      <el-form-item :label="t('portmaps.remark')">
        <el-input v-model="form.remark" :placeholder="t('portmaps.remarkPlaceholder')" />
      </el-form-item>
      <el-collapse v-model="advancedOpen" class="advanced-options">
        <el-collapse-item name="advanced">
          <template #title>
            <span>{{ t('portmaps.advanced') }}</span>
            <el-tag v-if="form.tls_enabled" size="small" type="success" effect="plain" class="advanced-status">{{ t('portmaps.httpsOn') }}</el-tag>
          </template>
          <el-form-item>
            <div class="tls-row">
              <span class="tls-label">HTTPS / TLS</span>
              <el-switch v-model="form.tls_enabled" />
            </div>
          </el-form-item>
          <template v-if="form.tls_enabled">
            <el-form-item :label="t('portmaps.certLabel')">
              <el-input v-model="form.cert_pem" type="textarea" :rows="5" class="mono" placeholder="-----BEGIN CERTIFICATE-----" />
            </el-form-item>
            <el-form-item :label="t('portmaps.keyLabel')">
              <el-input v-model="form.key_pem" type="textarea" :rows="5" class="mono" placeholder="-----BEGIN PRIVATE KEY-----" />
            </el-form-item>
          </template>
        </el-collapse-item>
      </el-collapse>
    </el-form>
    <template #footer>
      <el-button @click="showForm = false">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" :loading="saving" @click="save">{{ t('common.save') }}</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.port {
  font-size: 13px;
  font-weight: 600;
  color: var(--el-color-primary);
}

.client-cell {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}

.dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  flex-shrink: 0;
}

.dot.on {
  background: var(--el-color-success);
}

.dot.off {
  background: var(--el-border-color-darker);
}

.ellipsis {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.form-row {
  display: flex;
  gap: 12px;
  flex-wrap: wrap;
}

.advanced-options {
  margin-top: 8px;
  border-top: 1px solid var(--el-border-color-lighter);
  border-bottom: 0;
}

.advanced-status {
  margin-left: 8px;
}

.tls-row {
  display: flex;
  align-items: center;
  gap: 10px;
}

.tls-label {
  color: var(--el-text-color-regular);
}

.field-hint {
  margin-top: 6px;
  color: var(--el-text-color-secondary);
  font-size: 12px;
  line-height: 1.5;
}
</style>
