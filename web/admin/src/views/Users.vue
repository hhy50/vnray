<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { Plus, Search } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { User } from '../api/types'
import { bytes, copyText, rate } from '../lib/format'

const users = ref<User[]>([])
const clients = ref<{ name: string; label: string }[]>([])
const protocols = ref<string[]>([])
const q = ref('')
const protocol = ref('')
const loading = ref(false)
const loadError = ref('')

const showAdd = ref(false)
const saving = ref(false)
const addForm = reactive({ name: '', protocol: 'vmess', client: '', user_key: '' })

const limitsFor = ref<User | null>(null)
const limitsSaving = ref(false)
const limits = reactive({ up_kbps: 0, down_kbps: 0, concurrency: 0, log_level: 'info' })

const logLevels = [
  { value: 'info', label: 'info（默认）' },
  { value: 'debug', label: 'debug' },
  { value: 'warn', label: 'warn' },
  { value: 'error', label: 'error' },
]

// 同一个 user_key 可以在不同协议下各有一条，行键必须带协议。
const rowKey = (row: User) => row.protocol + ':' + row.user_key

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    const data = await api.users({ q: q.value.trim(), protocol: protocol.value })
    users.value = data.users
    clients.value = data.clients
    protocols.value = data.protocols
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载失败'
  } finally {
    loading.value = false
  }
}

function openAdd() {
  addForm.name = ''
  addForm.protocol = 'vmess'
  addForm.client = ''
  addForm.user_key = generateKey('vmess')
  showAdd.value = true
}

function generateKey(p: string): string {
  const hex = (n: number) => {
    const r = new Uint8Array(n)
    crypto.getRandomValues(r)
    return Array.from(r, (x) => ('0' + x.toString(16)).slice(-2)).join('')
  }
  if (p === 'vmess' || p === 'vless') {
    if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
    const h = hex(16)
    return `${h.slice(0, 8)}-${h.slice(8, 12)}-4${h.slice(13, 16)}-${((parseInt(h[16], 16) & 0x3) | 0x8).toString(16)}${h.slice(17, 20)}-${h.slice(20, 32)}`
  }
  if (p === 'ss') {
    const chars = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
    const b = new Uint8Array(24)
    crypto.getRandomValues(b)
    return Array.from(b, (x) => chars[x % chars.length]).join('')
  }
  return 'user-' + hex(12)
}

async function submitAdd() {
  saving.value = true
  try {
    await api.userSave({
      protocol: addForm.protocol,
      user_key: addForm.user_key.trim(),
      name: addForm.name.trim(),
      client: addForm.client.trim(),
      enabled: true,
    })
    showAdd.value = false
    ElMessage.success('用户已添加')
    await load(true)
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '添加失败')
  } finally {
    saving.value = false
  }
}

async function toggleStatus(u: User) {
  try {
    await api.userStatus(u.protocol, u.user_key, !u.enabled)
    ElMessage.success(u.enabled ? '已停用' : '已启用')
    await load(true)
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

async function removeUser(u: User) {
  try {
    await ElMessageBox.confirm(
      `删除用户 ${u.user_key}（${u.protocol}）？该操作立即生效且不可撤销。`,
      '删除用户',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await api.userDelete(u.protocol, u.user_key)
    ElMessage.success('已删除')
    await load(true)
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '删除失败')
  }
}

function openLimits(u: User) {
  limitsFor.value = u
  limits.up_kbps = u.limit_up_kb
  limits.down_kbps = u.limit_down_kb
  limits.concurrency = u.concurrency
  limits.log_level = u.log_level || 'info'
}

async function saveLimits() {
  const u = limitsFor.value
  if (!u) return
  limitsSaving.value = true
  try {
    await api.userLimits(u.protocol, u.user_key, {
      up_kbps: limits.up_kbps,
      down_kbps: limits.down_kbps,
      concurrency: limits.concurrency,
      log_level: limits.log_level,
    })
    limitsFor.value = null
    ElMessage.success('已保存，限速即时生效')
    await load(true)
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    limitsSaving.value = false
  }
}

async function copyLink(u: User) {
  if (!u.share_link) {
    ElMessage.warning('该用户没有可分享的链接（插件未设置端口或协议不适用）')
    return
  }
  if (await copyText(u.share_link)) ElMessage.success('链接已复制')
  else ElMessage.error('复制失败')
}

onMounted(load)
</script>

<template>
  <el-card shadow="never" class="stack">
    <template #header>
      <div class="toolbar">
        <strong>共 {{ users.length }} 个用户</strong>
        <div class="spacer" />
        <el-input
          v-model="q"
          placeholder="搜索 user_key / 名称 / 出口客户端"
          clearable
          :prefix-icon="Search"
          style="width: 16rem"
          @keyup.enter="load()"
        />
        <el-select v-model="protocol" placeholder="全部协议" style="width: 8rem" @change="load()">
          <el-option label="全部协议" value="" />
          <el-option v-for="p in protocols" :key="p" :label="p" :value="p" />
        </el-select>
        <el-button :loading="loading" @click="load()">查询</el-button>
        <el-button type="primary" :icon="Plus" @click="openAdd">添加用户</el-button>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-table
      v-else
      v-loading="loading"
      :data="users"
      size="small"
      :row-key="rowKey"
      empty-text="没有符合条件的用户。"
    >
      <el-table-column label="协议" width="86">
        <template #default="{ row }"><span class="faint">{{ row.protocol }}</span></template>
      </el-table-column>

      <el-table-column label="user_key" min-width="230">
        <template #default="{ row }"><span class="mono">{{ row.user_key }}</span></template>
      </el-table-column>

      <el-table-column label="名称" min-width="110">
        <template #default="{ row }">{{ row.name || '—' }}</template>
      </el-table-column>

      <el-table-column label="状态" width="84">
        <template #default="{ row }">
          <el-tag :type="row.enabled ? 'success' : 'info'" size="small" :effect="row.enabled ? 'light' : 'plain'">
            {{ row.enabled ? '启用' : '停用' }}
          </el-tag>
        </template>
      </el-table-column>

      <el-table-column label="出口客户端" min-width="130">
        <template #default="{ row }"><span class="mono">{{ row.client }}</span></template>
      </el-table-column>

      <el-table-column label="限速" width="104">
        <template #default="{ row }">
          <div class="faint">↑ {{ rate(row.limit_up_kb) }}</div>
          <div class="faint">↓ {{ rate(row.limit_down_kb) }}</div>
        </template>
      </el-table-column>

      <el-table-column label="并发" width="70">
        <template #default="{ row }">
          <span class="faint">{{ row.concurrency || '不限' }}</span>
        </template>
      </el-table-column>

      <el-table-column label="累计转发" width="104">
        <template #default="{ row }">
          <div class="faint">↑ {{ bytes(row.up_bytes) }}</div>
          <div class="faint">↓ {{ bytes(row.down_bytes) }}</div>
        </template>
      </el-table-column>

      <el-table-column label="日志" width="70">
        <template #default="{ row }"><span class="faint">{{ row.log_level || 'info' }}</span></template>
      </el-table-column>

      <el-table-column label="操作" width="220" align="right">
        <template #default="{ row }">
          <el-button link type="primary" @click="openLimits(row)">配置</el-button>
          <el-button link @click="toggleStatus(row)">{{ row.enabled ? '停用' : '启用' }}</el-button>
          <el-button link type="danger" @click="removeUser(row)">删除</el-button>
          <el-tooltip
            :disabled="!!row.share_link"
            :content="'v2ray 插件还没有对应协议的监听端口'"
            placement="top"
          >
            <span>
              <el-button
                v-if="row.shareable"
                link
                :disabled="!row.share_link"
                @click="copyLink(row)"
              >复制链接</el-button>
            </span>
          </el-tooltip>
        </template>
      </el-table-column>
    </el-table>

    <p class="page-hint">
      限速按用户计（同用户多条隧道共享配额）；绑定、限速、日志级别都写穿到持久存储。
      改绑后需重连生效，限速与停用即时生效，日志级别下次连接生效。协议是身份的一部分：同一个 user_key 可以在不同协议下各有一条记录。
    </p>
  </el-card>

  <el-dialog v-model="showAdd" title="添加用户" width="560px">
    <el-form label-position="top" @submit.prevent="submitAdd">
      <el-form-item label="名称">
        <el-input v-model="addForm.name" placeholder="给这个用户起个一眼能认出的名字" />
      </el-form-item>
      <div class="form-row">
        <el-form-item label="协议" style="flex: 1">
          <el-select v-model="addForm.protocol" @change="addForm.user_key = generateKey(addForm.protocol)">
            <el-option v-for="p in protocols" :key="p" :label="p" :value="p" />
          </el-select>
        </el-form-item>
        <el-form-item label="出口客户端" style="flex: 1">
          <el-select
            v-model="addForm.client"
            filterable
            allow-create
            default-first-option
            placeholder="选择或输入客户端名"
          >
            <el-option v-for="c in clients" :key="c.name" :label="c.label" :value="c.name" />
          </el-select>
        </el-form-item>
      </div>
      <el-form-item label="user_key / uuid / 密码">
        <div style="display: flex; gap: 8px; width: 100%">
          <el-input v-model="addForm.user_key" class="mono" />
          <el-button @click="addForm.user_key = generateKey(addForm.protocol)">生成</el-button>
        </div>
      </el-form-item>
      <p class="page-hint">
        vmess/vless 的密钥必须是合法 UUID；vn-v1 是共享密钥，ss 是连接密码。新增用户默认启用，协议创建后不可修改。
      </p>
    </el-form>
    <template #footer>
      <el-button @click="showAdd = false">取消</el-button>
      <el-button type="primary" :loading="saving" @click="submitAdd">添加</el-button>
    </template>
  </el-dialog>

  <el-dialog
    :model-value="!!limitsFor"
    :title="limitsFor ? `配置 · ${limitsFor.user_key}（${limitsFor.protocol}）` : '配置'"
    width="520px"
    @update:model-value="limitsFor = null"
  >
    <el-form label-position="top">
      <div class="form-row">
        <el-form-item label="↑ 上行 KB/s（0 = 不限）" style="flex: 1">
          <el-input-number v-model="limits.up_kbps" :min="0" controls-position="right" style="width: 100%" />
        </el-form-item>
        <el-form-item label="↓ 下行 KB/s（0 = 不限）" style="flex: 1">
          <el-input-number v-model="limits.down_kbps" :min="0" controls-position="right" style="width: 100%" />
        </el-form-item>
      </div>
      <div class="form-row">
        <el-form-item label="最大并发连接（0 = 不限）" style="flex: 1">
          <el-input-number v-model="limits.concurrency" :min="0" controls-position="right" style="width: 100%" />
        </el-form-item>
        <el-form-item label="日志级别（下次连接生效）" style="flex: 1">
          <el-select v-model="limits.log_level">
            <el-option v-for="l in logLevels" :key="l.value" :label="l.label" :value="l.value" />
          </el-select>
        </el-form-item>
      </div>
      <p class="page-hint">限速与并发即时生效；超限时新连接排队等待空位。日志级别只作用于该用户会话。</p>
    </el-form>
    <template #footer>
      <el-button @click="limitsFor = null">取消</el-button>
      <el-button type="primary" :loading="limitsSaving" @click="saveLimits">保存</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.form-row {
  display: flex;
  gap: 12px;
}
</style>
