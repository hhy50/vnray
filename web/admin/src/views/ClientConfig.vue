<script setup lang="ts">
// 客户端配置：只维护下发给所有远程客户端的全局 Host 与 IP 段规则。
// 单台客户端的独立规则在「客户端」页的弹窗里编辑，两者不共用保存按钮。
import { computed, onMounted, ref } from 'vue'
import { Delete, Plus, RefreshLeft } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { GlobalClientConfig } from '../api/types'

const form = ref<GlobalClientConfig | null>(null)
const loadError = ref('')
const loading = ref(false)
const saving = ref(false)

// 快照用来判断"有没有改过"：只有脏表单才允许保存，避免误触把全局规则整体覆盖。
const snapshot = ref('')

const dirty = computed(() => !!form.value && JSON.stringify(form.value) !== snapshot.value)

const hostCount = computed(() => form.value?.custom_hosts.length ?? 0)
const mapCount = computed(() => form.value?.ip_maps.length ?? 0)

async function load() {
  loading.value = true
  try {
    const data = await api.clientConfig()
    form.value = data.global_client
    snapshot.value = JSON.stringify(data.global_client)
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载失败'
  } finally {
    loading.value = false
  }
}

async function save() {
  if (!form.value) return
  saving.value = true
  try {
    await api.clientConfigSave({ global_client: form.value })
    snapshot.value = JSON.stringify(form.value)
    ElMessage.success('已保存并下发给在线客户端')
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    saving.value = false
  }
}

function reset() {
  if (!snapshot.value) return
  form.value = JSON.parse(snapshot.value) as GlobalClientConfig
}

onMounted(load)
</script>

<template>
  <div class="client-config-page">
    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-form
      v-else-if="form"
      v-loading="loading"
      class="config-form"
      label-position="top"
      @submit.prevent="save"
    >
      <p class="page-hint intro">
        这里的规则下发给所有远程 vnray 客户端：在线客户端立即更新，离线客户端下次连接时生效，服务端重启后依然保留。
        客户端先匹配自己的独立配置，未命中才使用这里的全局配置；命中后不再匹配或改写。
      </p>

      <el-card shadow="never" class="config-card">
        <template #header>
          <div class="card-head">
            <span class="card-title">自定义 Host</span>
            <span class="faint">{{ hostCount }} 条</span>
            <div class="spacer" />
            <el-button size="small" :icon="Plus" @click="form.custom_hosts.push({ domain: '', ip: '' })">
              添加 Host
            </el-button>
          </div>
        </template>
        <el-table :data="form.custom_hosts" size="small" empty-text="尚未配置 Host：域名按客户端自己的 DNS 解析">
          <el-table-column label="域名" min-width="240">
            <template #default="{ row }">
              <el-input v-model="row.domain" placeholder="vpn.example.com" />
            </template>
          </el-table-column>
          <el-table-column label="固定 IP" min-width="200">
            <template #default="{ row }">
              <el-input v-model="row.ip" placeholder="192.168.60.1" class="mono" />
            </template>
          </el-table-column>
          <el-table-column width="60" align="right">
            <template #default="{ $index }">
              <el-button
                link
                type="danger"
                :icon="Delete"
                aria-label="删除 Host"
                @click="form.custom_hosts.splice($index, 1)"
              />
            </template>
          </el-table-column>
        </el-table>
        <p class="field-tip">
          域名由代理请求带到客户端才会命中；浏览器把单段名称当搜索词、或提前在本地解析时，这条规则不参与。
        </p>
      </el-card>

      <el-card shadow="never" class="config-card">
        <template #header>
          <div class="card-head">
            <span class="card-title">IP 段映射</span>
            <span class="faint">{{ mapCount }} 条</span>
            <div class="spacer" />
            <el-button size="small" :icon="Plus" @click="form.ip_maps.push({ from: '', to: '' })">
              添加映射
            </el-button>
          </div>
        </template>
        <el-table :data="form.ip_maps" size="small" empty-text="尚未配置 IP 段映射">
          <el-table-column label="源段" min-width="240">
            <template #default="{ row }">
              <el-input v-model="row.from" placeholder="186.12.12.*" class="mono" />
            </template>
          </el-table-column>
          <el-table-column label="目标段" min-width="200">
            <template #default="{ row }">
              <el-input v-model="row.to" placeholder="192.168.60.*" class="mono" />
            </template>
          </el-table-column>
          <el-table-column width="60" align="right">
            <template #default="{ $index }">
              <el-button
                link
                type="danger"
                :icon="Delete"
                aria-label="删除映射"
                @click="form.ip_maps.splice($index, 1)"
              />
            </template>
          </el-table-column>
        </el-table>
        <p class="field-tip">
          每段可填写 0–255 或 <span class="mono">*</span>，目标中的 <span class="mono">*</span> 保留原 IP 的对应段；
          同一层内取首条命中结果。清空后保存只撤回全局规则，各客户端的独立配置不受影响。
        </p>
      </el-card>

      <div class="save-bar">
        <span class="faint">{{ dirty ? '有未保存的改动' : '已与数据库一致' }}</span>
        <div class="spacer" />
        <el-button :icon="RefreshLeft" :disabled="!dirty" @click="reset">撤销</el-button>
        <el-button type="primary" :loading="saving" :disabled="!dirty" @click="save">
          保存并下发
        </el-button>
      </div>
    </el-form>
  </div>
</template>

<style scoped>
.client-config-page {
  max-width: 1080px;
}

.config-form {
  display: flex;
  flex-direction: column;
  gap: 16px;
  padding-bottom: 8px;
}

.intro {
  margin: 0;
  padding: 0;
  border-top: none;
}

.config-card :deep(.el-card__body) {
  padding-top: 12px;
}

.card-head {
  display: flex;
  align-items: center;
  gap: 8px;
}

.card-title {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 14px;
  font-weight: 600;
  color: var(--el-text-color-primary);
}

.card-title::before {
  content: '';
  width: 3px;
  height: 14px;
  border-radius: 2px;
  background: var(--el-color-primary);
}

.field-tip {
  margin: 12px 0 0;
  font-size: 12px;
  line-height: 1.75;
  color: var(--el-text-color-secondary);
}

/* 保存条吸附在内容区底部：长表单滚到哪都能提交 */
.save-bar {
  position: sticky;
  bottom: 0;
  z-index: 5;
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 4px;
  padding: 12px 16px;
  background: var(--el-bg-color);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 10px;
  box-shadow: 0 -6px 16px rgba(0, 0, 0, 0.05);
}
</style>
