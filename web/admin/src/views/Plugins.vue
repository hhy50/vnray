<script setup lang="ts">
// 入站插件：启停与监听端口，端口写在持久存储里、重启后恢复。
import { onMounted, reactive, ref } from 'vue'
import { Refresh } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { Plugin } from '../api/types'

const plugins = ref<Plugin[]>([])
const serverPort = ref(0)
const loadError = ref('')
const loading = ref(false)

const dialog = ref<{ plugin: Plugin; mode: 'start' | 'port' } | null>(null)
const params = reactive<Record<string, string>>({})
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
function openDialog(plugin: Plugin, mode: 'start' | 'port') {
  dialog.value = { plugin, mode }
  for (const k of Object.keys(params)) delete params[k]
  for (const slot of plugin.slots) params[slot.field] = String(slot.value)
}

async function submit() {
  const current = dialog.value
  if (!current) return
  saving.value = true
  try {
    await api.pluginAction(current.plugin.name, current.mode, { ...params })
    dialog.value = null
    ElMessage.success(current.mode === 'start' ? '已启用' : '端口已更新')
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

async function stop(plugin: Plugin) {  try {
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

      <el-table-column label="操作" width="180" align="right" fixed="right">
        <template #default="{ row }">
          <template v-if="row.enabled">
            <el-button link type="danger" @click="stop(row)">停用</el-button>
            <el-button link type="primary" @click="openDialog(row, 'port')">修改端口</el-button>
          </template>
          <el-button v-else link type="primary" @click="openDialog(row, 'start')">启用</el-button>
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
    :title="dialog ? `${dialog.plugin.name} · ${dialog.mode === 'start' ? '启用' : '监听端口'}` : ''"
    width="min(460px, 94vw)"
    :close-on-click-modal="false"
    @update:model-value="dialog = null"
  >
    <template v-if="dialog">
      <p class="hint-block">
        {{ dialog.plugin.enabled ? '当前运行中，保存后热切换；旧连接不受影响。' : '当前已停止，保存后立即按该端口启用。' }}
      </p>
      <el-form label-position="top">
        <el-form-item v-for="slot in dialog.plugin.slots" :key="slot.field" :label="`${slot.label} 端口`">
          <el-input v-model="params[slot.field]" type="number" min="1" max="65535" />
        </el-form-item>
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
