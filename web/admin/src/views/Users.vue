<script setup lang="ts">
// 用户：协议 × user_key 的绑定记录，限速/并发/日志级别集中在这里配置。
import { computed, onMounted, reactive, ref } from 'vue'
import { Plus, Search, CopyDocument, ArrowDown } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { User } from '../api/types'
import { bytes, copyText, rate, shortName } from '../lib/format'

const users = ref<User[]>([])
const clients = ref<{ name: string; label: string }[]>([])
const protocols = ref<string[]>([])
const q = ref('')
const protocol = ref('')
const loading = ref(false)
const loadError = ref('')

const showAdd = ref(false)
const saving = ref(false)
const addForm = reactive({ name: '', protocol: 'vless', client: '', user_key: '' })

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

const selectedClientLabel = computed(() => {
  const hit = clients.value.find((c) => c.name === addForm.client)
  return hit ? hit.label : addForm.client
})

const emptyText = computed(() => {
  if (q.value.trim() || protocol.value) return '没有符合条件的用户，换个关键字或协议试试。'
  return '还没有用户。点右上角「添加用户」，把某个协议的 user_key 绑定到出口客户端。'
})

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
  // 默认 vless：新接入以 vless 为主，省得每次手动切协议再点生成。
  addForm.protocol = 'vless'
  addForm.client = ''
  addForm.user_key = generateKey('vless')
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

// 开关是即时生效的：先改本地再回滚，避免等接口时开关自己弹回去。
async function toggleStatus(u: User, enabled: boolean) {
  const previous = u.enabled
  u.enabled = enabled
  try {
    await api.userStatus(u.protocol, u.user_key, enabled)
    ElMessage.success(enabled ? '已启用' : '已停用')
  } catch (err) {
    u.enabled = previous
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

async function copyLink(u: User, legacy = false) {
  // 两代链接互斥：新版用证书指纹，旧版用 allowInsecure，混在一份配置里会让新内核拒绝启动。
  const link = legacy ? u.share_link_legacy : u.share_link
  if (!link) {
    ElMessage.warning(
      legacy
        ? '没有旧版链接：只有自签证书才需要，且插件需已启用 TLS'
        : '该用户没有可分享的链接（插件未设置端口或协议不适用）',
    )
    return
  }
  if (await copyText(link)) {
    ElMessage.success(legacy ? '旧版链接已复制（仅限 Xray < 26.2.6）' : '链接已复制')
  } else {
    ElMessage.error('复制失败')
  }
}

async function copyKey(u: User) {
  if (await copyText(u.user_key)) ElMessage.success('user_key 已复制')
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
          style="width: 17rem"
          @keyup.enter="load()"
        />
        <el-select v-model="protocol" placeholder="全部协议" style="width: 7.5rem" @change="load()">
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
      :empty-text="emptyText"
    >
      <el-table-column label="协议" width="78">
        <template #default="{ row }">
          <el-tag size="small" type="info" effect="plain">{{ row.protocol }}</el-tag>
        </template>
      </el-table-column>

      <el-table-column label="user_key" min-width="230">
        <template #default="{ row }">
          <div class="key-cell">
            <span class="mono ellipsis" :title="row.user_key">{{ row.user_key }}</span>
            <el-tooltip content="复制 user_key" placement="top">
              <el-icon class="copy-btn" @click="copyKey(row)"><CopyDocument /></el-icon>
            </el-tooltip>
          </div>
        </template>
      </el-table-column>

      <el-table-column label="名称" min-width="100" show-overflow-tooltip>
        <template #default="{ row }">{{ row.name || '—' }}</template>
      </el-table-column>

      <el-table-column label="状态" width="86">
        <template #default="{ row }">
          <el-switch
            :model-value="row.enabled"
            size="small"
            inline-prompt
            active-text="启用"
            inactive-text="停用"
            @change="(v: string | number | boolean) => toggleStatus(row, Boolean(v))"
          />
        </template>
      </el-table-column>

      <el-table-column label="出口客户端" min-width="130" show-overflow-tooltip>
        <template #default="{ row }">
          <span class="mono">{{ shortName(row.client) }}</span>
        </template>
      </el-table-column>

      <el-table-column label="限速 / 并发" width="120">
        <template #default="{ row }">
          <div class="faint">↑ {{ rate(row.limit_up_kb) }}</div>
          <div class="faint">↓ {{ rate(row.limit_down_kb) }}</div>
          <div class="faint">并发 {{ row.concurrency || '不限' }}</div>
        </template>
      </el-table-column>

      <el-table-column label="累计转发" width="96">
        <template #default="{ row }">
          <div class="faint">↑ {{ bytes(row.up_bytes) }}</div>
          <div class="faint">↓ {{ bytes(row.down_bytes) }}</div>
        </template>
      </el-table-column>

      <el-table-column label="操作" width="196" align="right" fixed="right">
        <template #default="{ row }">
          <el-button link type="primary" @click="openLimits(row)">配置</el-button>
          <el-tooltip
            :disabled="!!row.share_link"
            :content="'该协议还没有对应的监听端口'"
            placement="top"
          >
            <span>
              <el-dropdown
                v-if="row.shareable"
                trigger="click"
                :disabled="!row.share_link"
                @command="(cmd: string) => copyLink(row, cmd === 'legacy')"
              >
                <el-button link type="primary" :disabled="!row.share_link">
                  复制链接<el-icon class="el-icon--right"><ArrowDown /></el-icon>
                </el-button>
                <template #dropdown>
                  <el-dropdown-menu>
                    <el-dropdown-item command="modern">新版内核：证书指纹</el-dropdown-item>
                    <el-dropdown-item v-if="row.share_link_legacy" command="legacy" divided>
                      旧版内核：允许不安全（Xray &lt; 26.2.6）
                    </el-dropdown-item>
                  </el-dropdown-menu>
                </template>
              </el-dropdown>
            </span>
          </el-tooltip>
          <el-button link type="danger" @click="removeUser(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>
  </el-card>

  <el-dialog v-model="showAdd" title="添加用户" width="min(600px, 94vw)" :close-on-click-modal="false">
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
        <div class="key-row">
          <el-input v-model="addForm.user_key" class="mono" />
          <el-button @click="addForm.user_key = generateKey(addForm.protocol)">生成</el-button>
        </div>
      </el-form-item>
      <p class="hint-inline">
        当前出口：<span class="mono">{{ selectedClientLabel || '未选择' }}</span>
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
    width="min(560px, 94vw)"
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
  flex-wrap: wrap;
}

.key-cell {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}

.ellipsis {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.copy-btn {
  flex-shrink: 0;
  color: var(--el-text-color-placeholder);
  cursor: pointer;
}

.copy-btn:hover {
  color: var(--el-color-primary);
}

.key-row {
  display: flex;
  gap: 8px;
  width: 100%;
}

.hint-inline {
  margin: 0;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}

.hint-inline .mono {
  color: var(--el-text-color-regular);
}
</style>
