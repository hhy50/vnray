<script setup lang="ts">
// 端口映射：服务端远程端口 → 客户端本地目标，保存即热更新监听器。
import { onMounted, reactive, ref } from 'vue'
import { Plus, Refresh } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { PortMap } from '../api/types'
import { rate, shortName } from '../lib/format'

const rows = ref<PortMap[]>([])
const clients = ref<{ name: string; label: string }[]>([])
const publicHost = ref('')
const loadError = ref('')
const loading = ref(false)

const editing = ref<number | undefined>(undefined)
const showForm = ref(false)
const saving = ref(false)
const form = reactive({
  client: '',
  remote_port: 0,
  local_host: '127.0.0.1',
  local_port: 0,
  limit_up_kb: 0,
  limit_down_kb: 0,
  remark: '',
  enabled: true,
})

async function load() {
  loading.value = true
  try {
    const data = await api.portmaps()
    rows.value = data.rows
    clients.value = data.clients
    publicHost.value = data.public_host
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载失败'
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
  form.remark = m.remark
  form.enabled = m.enabled
  showForm.value = true
}

async function save() {
  if (!form.client) {
    ElMessage.warning('请选择客户端')
    return
  }
  if (!form.remote_port || !form.local_port) {
    ElMessage.warning('远程端口与本地端口都必须大于 0')
    return
  }
  saving.value = true
  try {
    await api.portMapSave({ ...form }, editing.value)
    showForm.value = false
    ElMessage.success('已保存，监听器已热更新')
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    saving.value = false
  }
}

async function toggle(m: PortMap) {
  try {
    await api.portMapToggle(m.id)
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

async function remove(m: PortMap) {
  try {
    await ElMessageBox.confirm(
      `删除映射 ${m.remote_port} → ${m.local_host}:${m.local_port}？`,
      '删除映射',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await api.portMapDelete(m.id)
    ElMessage.success('已删除')
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '删除失败')
  }
}

onMounted(load)
</script>

<template>
  <el-card shadow="never" class="stack">
    <template #header>
      <div class="toolbar">
        <strong>共 {{ rows.length }} 条映射</strong>
        <span v-if="publicHost" class="faint">对外地址 {{ publicHost }}:远程端口</span>
        <div class="spacer" />
        <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
        <el-button type="primary" :icon="Plus" @click="openCreate">添加映射</el-button>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-table
      v-else
      v-loading="loading"
      :data="rows"
      size="small"
      row-key="id"
      empty-text="还没有配置端口映射。添加后服务端立即在远程端口上监听，外部访问「服务器地址:远程端口」会被转发到客户端本地目标。"
    >
      <el-table-column label="远程端口" width="92">
        <template #default="{ row }"><span class="mono port">{{ row.remote_port }}</span></template>
      </el-table-column>

      <el-table-column label="客户端" min-width="168">
        <template #default="{ row }">
          <div class="client-cell" :title="row.client">
            <span class="dot" :class="row.client_online ? 'on' : 'off'" />
            <span class="mono ellipsis">{{ shortName(row.client) }}</span>
          </div>
        </template>
      </el-table-column>

      <el-table-column label="本地目标" min-width="176">
        <template #default="{ row }">
          <span class="mono">{{ row.local_host }}:{{ row.local_port }}</span>
        </template>
      </el-table-column>

      <el-table-column label="限速 ↑ / ↓" width="112">
        <template #default="{ row }">
          <span class="faint">{{ rate(row.limit_up_kb) }} / {{ rate(row.limit_down_kb) }}</span>
        </template>
      </el-table-column>

      <el-table-column label="状态" width="126">
        <template #default="{ row }">
          <el-tag v-if="!row.enabled" size="small" type="info" effect="plain">已停用</el-tag>
          <el-tag v-else-if="row.listening" size="small" type="success">运行中</el-tag>
          <el-tooltip v-else content="端口被占用或监听启动失败，检查服务端日志" placement="top">
            <el-tag size="small" type="danger" effect="plain">未监听</el-tag>
          </el-tooltip>
        </template>
      </el-table-column>

      <el-table-column label="备注" min-width="130" show-overflow-tooltip>
        <template #default="{ row }"><span class="faint">{{ row.remark || '—' }}</span></template>
      </el-table-column>

      <el-table-column label="更新时间" width="146">
        <template #default="{ row }"><span class="faint">{{ row.updated_at }}</span></template>
      </el-table-column>

      <el-table-column label="操作" width="156" align="right" fixed="right">
        <template #default="{ row }">
          <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
          <el-button link @click="toggle(row)">{{ row.enabled ? '停用' : '启用' }}</el-button>
          <el-button link type="danger" @click="remove(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <p class="page-hint">
      链路：外部访问 {{ publicHost || '服务器地址' }}:远程端口 → 服务端 → 该客户端隧道 → 客户端本地拨号。
      规则持久化在数据库，保存 / 启停 / 删除即时生效；改限速不中断在途连接。客户端离线时监听仍在，新连接会被拒绝。
    </p>
  </el-card>

  <el-dialog
    v-model="showForm"
    :title="editing ? '编辑映射' : '添加映射'"
    width="min(600px, 94vw)"
    :close-on-click-modal="false"
  >
    <el-form label-position="top">
      <div class="form-row">
        <el-form-item label="远程端口（服务端监听）" style="flex: 1">
          <el-input-number
            v-model="form.remote_port"
            :min="1"
            :max="65535"
            controls-position="right"
            style="width: 100%"
          />
        </el-form-item>
        <el-form-item label="本地端口" style="flex: 1">
          <el-input-number
            v-model="form.local_port"
            :min="1"
            :max="65535"
            controls-position="right"
            style="width: 100%"
          />
        </el-form-item>
      </div>
      <el-form-item label="客户端">
        <el-select v-model="form.client" filterable allow-create default-first-option placeholder="namespace:group:name">
          <el-option v-for="c in clients" :key="c.name" :label="c.label" :value="c.name" />
        </el-select>
      </el-form-item>
      <el-form-item label="本地主机（客户端侧）">
        <el-input v-model="form.local_host" class="mono" />
      </el-form-item>
      <div class="form-row">
        <el-form-item label="上行限速 KB/s（0 = 不限）" style="flex: 1">
          <el-input-number v-model="form.limit_up_kb" :min="0" controls-position="right" style="width: 100%" />
        </el-form-item>
        <el-form-item label="下行限速 KB/s（0 = 不限）" style="flex: 1">
          <el-input-number v-model="form.limit_down_kb" :min="0" controls-position="right" style="width: 100%" />
        </el-form-item>
      </div>
      <el-form-item label="备注">
        <el-input v-model="form.remark" placeholder="例如：办公室 NAS" />
      </el-form-item>
      <el-form-item label="启用">
        <el-switch v-model="form.enabled" />
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="showForm = false">取消</el-button>
      <el-button type="primary" :loading="saving" @click="save">保存</el-button>
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
</style>
