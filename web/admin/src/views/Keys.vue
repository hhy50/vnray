<script setup lang="ts">
// 认证密钥：按范围（* / group:* / 客户端名）决定哪些客户端能用哪把密钥。
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
const form = reactive({ scope: '*' })
const generatedKey = ref('')

const scopeOptions = computed(() => [
  { value: '*', label: t('keys.scopeAll') },
  { value: 'default:*', label: t('keys.scopeGroup') },
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
  generatedKey.value = ''
  form.scope = '*'
  showAdd.value = true
}

async function save() {
  if (saving.value || generatedKey.value) return
  saving.value = true
  try {
    const result = await api.keySave({ scope: form.scope.trim() || '*' })
    generatedKey.value = result.key
    form.scope = result.scope
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
      <span class="mono">group:*</span>{{ t('keys.hintGroup') }}
    </p>
  </el-card>

  <el-dialog v-model="showAdd" :title="t('keys.dialogTitle')" width="min(560px, 94vw)" :close-on-click-modal="false" :close-on-press-escape="!saving" :show-close="!saving">
    <el-form label-position="top">
      <el-form-item v-if="generatedKey" :label="t('keys.keyLabel')">
        <div class="key-row">
          <el-input :model-value="generatedKey" readonly class="mono" />
          <el-button :icon="CopyDocument" @click="copy(generatedKey)">{{ t('common.copy') }}</el-button>
        </div>
      </el-form-item>
      <el-form-item :label="t('keys.scopeLabel')">
        <el-select
          v-model="form.scope"
          :disabled="saving || !!generatedKey"
          filterable
          allow-create
          default-first-option
          :placeholder="t('keys.scopePlaceholder')"
          class="mono"
        >
          <el-option v-for="o in scopeOptions" :key="o.value" :label="o.label" :value="o.value" />
        </el-select>
      </el-form-item>
      <p class="hint-inline">{{ t('keys.generatedHint') }}</p>
      <p class="hint-inline">
        {{ t('keys.scopeHintBefore') }}<span class="mono">*</span>{{ t('keys.scopeHintAll') }}<span class="mono">office:*</span>{{ t('keys.scopeHintGroup') }}
        <span class="mono">office:mac</span>{{ t('keys.scopeHintOne') }}
      </p>
    </el-form>
    <template #footer>
      <el-button :disabled="saving" @click="showAdd = false">{{ t(generatedKey ? 'common.close' : 'common.cancel') }}</el-button>
      <el-button v-if="!generatedKey" type="primary" :loading="saving" @click="save">{{ t('keys.generate') }}</el-button>
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
