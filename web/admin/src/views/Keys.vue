<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { Plus, Refresh } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { KeyRow } from '../api/types'
import { copyText } from '../lib/format'

const keys = ref<KeyRow[]>([])
const loadError = ref('')
const loading = ref(false)

const showAdd = ref(false)
const saving = ref(false)
const form = reactive({ key: '', scope: '*' })

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
  saving.value = true
  try {
    await api.keySave({ key: form.key.trim(), scope: form.scope.trim() })
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

async function copy(k: KeyRow) {
  if (await copyText(k.key)) ElMessage.success('密钥已复制')
  else ElMessage.error('复制失败')
}

onMounted(load)
</script>

<template>
  <el-card shadow="never" class="stack">
    <template #header>
      <div class="toolbar">
        <strong>共 {{ keys.length }} 条密钥</strong>
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
      <el-table-column label="范围 key" min-width="200">
        <template #default="{ row }"><span class="mono">{{ row.scope }}</span></template>
      </el-table-column>

      <el-table-column label="密钥" min-width="320">
        <template #default="{ row }"><span class="mono">{{ row.key }}</span></template>
      </el-table-column>

      <el-table-column label="创建时间" width="170">
        <template #default="{ row }"><span class="faint">{{ row.created_at }}</span></template>
      </el-table-column>

      <el-table-column label="更新时间" width="170">
        <template #default="{ row }"><span class="faint">{{ row.updated_at }}</span></template>
      </el-table-column>

      <el-table-column label="操作" width="130" align="right">
        <template #default="{ row }">
          <el-button link type="primary" @click="copy(row)">复制</el-button>
          <el-button link type="danger" @click="remove(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <p class="page-hint">
      范围 key 约束密钥可供哪些客户端使用：<span class="mono">*</span> 表示所有客户端；
      <span class="mono">namespace:*</span> 表示该 namespace 下的客户端；填具体客户端名（namespace:group:name）则只有它能用。
      密钥唯一，同密钥再次提交即修改范围。改动写穿到持久存储、即时生效，已在线会话不受影响。
    </p>
  </el-card>

  <el-dialog v-model="showAdd" title="添加密钥" width="520px">
    <el-form label-position="top">
      <el-form-item label="密钥（至少 8 个字符）">
        <div style="display: flex; gap: 8px; width: 100%">
          <el-input v-model="form.key" class="mono" />
          <el-button @click="form.key = generateKey()">生成</el-button>
        </div>
      </el-form-item>
      <el-form-item label="范围 key">
        <el-input v-model="form.scope" placeholder="* 或 namespace:* 或 namespace:group:name" class="mono" />
      </el-form-item>
      <p class="page-hint">* = 所有客户端；namespace:* = 该 namespace 下的客户端；具体名字 = 仅该客户端。</p>
    </el-form>
    <template #footer>
      <el-button @click="showAdd = false">取消</el-button>
      <el-button type="primary" :loading="saving" @click="save">添加</el-button>
    </template>
  </el-dialog>
</template>
