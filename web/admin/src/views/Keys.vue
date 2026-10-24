<script setup lang="ts">
// 认证密钥：按范围（* / namespace:* / 客户端名）决定哪些客户端能用哪把密钥。
import { onMounted, reactive, ref } from 'vue'
import { Plus, Refresh, CopyDocument } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { KeyRow } from '../api/types'
import { copyText } from '../lib/format'

const keys = ref<KeyRow[]>([])
const loadError = ref('')
const loading = ref(false)

const showAdd = ref(false)
const saving = ref(false)
const form = reactive({ key: '', scope: '*' })

const scopeOptions = [
  { value: '*', label: '* — 所有客户端' },
  { value: 'default:*', label: 'default:* — 该 namespace 下全部客户端' },
]

async function load() {
  loading.value = true
  try {
    const data = await api.keys()
    keys.value = data.keys
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载失败'
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
    ElMessage.warning('密钥至少 8 个字符')
    return
  }
  saving.value = true
  try {
    await api.keySave({ key: form.key.trim(), scope: form.scope.trim() || '*' })
    showAdd.value = false
    ElMessage.success('已保存，即时生效')
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    saving.value = false
  }
}

async function remove(k: KeyRow) {
  try {
    await ElMessageBox.confirm(`删除密钥 ${k.key}？删除后该密钥立即失效。`, '删除密钥', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api.keyDelete(k.key)
    ElMessage.success('已删除')
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '删除失败')
  }
}

async function copy(value: string) {
  if (await copyText(value)) ElMessage.success('已复制到剪贴板')
  else ElMessage.error('复制失败')
}

onMounted(load)
</script>

<template>
  <el-card shadow="never" class="stack">
    <template #header>
      <div class="toolbar">
        <strong>共 {{ keys.length }} 条密钥</strong>
        <span class="faint">配置后以本页为准，即时生效</span>
        <div class="spacer" />
        <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
        <el-button type="primary" :icon="Plus" @click="openAdd">添加密钥</el-button>
      </div>
    </template>

    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-table
      v-else
      v-loading="loading"
      :data="keys"
      size="small"
      row-key="key"
      empty-text="还没有配置任何密钥。未配置时客户端认证按服务端配置文件里的 auth.key 校验（也没有则放行）；在这里添加密钥后，以本页为准。"
    >
      <el-table-column label="范围" width="170">
        <template #default="{ row }">
          <el-tag v-if="row.scope === '*'" size="small" type="info" effect="plain">全部客户端</el-tag>
          <span v-else class="mono">{{ row.scope }}</span>
        </template>
      </el-table-column>

      <el-table-column label="密钥" min-width="300">
        <template #default="{ row }">
          <div class="key-cell">
            <span class="mono ellipsis" :title="row.key">{{ row.key }}</span>
            <el-tooltip content="复制密钥" placement="top">
              <el-icon class="copy-btn" @click="copy(row.key)"><CopyDocument /></el-icon>
            </el-tooltip>
          </div>
        </template>
      </el-table-column>

      <el-table-column label="创建时间" width="160">
        <template #default="{ row }"><span class="faint">{{ row.created_at }}</span></template>
      </el-table-column>

      <el-table-column label="更新时间" width="160">
        <template #default="{ row }"><span class="faint">{{ row.updated_at }}</span></template>
      </el-table-column>

      <el-table-column label="操作" width="120" align="right" fixed="right">
        <template #default="{ row }">
          <el-button link type="primary" @click="copy(row.key)">复制</el-button>
          <el-button link type="danger" @click="remove(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <p class="page-hint">
      范围决定密钥可供哪些客户端使用：<span class="mono">*</span> 所有客户端；
      <span class="mono">namespace:*</span> 该 namespace 下的客户端；具体客户端名则只有它能用。
      密钥唯一，同密钥再次提交即修改范围；已在线会话不受影响。
    </p>
  </el-card>

  <el-dialog v-model="showAdd" title="添加密钥" width="min(560px, 94vw)" :close-on-click-modal="false">
    <el-form label-position="top">
      <el-form-item label="密钥（至少 8 个字符）">
        <div class="key-row">
          <el-input v-model="form.key" class="mono" />
          <el-button @click="form.key = generateKey()">重新生成</el-button>
        </div>
      </el-form-item>
      <el-form-item label="范围">
        <el-select
          v-model="form.scope"
          filterable
          allow-create
          default-first-option
          placeholder="* 或 namespace:* 或 namespace:group:name"
          class="mono"
        >
          <el-option v-for="o in scopeOptions" :key="o.value" :label="o.label" :value="o.value" />
        </el-select>
      </el-form-item>
      <p class="hint-inline">
        范围可以手填：<span class="mono">*</span> 全部、<span class="mono">team:*</span> 某 namespace、
        <span class="mono">team:office:mac</span> 单个客户端。
      </p>
    </el-form>
    <template #footer>
      <el-button @click="showAdd = false">取消</el-button>
      <el-button type="primary" :loading="saving" @click="save">添加</el-button>
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
