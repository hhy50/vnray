<script setup lang="ts">
// 用户：协议 × user_key 的绑定记录，限速/并发/日志级别集中在这里配置。
import { computed, onMounted, reactive, ref } from 'vue'
import { Plus, Search, CopyDocument, ArrowDown, Filter } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { User } from '../api/types'
import RulesDialog from '../components/RulesDialog.vue'
import { bytes, copyText, rate, shortName } from '../lib/format'
import { useI18n } from '../i18n'

const { t } = useI18n()

const users = ref<User[]>([])
const clients = ref<{ name: string; label: string }[]>([])
const protocols = ref<string[]>([])
const rules = ref<string[]>([])
const q = ref('')
const protocol = ref('')
const loading = ref(false)
const loadError = ref('')

const showAdd = ref(false)
const saving = ref(false)
// rule 为空串即内置 default：判定时空规则名按 default 处理，不必写冗余值。
const addForm = reactive({
  name: '',
  protocol: 'vless',
  client: '',
  user_key: '',
  ss_method: 'aes-256-gcm',
  rule: 'default',
})
const ssMethods = ['aes-256-gcm', 'aes-128-gcm', 'chacha20-ietf-poly1305']

// 出站规则改成用户页的弹窗入口：规则本身不再占独立菜单。
const showRules = ref(false)
// default 排在最前，其余按服务端返回的名字升序。
const ruleOptions = computed(() => [
  'default',
  ...rules.value.filter((name) => name !== 'default'),
])

const limitsFor = ref<User | null>(null)
const limitsSaving = ref(false)
const limits = reactive({ up_kbps: 0, down_kbps: 0, concurrency: 0, log_level: 'info', rule: '' })

const logLevels = [
  { value: 'info', label: 'info' },
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
  if (q.value.trim() || protocol.value) return t('users.emptyFiltered')
  return t('users.emptyAll')
})

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    const data = await api.users({ q: q.value.trim(), protocol: protocol.value })
    users.value = data.users
    clients.value = data.clients
    protocols.value = data.protocols
    rules.value = data.rules ?? []
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : t('common.loadFailed')
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
  addForm.ss_method = 'aes-256-gcm'
  addForm.rule = 'default'
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
      ss_method: addForm.protocol === 'ss' ? addForm.ss_method : undefined,
      rule: addForm.rule,
    })
    showAdd.value = false
    ElMessage.success(t('users.added'))
    await load(true)
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('users.addFailed'))
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
    ElMessage.success(enabled ? t('users.enabled') : t('users.disabled'))
  } catch (err) {
    u.enabled = previous
    ElMessage.error(err instanceof ApiError ? err.message : t('common.opFailed'))
  }
}

async function removeUser(u: User) {
  try {
    await ElMessageBox.confirm(
      t('users.deleteConfirm', { key: u.user_key, protocol: u.protocol }),
      t('users.deleteTitle'),
      { type: 'warning', confirmButtonText: t('common.delete'), cancelButtonText: t('common.cancel') },
    )
  } catch {
    return
  }
  try {
    await api.userDelete(u.protocol, u.user_key)
    ElMessage.success(t('common.deleted'))
    await load(true)
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.deleteFailed'))
  }
}

function openLimits(u: User) {
  limitsFor.value = u
  limits.up_kbps = u.limit_up_kb
  limits.down_kbps = u.limit_down_kb
  limits.concurrency = u.concurrency
  limits.log_level = u.log_level || 'info'
  limits.rule = u.rule || 'default'
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
      rule: limits.rule,
    })
    limitsFor.value = null
    ElMessage.success(t('users.limitsSaved'))
    await load(true)
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.saveFailed'))
  } finally {
    limitsSaving.value = false
  }
}

async function copyLink(u: User, legacy = false) {
  // 两代链接互斥：新版用证书指纹，旧版用 allowInsecure，混在一份配置里会让新内核拒绝启动。
  const link = legacy ? u.share_link_legacy : u.share_link
  if (!link) {
    ElMessage.warning(legacy ? t('users.noLegacyLink') : t('users.noLink'))
    return
  }
  if (await copyText(link)) {
    ElMessage.success(legacy ? t('users.legacyLinkCopied') : t('users.linkCopied'))
  } else {
    ElMessage.error(t('common.copyFailed'))
  }
}

async function copyKey(u: User) {
  if (await copyText(u.user_key)) ElMessage.success(t('users.keyCopied'))
  else ElMessage.error(t('common.copyFailed'))
}

onMounted(load)
</script>

<template>
  <el-card shadow="never" class="stack">
    <template #header>
      <div class="toolbar">
        <strong>{{ t('users.count', { n: users.length }) }}</strong>
        <div class="spacer" />
        <el-input
          v-model="q"
          :placeholder="t('users.searchPlaceholder')"
          clearable
          :prefix-icon="Search"
          style="width: 17rem"
          @keyup.enter="load()"
        />
        <el-select v-model="protocol" :placeholder="t('users.allProtocols')" style="width: 7.5rem" @change="load()">
          <el-option :label="t('users.allProtocols')" value="" />
          <el-option v-for="p in protocols" :key="p" :label="p" :value="p" />
        </el-select>
        <el-button :loading="loading" @click="load()">{{ t('users.query') }}</el-button>
        <el-button :icon="Filter" @click="showRules = true">{{ t('users.rulesButton') }}</el-button>
        <el-button type="primary" :icon="Plus" @click="openAdd">{{ t('users.add') }}</el-button>
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
      <el-table-column :label="t('users.colProtocol')" min-width="150">
        <template #default="{ row }">
          <el-tag size="small" type="info" effect="plain">{{ row.protocol }}</el-tag>
          <div v-if="row.protocol === 'ss'" class="faint">{{ row.ss_method }}</div>
        </template>
      </el-table-column>

      <el-table-column label="user_key" min-width="210">
        <template #default="{ row }">
          <div class="key-cell">
            <span class="mono ellipsis" :title="row.user_key">{{ row.user_key }}</span>
            <el-tooltip :content="t('users.copyKey')" placement="top">
              <el-icon class="copy-btn" @click="copyKey(row)"><CopyDocument /></el-icon>
            </el-tooltip>
          </div>
        </template>
      </el-table-column>

      <el-table-column :label="t('users.colName')" min-width="100" show-overflow-tooltip>
        <template #default="{ row }">{{ row.name || '—' }}</template>
      </el-table-column>

      <el-table-column :label="t('users.colStatus')" width="86">
        <template #default="{ row }">
          <el-switch
            :model-value="row.enabled"
            size="small"
            inline-prompt
            :active-text="t('common.on')"
            :inactive-text="t('common.off')"
            @change="(v: string | number | boolean) => toggleStatus(row, Boolean(v))"
          />
        </template>
      </el-table-column>

      <el-table-column :label="t('users.colClient')" min-width="130" show-overflow-tooltip>
        <template #default="{ row }">
          <span class="mono">{{ shortName(row.client) }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('users.colRule')" width="110" show-overflow-tooltip>
        <template #default="{ row }">
          <span class="mono">{{ row.rule || 'default' }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('users.colLimits')" width="120">
        <template #default="{ row }">
          <div class="faint">↑ {{ rate(row.limit_up_kb) }}</div>
          <div class="faint">↓ {{ rate(row.limit_down_kb) }}</div>
          <div class="faint">
            {{ row.concurrency ? t('users.concurrency', { value: row.concurrency }) : t('common.unlimited') }}
          </div>
        </template>
      </el-table-column>

      <el-table-column :label="t('users.colTraffic')" width="96">
        <template #default="{ row }">
          <div class="faint">↑ {{ bytes(row.up_bytes) }}</div>
          <div class="faint">↓ {{ bytes(row.down_bytes) }}</div>
        </template>
      </el-table-column>

      <el-table-column :label="t('users.colActions')" width="240" align="right" fixed="right">
        <template #default="{ row }">
          <div class="row-actions">
            <el-button link type="primary" @click="openLimits(row)">{{ t('common.configure') }}</el-button>
            <el-tooltip
              v-if="row.shareable"
              :disabled="!!row.share_link"
              :content="t('users.noPortTip')"
              placement="top"
            >
              <span class="share-action">
                <el-dropdown
                  v-if="row.share_link_legacy"
                  trigger="click"
                  placement="bottom-end"
                  :disabled="!row.share_link"
                  @command="(cmd: string) => copyLink(row, cmd === 'legacy')"
                >
                  <el-button
                    class="share-button"
                    plain
                    size="small"
                    type="primary"
                    :icon="CopyDocument"
                    :disabled="!row.share_link"
                  >
                    {{ t('users.copyLink') }}<el-icon class="share-arrow"><ArrowDown /></el-icon>
                  </el-button>
                  <template #dropdown>
                    <el-dropdown-menu>
                      <el-dropdown-item command="modern">
                        <div class="share-option">
                          <span class="share-option-title">
                            {{ t('users.newLink') }}<el-tag size="small" effect="light">{{ t('users.recommended') }}</el-tag>
                          </span>
                          <span class="share-option-description">{{ t('users.newLinkDesc') }}</span>
                        </div>
                      </el-dropdown-item>
                      <el-dropdown-item command="legacy" divided>
                        <div class="share-option">
                          <span class="share-option-title">{{ t('users.legacyLink') }}</span>
                          <span class="share-option-description">{{ t('users.legacyLinkDesc') }}</span>
                        </div>
                      </el-dropdown-item>
                    </el-dropdown-menu>
                  </template>
                </el-dropdown>
                <el-button
                  v-else
                  class="share-button"
                  plain
                  size="small"
                  type="primary"
                  :icon="CopyDocument"
                  :disabled="!row.share_link"
                  @click="copyLink(row)"
                >{{ t('users.copyLink') }}</el-button>
              </span>
            </el-tooltip>
            <el-button link type="danger" @click="removeUser(row)">{{ t('common.delete') }}</el-button>
          </div>
        </template>
      </el-table-column>
    </el-table>
  </el-card>

  <el-dialog v-model="showAdd" :title="t('users.add')" width="min(600px, 94vw)" :close-on-click-modal="false">
    <el-form label-position="top" @submit.prevent="submitAdd">
      <el-form-item :label="t('users.colName')">
        <el-input v-model="addForm.name" :placeholder="t('users.namePlaceholder')" />
      </el-form-item>
      <div class="form-row">
        <el-form-item :label="t('users.colProtocol')" style="flex: 1">
          <el-select v-model="addForm.protocol" @change="addForm.user_key = generateKey(addForm.protocol)">
            <el-option v-for="p in protocols" :key="p" :label="p" :value="p" />
          </el-select>
        </el-form-item>
        <el-form-item :label="t('users.colClient')" style="flex: 1">
          <el-select
            v-model="addForm.client"
            filterable
            allow-create
            default-first-option
            :placeholder="t('users.clientPlaceholder')"
          >
            <el-option v-for="c in clients" :key="c.name" :label="c.label" :value="c.name" />
          </el-select>
        </el-form-item>
      </div>
      <el-form-item v-if="addForm.protocol === 'ss'" :label="t('users.ssMethod')">
        <el-select v-model="addForm.ss_method">
          <el-option v-for="method in ssMethods" :key="method" :label="method" :value="method" />
        </el-select>
      </el-form-item>
      <el-form-item :label="t('users.ruleAddLabel')">
        <el-select v-model="addForm.rule" filterable style="width: 100%">
          <el-option v-for="r in ruleOptions" :key="r" :label="r" :value="r" />
        </el-select>
      </el-form-item>
      <el-form-item :label="t('users.keyLabel')">
        <div class="key-row">
          <el-input v-model="addForm.user_key" class="mono" />
          <el-button @click="addForm.user_key = generateKey(addForm.protocol)">{{ t('users.generate') }}</el-button>
        </div>
      </el-form-item>
      <p class="hint-inline">
        {{ t('users.currentEgress') }}<span class="mono">{{ selectedClientLabel || t('users.notSelected') }}</span>
      </p>
    </el-form>
    <template #footer>
      <el-button @click="showAdd = false">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" :loading="saving" @click="submitAdd">{{ t('common.add') }}</el-button>
    </template>
  </el-dialog>

  <RulesDialog v-model="showRules" @changed="load(true)" />

  <el-dialog
    :model-value="!!limitsFor"
    :title="limitsFor ? t('users.configTitle', { key: limitsFor.user_key, protocol: limitsFor.protocol }) : t('users.configFallback')"
    width="min(560px, 94vw)"
    @update:model-value="limitsFor = null"
  >
    <el-form label-position="top">
      <div class="form-row">
        <el-form-item :label="t('users.upLimit')" style="flex: 1">
          <el-input-number v-model="limits.up_kbps" :min="0" controls-position="right" style="width: 100%" />
        </el-form-item>
        <el-form-item :label="t('users.downLimit')" style="flex: 1">
          <el-input-number v-model="limits.down_kbps" :min="0" controls-position="right" style="width: 100%" />
        </el-form-item>
      </div>
      <div class="form-row">
        <el-form-item :label="t('users.maxConcurrency')" style="flex: 1">
          <el-input-number v-model="limits.concurrency" :min="0" controls-position="right" style="width: 100%" />
        </el-form-item>
        <el-form-item :label="t('users.logLevel')" style="flex: 1">
          <el-select v-model="limits.log_level">
            <el-option
              v-for="l in logLevels"
              :key="l.value"
              :label="l.value === 'info' ? t('users.logLevelDefault') : l.label"
              :value="l.value"
            />
          </el-select>
        </el-form-item>
      </div>
      <el-form-item :label="t('users.ruleLabel')">
        <el-select v-model="limits.rule" filterable style="width: 100%">
          <el-option v-for="r in ruleOptions" :key="r" :label="r" :value="r" />
        </el-select>
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="limitsFor = null">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" :loading="limitsSaving" @click="saveLimits">{{ t('common.save') }}</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.row-actions {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 12px;
  white-space: nowrap;
}

.row-actions > .el-button {
  margin-left: 0;
}

.share-action {
  display: inline-flex;
  align-items: center;
}

.share-button {
  height: 28px;
  padding: 0 10px;
  border-radius: 6px;
  font-weight: 500;
}

.share-button:focus-visible {
  outline: 2px solid var(--el-color-primary);
  outline-offset: 2px;
}

.share-arrow {
  margin-left: 8px;
  font-size: 12px;
}

.share-option {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 6px 0;
  line-height: 1.5;
}

.share-option-title {
  display: flex;
  align-items: center;
  gap: 8px;
  font-weight: 500;
}

.share-option-description {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}

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
