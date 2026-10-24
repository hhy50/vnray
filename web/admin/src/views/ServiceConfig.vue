<script setup lang="ts">
// 服务配置：只收运行期可热生效的参数，保存即落库；需要重启的项留在配置文件里。
import { computed, onMounted, ref } from 'vue'
import { RefreshLeft } from '@element-plus/icons-vue'
import { api, ApiError } from '../api/client'
import type { ServiceConfig } from '../api/types'

const form = ref<ServiceConfig | null>(null)
const loadError = ref('')
const loading = ref(false)
const saving = ref(false)

// 快照用来判断"有没有改过"：只有脏表单才允许保存，避免误触把默认值写回库。
const snapshot = ref('')

const dirty = computed(() => !!form.value && JSON.stringify(form.value) !== snapshot.value)

async function load() {
  loading.value = true
  try {
    const data = await api.serviceConfig()
    form.value = data
    snapshot.value = JSON.stringify(data)
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
    await api.serviceConfigSave(form.value)
    snapshot.value = JSON.stringify(form.value)
    ElMessage.success('已保存并热生效')
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    saving.value = false
  }
}

function reset() {
  if (!snapshot.value) return
  form.value = JSON.parse(snapshot.value) as ServiceConfig
}

onMounted(load)
</script>

<template>
  <div class="config-page">
    <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

    <el-form v-else-if="form" v-loading="loading" class="config-form" label-position="top" @submit.prevent="save">
      <p class="page-hint intro">
        这里只放可以在线修改并即时生效的参数，改动落库、重启后依然生效。
        监听地址、管理端口、存储等需要重启的配置请编辑配置文件。
      </p>

      <el-card shadow="never" class="config-card">
        <template #header>
          <div class="card-title">对外分享主机</div>
        </template>
        <el-form-item label="public_host（vmess / vless 分享链接对外暴露的主机）" class="w-md">
          <el-input v-model="form.public_host" placeholder="vpn.example.com" />
        </el-form-item>
        <p class="field-tip">
          留空时退回管理台请求的 Host。管理台只监听回环地址时，生成的链接仅本机可用。
        </p>
      </el-card>

      <el-card shadow="never" class="config-card">
        <template #header>
          <div class="card-title">本机出口（server_proxy）</div>
        </template>
        <el-form-item label="启用本机出口（保留名 server:self）">
          <el-switch v-model="form.server_proxy_enabled" />
        </el-form-item>
        <p class="field-tip">
          启用后服务端自身成为可绑定的出口：把用户绑定到 <span class="mono">server:self</span> 即由本机直连出站。
          启用时客户端页会登记一行「本机」，停用时移除。
        </p>
        <div class="form-row">
          <el-form-item label="public（允许出站网段白名单，空 = 不限制）">
            <el-input v-model="form.server_proxy_public" placeholder="192.168.31.*, fd00::/64" class="mono" />
          </el-form-item>
          <el-form-item label="private（禁止出站网段黑名单，优先级更高）">
            <el-input v-model="form.server_proxy_private" class="mono" />
          </el-form-item>
        </div>
        <el-form-item label="routing（出站与规则，YAML；留空用内置默认）">
          <el-input v-model="form.server_proxy_routing" type="textarea" :rows="9" class="code-area" spellcheck="false" />
        </el-form-item>
      </el-card>

      <el-card shadow="never" class="config-card">
        <template #header>
          <div class="card-title">心跳</div>
        </template>
        <div class="form-row">
          <el-form-item label="启用心跳探测（影响客户端的「延迟」列）" class="switch-item">
            <el-switch v-model="form.heartbeat_enabled" />
          </el-form-item>
          <el-form-item label="探测间隔">
            <el-input v-model="form.heartbeat_interval" placeholder="5s" class="mono" />
          </el-form-item>
          <el-form-item label="单次等待上限">
            <el-input v-model="form.heartbeat_timeout" placeholder="3s" class="mono" />
          </el-form-item>
        </div>
        <p class="field-tip">心跳只用于测往返时延；断连判定由多路复用 keepalive 负责。改动即时作用于在线会话。</p>
      </el-card>

      <el-card shadow="never" class="config-card">
        <template #header>
          <div class="card-title">日志与记账</div>
        </template>
        <div class="form-row">
          <el-form-item label="全局日志级别">
            <el-select v-model="form.log_level">
              <el-option label="debug" value="debug" />
              <el-option label="info" value="info" />
              <el-option label="warn" value="warn" />
              <el-option label="error" value="error" />
            </el-select>
          </el-form-item>
          <el-form-item label="落库周期 / 流量记账">
            <el-input v-model="form.traffic_flush_interval" placeholder="10s" class="mono" />
          </el-form-item>
        </div>
        <div class="form-row">
          <el-form-item label="轮转单文件上限（MB）">
            <el-input-number v-model="form.log_rotate_max_size_mb" :min="1" controls-position="right" style="width: 100%" />
          </el-form-item>
          <el-form-item label="保留份数">
            <el-input-number v-model="form.log_rotate_max_backups" :min="0" controls-position="right" style="width: 100%" />
          </el-form-item>
          <el-form-item label="保留天数">
            <el-input-number v-model="form.log_rotate_max_age_days" :min="0" controls-position="right" style="width: 100%" />
          </el-form-item>
        </div>
        <p v-if="!form.log_to_file" class="field-tip">
          当前日志未写入文件（输出到终端），轮转参数不生效；把配置文件的 log.output 指向文件后才会用到。
        </p>
      </el-card>

      <el-card shadow="never" class="config-card">
        <template #header>
          <div class="card-title">多路复用会话参数</div>
        </template>
        <el-alert
          type="warning"
          :closable="false"
          show-icon
          class="mp-alert"
          title="一条链路的两端必须一致"
          description="客户端侧目前固定使用内置默认值，只改服务端会与客户端不对称，可能影响存活检测。改动只对之后建立的连接生效，不确定时保持默认。"
        />
        <div class="form-row">
          <el-form-item label="keepalive 探测间隔（0 = 默认）">
            <el-input v-model="form.keepalive_interval" placeholder="10s" class="mono" />
          </el-form-item>
          <el-form-item label="keepalive 等待上限（0 = 默认）">
            <el-input v-model="form.keepalive_timeout" placeholder="30s" class="mono" />
          </el-form-item>
          <el-form-item label="单流窗口 KiB（0 = 默认）">
            <el-input-number v-model="form.max_stream_window_kb" :min="0" controls-position="right" style="width: 100%" />
          </el-form-item>
          <el-form-item label="入站流积压上限（0 = 默认）">
            <el-input-number v-model="form.accept_backlog" :min="0" controls-position="right" style="width: 100%" />
          </el-form-item>
          <el-form-item label="开流确认上限（0 = 不限）">
            <el-input v-model="form.stream_open_timeout" placeholder="75s" class="mono" />
          </el-form-item>
        </div>
      </el-card>

      <div class="save-bar">
        <span class="faint">{{ dirty ? '有未保存的改动' : '已与数据库一致' }}</span>
        <div class="spacer" />
        <el-button :icon="RefreshLeft" :disabled="!dirty" @click="reset">撤销</el-button>
        <el-button type="primary" :loading="saving" :disabled="!dirty" @click="save">保存并生效</el-button>
      </div>
    </el-form>
  </div>
</template>

<style scoped>
.config-page {
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
  padding-top: 18px;
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
  margin: -6px 0 14px;
  font-size: 12px;
  line-height: 1.75;
  color: var(--el-text-color-secondary);
}

.form-row {
  display: flex;
  flex-wrap: wrap;
  gap: 0 16px;
}

.form-row > .el-form-item {
  flex: 1 1 220px;
  max-width: 320px;
}

.form-row > .switch-item {
  flex: 0 0 auto;
  max-width: none;
}

.form-row .el-select {
  width: 100%;
}

.w-md {
  max-width: 420px;
}

.mp-alert {
  margin-bottom: 16px;
}

.code-area :deep(textarea) {
  font-family: var(--mono);
  font-size: 12px;
  line-height: 1.65;
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
