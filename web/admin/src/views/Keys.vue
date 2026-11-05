<script setup lang="ts">
// 认证密钥：按范围（* / namespace:* / 客户端名）决定哪些客户端能用哪把密钥。
import { computed, onMounted, reactive, ref } from 'vue'
import { Plus, Refresh, CopyDocument } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { KeyRow } from '../api/types'
import { copyText } from '../lib/format'
import { useI18n } from '../i18n'

const { t } = useI18n()

const keys = ref<KeyRow[]>([])
const loadError = ref('')
const loading = ref(false)

const showAdd = ref(false)
const saving = ref(false)
const form = reactive({ key: '', scope: '*' })

const scopeOptions = computed(() => [
  { value: '*', label: t('keys.scopeAll') },
  { value: 'default:*', label: t('keys.scopeNamespace') },
])

async function load() {
  loading.value = true
  try {
    const data = await api.keys()
    keys.value = data.keys
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : t('common.loadFailed')
  } finally {
    loading.value = false
  }
}

function openAdd() {
  form.key = generateKey()
  form.scope = '*'
  showAdd.value = true
}

function generateKey(): string {
  const b = new Uint8Array(24)
  crypto.getRandomValues(b)
  return Array.from(b, (x) => ('0' + x.toString(16)).slice(-2)).join('')
}

async function save() {
  if (form.key.trim().length < 8) {
    ElMessage.warning(t('keys.needLength'))
    return
  }
  saving.value = true
  try {
    await api.keySave({ key: form.key.trim(), scope: form.scope.trim() || '*' })
    showAdd.value = false
    ElMessage.success(t('keys.saved'))
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.saveFailed'))
  } finally {
    saving.value = false
  }
}

async function remove(k: KeyRow) {
  try {
    await ElMessageBox.confirm(
      t('keys.deleteConfirm', { key: k.key }),
      t('keys.deleteTitle'),
      { type: 'warning', confirmButtonText: t('common.delete'), cancelButtonText: t('common.cancel') },
    )
  } catch {
    return
  }
  try {
    await api.keyDelete(k.key)
    ElMessage.success(t('common.deleted'))
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.deleteFailed'))
  }
}

async function copy(value: string) {
  if (await copyText(value)) ElMessage.success(t('keys.copied'))
  else ElMessage.error(t('common.copyFailed'))
}

onMounted(load)
</script>

<template>
  <el-card shadow="never" class="stack">
    <template #header>
      <div class="toolbar">
        <strong>{{ t('keys.count', { n: keys.length }) }}</strong>
        <div class="spacer" />
        <el-button :icon="Refresh" :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
        <el-button type="primary" :icon="Plus" @click="openAdd">{{ t('keys.add') }}</el-button>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-table
      v-else
      v-loading="loading"
      :data="keys"
      size="small"
      row-key="key"
      :empty-text="t('keys.empty')"
    >
      <el-table-column :label="t('keys.colScope')" width="170">
        <template #default="{ row }">
          <el-tag v-if="row.scope === '*'" size="small" type="info" effect="plain">{{ t('keys.allClients') }}</el-tag>
          <span v-else class="mono">{{ row.scope }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('keys.colKey')" min-width="300">
        <template #default="{ row }">
          <div class="key-cell">
            <span class="mono ellipsis" :title="row.key">{{ row.key }}</span>
            <el-tooltip :content="t('keys.copyKey')" placement="top">
              <el-icon class="copy-btn" @click="copy(row.key)"><CopyDocument /></el-icon>
            </el-tooltip>
          </div>
        </template>
      </el-table-column>

      <el-table-column :label="t('keys.colCreated')" width="160">
        <template #default="{ row }"><span class="faint">{{ row.created_at }}</span></template>
      </el-table-column>

      <el-table-column :label="t('keys.colUpdated')" width="160">
        <template #default="{ row }"><span class="faint">{{ row.updated_at }}</span></template>
      </el-table-column>

      <el-table-column :label="t('keys.colActions')" width="120" align="right" fixed="right">
        <template #default="{ row }">
          <el-button link type="primary" @click="copy(row.key)">{{ t('common.copy') }}</el-button>
          <el-button link type="danger" @click="remove(row)">{{ t('common.delete') }}</el-button>
        </template>
      </el-table-column>
    </el-table>

    <p class="page-hint">
      {{ t('keys.hintBefore') }}<span class="mono">*</span>{{ t('keys.hintAll') }}
      <span class="mono">namespace:*</span>{{ t('keys.hintNs') }}
    </p>
  </el-card>

  <el-dialog v-model="showAdd" :title="t('keys.dialogTitle')" width="min(560px, 94vw)" :close-on-click-modal="false">
    <el-form label-position="top">
      <el-form-item :label="t('keys.keyLabel')">
        <div class="key-row">
          <el-input v-model="form.key" class="mono" />
          <el-button @click="form.key = generateKey()">{{ t('keys.regenerate') }}</el-button>
        </div>
      </el-form-item>
      <el-form-item :label="t('keys.scopeLabel')">
        <el-select
          v-model="form.scope"
          filterable
          allow-create
          default-first-option
          :placeholder="t('keys.scopePlaceholder')"
          class="mono"
        >
          <el-option v-for="o in scopeOptions" :key="o.value" :label="o.label" :value="o.value" />
        </el-select>
      </el-form-item>
      <p class="hint-inline">
        {{ t('keys.scopeHintBefore') }}<span class="mono">*</span>{{ t('keys.scopeHintAll') }}<span class="mono">team:*</span>{{ t('keys.scopeHintNs') }}
        <span class="mono">team:office:mac</span>{{ t('keys.scopeHintOne') }}
      </p>
    </el-form>
    <template #footer>
      <el-button @click="showAdd = false">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" :loading="saving" @click="save">{{ t('common.add') }}</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
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
  line-height: 1.7;
  color: var(--el-text-color-secondary);
}
</style>
