<script setup lang="ts">
// 入站插件：启停、监听端口与 vless 的 TLS，参数写在持久存储里、重启后恢复。
import { onMounted, reactive, ref } from 'vue'
import { Refresh } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { Plugin } from '../api/types'
import { useI18n } from '../i18n'

const { t } = useI18n()

const plugins = ref<Plugin[]>([])
const loadError = ref('')
const loading = ref(false)

const dialog = ref<{ plugin: Plugin; mode: 'start' | 'port' | 'tls' } | null>(null)
const params = reactive<Record<string, string>>({})
const password = ref('')
const tlsEnabled = ref(false)
const tlsSNI = ref('')
const saving = ref(false)

async function load() {
  loading.value = true
  try {
    const data = await api.plugins()
    plugins.value = data.plugins
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : t('common.loadFailed')
  } finally {
    loading.value = false
  }
}

// 弹窗里的端口输入框按 slots 动态生成：插件有几个协议入口就有几行。
function openDialog(plugin: Plugin, mode: 'start' | 'port' | 'tls') {
  dialog.value = { plugin, mode }
  for (const k of Object.keys(params)) delete params[k]
  for (const slot of plugin.slots) params[slot.field] = String(slot.value)
  password.value = ''
  tlsEnabled.value = plugin.tls?.enabled ?? false
  tlsSNI.value = plugin.tls?.server_name ?? ''
}

async function submit() {
  const current = dialog.value
  if (!current) return
  saving.value = true
  try {
    if (current.mode === 'tls') {
      await api.pluginAction(current.plugin.name, 'tls', {
        enabled: tlsEnabled.value ? 'on' : 'off',
        sni: tlsSNI.value.trim(),
      })
    } else {
      const body: Record<string, string> = { ...params }
      if (current.plugin.basic_auth && password.value.trim()) {
        body.password = password.value
      }
      if (current.plugin.tls) {
        body.vless_tls = tlsEnabled.value ? 'on' : 'off'
        body.vless_tls_sni = tlsSNI.value.trim()
      }
      await api.pluginAction(current.plugin.name, current.mode, body)
    }
    dialog.value = null
    ElMessage.success(
      current.mode === 'start'
        ? t('plugins.enabledMsg')
        : current.mode === 'tls'
          ? t('plugins.tlsUpdated')
          : t('plugins.portUpdated'),
    )
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.opFailed'))
  } finally {
    saving.value = false
  }
}

// 端口 0 表示该协议还没配端口，列表里不显示占位标签。
function activePorts(plugin: Plugin) {
  return plugin.ports.filter((p) => p.port > 0)
}

async function stop(plugin: Plugin) {
  try {
    await ElMessageBox.confirm(
      t('plugins.stopConfirm', { name: plugin.name }),
      t('plugins.stopTitle'),
      {
        type: 'warning',
        confirmButtonText: t('plugins.stop'),
        cancelButtonText: t('common.cancel'),
      },
    )
  } catch {
    return
  }
  try {
    await api.pluginAction(plugin.name, 'stop')
    ElMessage.success(t('plugins.stoppedMsg'))
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('plugins.stopFailed'))
  }
}

onMounted(load)
</script>

<template>
  <el-card shadow="never" class="stack">
    <template #header>
      <div class="toolbar">
        <strong>{{ t('plugins.title') }}</strong>
        <div class="spacer" />
        <el-button :icon="Refresh" :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-table
      v-else
      v-loading="loading"
      :data="plugins"
      size="small"
      row-key="name"
      :empty-text="t('plugins.empty')"
    >
      <el-table-column :label="t('plugins.colPlugin')" min-width="200">
        <template #default="{ row }">
          <div class="plugin-name">{{ row.name }}</div>
          <div v-if="row.accepts_users" class="faint">{{ t('plugins.managed') }}</div>
        </template>
      </el-table-column>

      <el-table-column :label="t('plugins.colStatus')" width="104">
        <template #default="{ row }">
          <el-tag :type="row.enabled ? 'success' : 'info'" size="small" :effect="row.enabled ? 'light' : 'plain'">
            {{ row.enabled ? t('plugins.running') : t('plugins.stopped') }}
          </el-tag>
        </template>
      </el-table-column>

      <el-table-column :label="t('plugins.colPorts')" min-width="230">
        <template #default="{ row }">
          <div class="port-cell">
            <template v-if="activePorts(row).length">
              <el-tag
                v-for="kv in activePorts(row)"
                :key="kv.protocol"
                size="small"
                type="info"
                effect="plain"
              >
                {{ kv.protocol }} :{{ kv.port }}
              </el-tag>
            </template>
            <span v-else class="faint">{{ t('plugins.noPort') }}</span>
            <el-tag v-if="!row.enabled" size="small" type="warning" effect="plain">{{ t('plugins.pending') }}</el-tag>
          </div>
        </template>
      </el-table-column>

      <el-table-column :label="t('plugins.colTLS')" width="110">
        <template #default="{ row }">
          <template v-if="row.tls">
            <el-tag v-if="row.tls.enabled" size="small" type="success" effect="light">{{ t('plugins.tlsSelfSigned') }}</el-tag>
            <span v-else class="faint">{{ t('plugins.plaintext') }}</span>
          </template>
          <span v-else class="faint">—</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('plugins.colActions')" width="220" align="right" fixed="right">
        <template #default="{ row }">
          <template v-if="row.enabled">
            <el-button link type="danger" @click="stop(row)">{{ t('plugins.stop') }}</el-button>
            <el-button link type="primary" @click="openDialog(row, 'port')">{{ t('plugins.changePort') }}</el-button>
          </template>
          <el-button v-else link type="primary" @click="openDialog(row, 'start')">{{ t('plugins.enable') }}</el-button>
          <el-button v-if="row.tls" link type="primary" @click="openDialog(row, 'tls')">TLS</el-button>
        </template>
      </el-table-column>
    </el-table>
  </el-card>

  <el-dialog
    :model-value="!!dialog"
    :title="dialog ? `${dialog.plugin.name} · ${dialog.mode === 'start' ? t('plugins.enable') : dialog.mode === 'port' ? t('plugins.dialogPort') : 'TLS'}` : ''"
    width="min(480px, 94vw)"
    :close-on-click-modal="false"
    @update:model-value="dialog = null"
  >
    <template v-if="dialog">
      <p class="hint-block">
        {{ dialog.plugin.enabled ? t('plugins.hintRunning') : t('plugins.hintStopped') }}
      </p>
      <el-form label-position="top">
        <template v-if="dialog.mode !== 'tls'">
          <el-form-item v-for="slot in dialog.plugin.slots" :key="slot.field" :label="t('plugins.slotPort', { label: slot.label })">
            <el-input v-model="params[slot.field]" type="number" min="1" max="65535" />
          </el-form-item>
          <el-form-item v-if="dialog.plugin.basic_auth" :label="t('plugins.basicAuth')">
            <el-input v-model="password" type="password" show-password autocomplete="new-password" :placeholder="t('plugins.basicAuthPlaceholder')" />
          </el-form-item>
        </template>

        <template v-if="dialog.plugin.tls && dialog.mode !== 'port'">
          <el-divider content-position="left">{{ t('plugins.tlsSection') }}</el-divider>
          <el-form-item :label="t('plugins.enableTls')">
            <el-switch v-model="tlsEnabled" />
          </el-form-item>
          <el-form-item v-if="tlsEnabled" :label="t('plugins.sni')">
            <el-input v-model="tlsSNI" :placeholder="t('plugins.sniPlaceholder')" />
          </el-form-item>
          <el-alert
            v-if="tlsEnabled"
            type="warning"
            :closable="false"
            show-icon
            :title="t('plugins.tlsWarn')"
          />
          <p v-if="tlsEnabled && dialog.plugin.tls.fingerprint" class="hint-block" style="margin-top: 12px">
            {{ t('plugins.certInfo', { date: dialog.plugin.tls.not_after }) }}<br />
            {{ t('plugins.fingerprint', { value: dialog.plugin.tls.fingerprint }) }}
          </p>
        </template>
      </el-form>
    </template>
    <template #footer>
      <el-button @click="dialog = null">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" :loading="saving" @click="submit">
        {{ dialog?.mode === 'start' ? t('plugins.enable') : t('common.save') }}
      </el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.plugin-name {
  font-weight: 600;
  text-transform: lowercase;
}

.port-cell {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
}

.hint-block {
  margin-bottom: 14px;
  font-size: 12px;
  line-height: 1.7;
  color: var(--el-text-color-secondary);
}
</style>
