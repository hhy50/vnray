<script setup lang="ts">
// 入站插件：启停、监听端口与 vless 的 TLS，参数写在持久存储里、重启后恢复。
import { onMounted, reactive, ref } from 'vue'
import { Refresh } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { Plugin } from '../api/types'

const plugins = ref<Plugin[]>([])
const serverPort = ref(0)
const loadError = ref('')
const loading = ref(false)

const dialog = ref<{ plugin: Plugin; mode: 'start' | 'port' | 'tls' } | null>(null)
const params = reactive<Record<string, string>>({})
const tlsEnabled = ref(false)
const tlsSNI = ref('')
const saving = ref(false)

async function load() {
  loading.value = true
  try {
    const data = await api.plugins()
    plugins.value = data.plugins
    serverPort.value = data.server_port
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载失败'
  } finally {
    loading.value = false
  }
}

// 弹窗里的端口输入框按 slots 动态生成：插件有几个协议入口就有几行。
function openDialog(plugin: Plugin, mode: 'start' | 'port' | 'tls') {
  dialog.value = { plugin, mode }
  for (const k of Object.keys(params)) delete params[k]
  for (const slot of plugin.slots) params[slot.field] = String(slot.value)
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
      if (current.plugin.tls) {
        body.vless_tls = tlsEnabled.value ? 'on' : 'off'
        body.vless_tls_sni = tlsSNI.value.trim()
      }
      await api.pluginAction(current.plugin.name, current.mode, body)
    }
    dialog.value = null
    ElMessage.success(
      current.mode === 'start' ? '已启用' : current.mode === 'tls' ? 'TLS 已更新' : '端口已更新',
    )
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '操作失败')
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
    await ElMessageBox.confirm(`停用插件 ${plugin.name}？已接入的会话不受影响。`, '停用插件', {
      type: 'warning',
      confirmButtonText: '停用',
      cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api.pluginAction(plugin.name, 'stop')
    ElMessage.success('已停用')
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '停用失败')
  }
}

onMounted(load)
</script>

<template>
  <el-card shadow="never" class="stack">
    <template #header>
      <div class="toolbar">
        <strong>入站插件</strong>
        <span class="faint">启停与参数写穿到持久存储</span>
        <div class="spacer" />
        <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-table
      v-else
      v-loading="loading"
      :data="plugins"
      size="small"
      row-key="name"
      empty-text="未配置插件管理器。"
    >
      <el-table-column label="插件" min-width="200">
        <template #default="{ row }">
          <div class="plugin-name">{{ row.name }}</div>
          <div v-if="row.accepts_users" class="faint">托管账号，在「用户」页维护</div>
        </template>
      </el-table-column>

      <el-table-column label="状态" width="104">
        <template #default="{ row }">
          <el-tag :type="row.enabled ? 'success' : 'info'" size="small" :effect="row.enabled ? 'light' : 'plain'">
            {{ row.enabled ? '运行中' : '已停止' }}
          </el-tag>
        </template>
      </el-table-column>

      <el-table-column label="监听端口" min-width="230">
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
            <span v-else class="faint">未设置端口</span>
            <el-tag v-if="!row.enabled" size="small" type="warning" effect="plain">待启用</el-tag>
          </div>
        </template>
      </el-table-column>

      <el-table-column label="传输加密" width="110">
        <template #default="{ row }">
          <template v-if="row.tls">
            <el-tag v-if="row.tls.enabled" size="small" type="success" effect="light">TLS 自签</el-tag>
            <span v-else class="faint">明文</span>
          </template>
          <span v-else class="faint">—</span>
        </template>
      </el-table-column>

      <el-table-column label="操作" width="220" align="right" fixed="right">
        <template #default="{ row }">
          <template v-if="row.enabled">
            <el-button link type="danger" @click="stop(row)">停用</el-button>
            <el-button link type="primary" @click="openDialog(row, 'port')">修改端口</el-button>
          </template>
          <el-button v-else link type="primary" @click="openDialog(row, 'start')">启用</el-button>
          <el-button v-if="row.tls" link type="primary" @click="openDialog(row, 'tls')">TLS</el-button>
        </template>
      </el-table-column>
    </el-table>

    <p class="page-hint">
      入站插件在本机开监听端口，把进来的流量按路由规则送进转发链路。
      端口默认取服务端监听端口 + 1（当前 {{ serverPort || '未知' }}），需避开数据面与端口映射占用的端口。
    </p>
  </el-card>

  <el-dialog
    :model-value="!!dialog"
    :title="dialog ? `${dialog.plugin.name} · ${dialog.mode === 'start' ? '启用' : dialog.mode === 'port' ? '监听端口' : 'TLS'}` : ''"
    width="min(480px, 94vw)"
    :close-on-click-modal="false"
    @update:model-value="dialog = null"
  >
    <template v-if="dialog">
      <p class="hint-block">
        {{ dialog.plugin.enabled ? '当前运行中，保存后热切换；旧连接不受影响。' : '当前已停止，保存后立即按该端口启用。' }}
      </p>
      <el-form label-position="top">
        <template v-if="dialog.mode !== 'tls'">
          <el-form-item v-for="slot in dialog.plugin.slots" :key="slot.field" :label="`${slot.label} 端口`">
            <el-input v-model="params[slot.field]" type="number" min="1" max="65535" />
          </el-form-item>
        </template>

        <template v-if="dialog.plugin.tls && dialog.mode !== 'port'">
          <el-divider content-position="left">VLESS TLS（自签证书）</el-divider>
          <el-form-item label="启用 TLS">
            <el-switch v-model="tlsEnabled" />
          </el-form-item>
          <el-form-item v-if="tlsEnabled" label="证书域名（客户端 SNI）">
            <el-input v-model="tlsSNI" placeholder="留空则用 vnray.local" />
          </el-form-item>
          <el-alert
            v-if="tlsEnabled"
            type="warning"
            :closable="false"
            show-icon
            title="自签证书需要客户端关闭证书校验：分享链接会自动带 allowInsecure=1。"
          />
          <p v-if="tlsEnabled && dialog.plugin.tls.fingerprint" class="hint-block" style="margin-top: 12px">
            证书存于服务端配置（vn_service_settings），到期 {{ dialog.plugin.tls.not_after }}<br />
            指纹 SHA-256：{{ dialog.plugin.tls.fingerprint }}
          </p>
        </template>
      </el-form>
    </template>
    <template #footer>
      <el-button @click="dialog = null">取消</el-button>
      <el-button type="primary" :loading="saving" @click="submit">
        {{ dialog?.mode === 'start' ? '启用' : '保存' }}
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
