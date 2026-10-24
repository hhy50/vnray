<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ArrowLeft, Delete, Plus } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { ClientDetail, HostRow, IPMapRow } from '../api/types'

const route = useRoute()
const router = useRouter()
const name = computed(() => String(route.params.name ?? ''))

const detail = ref<ClientDetail | null>(null)
const loadError = ref('')
const logLevel = ref('')
const serverConfig = ref('{}')
const hosts = ref<HostRow[]>([])
const ipMaps = ref<IPMapRow[]>([])
const concurrency = ref(0)
const saving = ref('')

const logLevels = [
  { value: 'info', label: 'info（默认）' },
  { value: 'debug', label: 'debug' },
  { value: 'warn', label: 'warn' },
  { value: 'error', label: 'error' },
]

async function load() {
  if (!name.value) return
  try {
    const data = await api.clientDetail(name.value)
    detail.value = data
    logLevel.value = data.log_level || 'info'
    serverConfig.value = data.server_config || '{}'
    hosts.value = data.hosts.length ? data.hosts.map((h) => ({ ...h })) : []
    ipMaps.value = data.ip_maps.length ? data.ip_maps.map((m) => ({ ...m })) : []
    concurrency.value = data.concurrency
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载失败'
  }
}

async function saveConfig() {
  saving.value = 'config'
  try {
    await api.clientConfigSave(name.value, { config: serverConfig.value, log_level: logLevel.value })
    ElMessage.success('服务端配置已保存')
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    saving.value = ''
  }
}

async function saveHosts() {
  saving.value = 'hosts'
  try {
    const res = await api.clientHostsSave(name.value, {
      hosts: hosts.value.filter((h) => h.domain || h.ip),
      ip_maps: ipMaps.value.filter((m) => m.from || m.to),
      concurrency: concurrency.value,
    })
    if (res.pushed) {
      ElMessage.success(`${res.name} 的下发配置已保存（在线客户端立即生效）`)
    } else {
      ElMessage.warning(`${res.name} 的下发配置已保存；客户端当前离线，下次认证时生效`)
    }
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    saving.value = ''
  }
}

function addHost() {
  hosts.value.push({ domain: '', ip: '' })
}

function addIPMap() {
  ipMaps.value.push({ from: '', to: '' })
}

onMounted(load)
watch(name, load)
</script>

<template>
  <div class="stack">
    <el-card shadow="never">
      <div class="toolbar">
        <el-button :icon="ArrowLeft" @click="router.push('/clients')">返回客户端</el-button>
        <el-tag v-if="detail" :type="detail.online ? 'success' : 'info'" effect="plain">
          {{ detail.online ? '在线' : '离线' }}
        </el-tag>
        <span v-if="detail?.remote" class="mono faint">{{ detail.remote }}</span>
        <div class="spacer" />
        <span v-if="detail?.has_stored" class="faint">最近更新 {{ detail.updated_at }}</span>
      </div>
    </el-card>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <template v-else-if="detail">
      <el-card shadow="never">
        <template #header><strong>服务端配置（下发到该客户端的运维值）</strong></template>
        <el-form label-position="top">
          <el-alert
            v-if="detail.stored_raw"
            title="现有内容不是合法 JSON，保存后将被整体覆盖。"
            type="warning"
            :closable="false"
            show-icon
            style="margin-bottom: 12px"
          />
          <el-form-item label="日志级别（只作用于该客户端会话，下次连接生效）">
            <el-select v-model="logLevel" style="width: 12rem">
              <el-option v-for="l in logLevels" :key="l.value" :label="l.label" :value="l.value" />
            </el-select>
          </el-form-item>
          <el-form-item label="server_config（JSON，写穿到 client_bindings.server_config）">
            <el-input
              v-model="serverConfig"
              type="textarea"
              :rows="14"
             
              class="code-area"
            />
          </el-form-item>
          <div class="toolbar">
            <el-button type="primary" :loading="saving === 'config'" @click="saveConfig">
              保存配置
            </el-button>
            <span class="page-hint">日志级别下拉框会覆盖文本域里的同名字段；其余键原样保留。</span>
          </div>
        </el-form>
      </el-card>

      <el-card shadow="never">
        <template #header><strong>自定义 Host / IP 段映射 / 并发上限</strong></template>

        <div class="section">
          <div class="section-head">
            <span class="muted">自定义 Host（域名 → 固定 IP）</span>
            <div class="spacer" />
            <el-button size="small" :icon="Plus" @click="addHost">添加记录</el-button>
          </div>
          <el-table :data="hosts" size="small" empty-text="还没有自定义 Host">
            <el-table-column label="域名" min-width="220">
              <template #default="{ row }">
                <el-input v-model="row.domain" placeholder="example.com" />
              </template>
            </el-table-column>
            <el-table-column label="固定 IP" min-width="180">
              <template #default="{ row }">
                <el-input v-model="row.ip" placeholder="192.168.1.10" class="mono" />
              </template>
            </el-table-column>
            <el-table-column width="80" align="right">
              <template #default="{ $index }">
                <el-button link type="danger" :icon="Delete" @click="hosts.splice($index, 1)" />
              </template>
            </el-table-column>
          </el-table>
        </div>

        <div class="section">
          <div class="section-head">
            <span class="muted">IP 段映射（源段 → 目标段，每段是 0-255 或 *）</span>
            <div class="spacer" />
            <el-button size="small" :icon="Plus" @click="addIPMap">添加映射</el-button>
          </div>
          <el-table :data="ipMaps" size="small" empty-text="还没有 IP 段映射">
            <el-table-column label="源段" min-width="180">
              <template #default="{ row }">
                <el-input v-model="row.from" placeholder="123.123.*.*" class="mono" />
              </template>
            </el-table-column>
            <el-table-column label="目标段" min-width="180">
              <template #default="{ row }">
                <el-input v-model="row.to" placeholder="192.168.*.*" class="mono" />
              </template>
            </el-table-column>
            <el-table-column width="80" align="right">
              <template #default="{ $index }">
                <el-button link type="danger" :icon="Delete" @click="ipMaps.splice($index, 1)" />
              </template>
            </el-table-column>
          </el-table>
        </div>

        <el-form label-position="top" style="margin-top: 6px">
          <el-form-item label="并发上限（同时路由到该客户端的隧道流数，0 = 不限）" style="max-width: 20rem">
            <el-input-number v-model="concurrency" :min="0" controls-position="right" />
          </el-form-item>
          <div class="toolbar">
            <el-button type="primary" :loading="saving === 'hosts'" @click="saveHosts">
              保存并下发
            </el-button>
            <span class="page-hint">在线客户端立即生效；离线客户端下次认证时随 server_config 补发。</span>
          </div>
        </el-form>
      </el-card>

      <el-card shadow="never">
        <template #header><strong>客户端上报配置（只读）</strong></template>
        <pre v-if="detail.reported_json" class="log-view">{{ detail.reported_json }}</pre>
        <p v-else class="page-hint">
          还没有上报记录。客户端连接后这里显示它认证时上报的完整配置快照（含 interfaces 网卡、log/reconnect/routing），
          并持久化到 client_bindings.config 列——离线后仍可查看最近一次上报。
        </p>
      </el-card>
    </template>
  </div>
</template>

<style scoped>
.section {
  margin-bottom: 18px;
}

.section-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}

.code-area :deep(textarea) {
  font-family: var(--mono);
  font-size: 12px;
  line-height: 1.6;
}
</style>
