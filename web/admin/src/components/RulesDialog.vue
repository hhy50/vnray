<script setup lang="ts">
// 出站规则：规则定义与判定都在服务端，用户端只引用规则名。
// 这里是用户页的「规则列表」弹窗——规则本身不占独立菜单。
import { computed, reactive, ref, watch } from 'vue'
import { Plus } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { OutboundRule } from '../api/types'
import { useI18n } from '../i18n'

const props = defineProps<{ modelValue: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [boolean]; changed: [] }>()

const { t } = useI18n()

const visible = computed({
  get: () => props.modelValue,
  set: (v: boolean) => emit('update:modelValue', v),
})

const rules = ref<OutboundRule[]>([])
const loading = ref(false)
const loadError = ref('')

const showEdit = ref(false)
const saving = ref(false)
// 新建时名字可编辑；编辑已有规则时名字固定（改名等价于新建 + 删除）。
const editing = ref<{ name: string } | null>(null)
const form = reactive({ name: '', reject: [] as string[], direct: [] as string[] })

const title = computed(() =>
  editing.value ? t('rules.editTitle', { name: editing.value.name }) : t('rules.addTitle'),
)

async function load() {
  loading.value = true
  try {
    const data = await api.rules()
    rules.value = data.rules
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : t('common.loadFailed')
  } finally {
    loading.value = false
  }
}

// 每次打开都重新取：用户页可能刚从别处改过规则。
watch(visible, (open) => {
  if (open) void load()
})

function openAdd() {
  editing.value = null
  form.name = ''
  form.reject = []
  form.direct = []
  showEdit.value = true
}

function openEdit(row: OutboundRule) {
  editing.value = { name: row.name }
  form.name = row.name
  form.reject = [...row.reject]
  form.direct = [...row.direct]
  showEdit.value = true
}

async function submit() {
  const name = form.name.trim()
  if (!name) {
    ElMessage.warning(t('rules.needName'))
    return
  }
  saving.value = true
  try {
    await api.ruleSave(name, { reject: form.reject, direct: form.direct })
    showEdit.value = false
    ElMessage.success(t('rules.saved'))
    await load()
    emit('changed')
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.saveFailed'))
  } finally {
    saving.value = false
  }
}

async function removeRule(row: OutboundRule) {
  try {
    await ElMessageBox.confirm(t('rules.deleteConfirm', { name: row.name }), t('rules.deleteTitle'), {
      type: 'warning',
      confirmButtonText: t('common.delete'),
      cancelButtonText: t('common.cancel'),
    })
  } catch {
    return
  }
  try {
    await api.ruleDelete(row.name)
    ElMessage.success(t('common.deleted'))
    await load()
    emit('changed')
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : t('common.deleteFailed'))
  }
}
</script>

<template>
  <el-dialog v-model="visible" :title="t('rules.dialogTitle')" width="min(920px, 96vw)">
    <div class="toolbar">
      <strong>{{ t('rules.count', { n: rules.length }) }}</strong>
      <div class="spacer" />
      <el-button :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
      <el-button type="primary" :icon="Plus" @click="openAdd">{{ t('rules.add') }}</el-button>
    </div>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />
    <template v-else>
      <el-alert :title="t('rules.intro')" type="info" :closable="false" show-icon class="intro" />
      <el-table v-loading="loading" :data="rules" size="small" max-height="420" :empty-text="t('rules.empty')">
        <el-table-column :label="t('rules.colName')" min-width="140">
          <template #default="{ row }">
            <span class="mono">{{ row.name }}</span>
          </template>
        </el-table-column>

        <el-table-column :label="t('rules.colReject')" min-width="220">
          <template #default="{ row }">
            <div v-if="row.reject.length" class="tags">
              <el-tag v-for="item in row.reject" :key="item" size="small" type="danger" effect="plain">
                {{ item }}
              </el-tag>
            </div>
            <span v-else class="faint">{{ t('rules.none') }}</span>
          </template>
        </el-table-column>

        <el-table-column :label="t('rules.colDirect')" min-width="220">
          <template #default="{ row }">
            <div v-if="row.direct.length" class="tags">
              <el-tag v-for="item in row.direct" :key="item" size="small" type="success" effect="plain">
                {{ item }}
              </el-tag>
            </div>
            <span v-else class="faint">{{ t('rules.none') }}</span>
          </template>
        </el-table-column>

        <el-table-column :label="t('rules.colActions')" width="150" align="right" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="openEdit(row)">{{ t('common.edit') }}</el-button>
            <el-button link type="danger" @click="removeRule(row)">{{ t('common.delete') }}</el-button>
          </template>
        </el-table-column>
      </el-table>
    </template>

    <template #footer>
      <el-button @click="visible = false">{{ t('common.close') }}</el-button>
    </template>
  </el-dialog>

  <el-dialog
    v-model="showEdit"
    :title="title"
    width="min(640px, 94vw)"
    append-to-body
    :close-on-click-modal="false"
  >
    <el-form label-position="top" @submit.prevent="submit">
      <el-form-item :label="t('rules.nameLabel')">
        <el-input v-model="form.name" :disabled="!!editing" class="mono" :placeholder="t('rules.namePlaceholder')" />
      </el-form-item>
      <el-form-item :label="t('rules.rejectLabel')">
        <el-select
          v-model="form.reject"
          multiple
          filterable
          allow-create
          default-first-option
          :reserve-keyword="false"
          :placeholder="t('rules.listPlaceholder')"
          style="width: 100%"
        />
      </el-form-item>
      <el-form-item :label="t('rules.directLabel')">
        <el-select
          v-model="form.direct"
          multiple
          filterable
          allow-create
          default-first-option
          :reserve-keyword="false"
          :placeholder="t('rules.listPlaceholder')"
          style="width: 100%"
        />
      </el-form-item>
      <p class="hint-inline">{{ t('rules.entriesHint') }}</p>
    </el-form>
    <template #footer>
      <el-button @click="showEdit = false">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" :loading="saving" @click="submit">{{ t('common.save') }}</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}

.toolbar .spacer {
  flex: 1;
}

.intro {
  margin-bottom: 12px;
}

.tags {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}

.hint-inline {
  margin: 0;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
</style>
