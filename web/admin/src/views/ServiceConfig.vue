<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, ApiError } from '../api/client'
import type { ServiceConfig } from '../api/types'

const form = ref<ServiceConfig | null>(null)
const loadError = ref('')
const saving = ref(false)

async function load() {
  try {
    form.value = await api.serviceConfig()
    loadError.value = ''
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载失败'
  }
}

async function save() {
  if (!form.value) return
  saving.value = true
  try {
    await api.serviceConfigSave(form.value)
    ElMessage.success('已保存并热生效')
    await load()
  } catch (err) {
    ElMessage.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <el-alert v-if="loadError" :title="loadError" type="error" :closable="false" show-icon />

  <el-form v-else-if="form" class="stack" label-position="top" @submit.prevent="save">
    <p class="page-hint">
      这里只放运行期可在线修改并即时生效的服务端参数。改动落库到数据库，重启后仍生效。
      需改监听地址、管理端口、存储等要重启的配置，请编辑配置文件。
    </p>

    <el-card shadow="never" class="config-card">
      <template #header>
        <div class="card-title">对外分享主机</div>
      </template>
      <el-form-item label="public_host（vmess/vless 分享链接对外暴露的裸主机，可留空）" class="w-md">
        <el-input v-model="form.public_host" placeholder="vpn.example.com" />
      </el-form-item>
      <p class="page-hint">留空时退回管理台请求的 Host；管理台只监听回环地址，那样生成的链接仅本机可用。</p>
    </el-card>

    <el-card shadow="never" class="config-card">
      <template #header>
        <div class="card-title">本机出口（server_proxy）</div>
      </template>
      <el-form-item label="启用本机出口（保留名 server:self）">
        <el-switch v-model="form.server_proxy_enabled" />
      </el-form-item>
      <p class="page-hint" style="margin-bottom: 14px">
        启用后服务端自身成为一个可绑定的出口：把用户绑定到 <span class="mono">server:self</span> 即由服务端本机直连出站。
        启用时会在客户端页登记一行「本机」，停用时移除。
      </p>
      <div class="form-row">
        <el-form-item label="public（允许出站网段白名单，逗号分隔；空 = 不限制）">
          <el-input v-model="form.server_proxy_public" placeholder="192.168.31.*, fd00::/64" />
        </el-form-item>
        <el-form-item label="private（禁止出站网段黑名单，优先级高于 public）">
          <el-input v-model="form.server_proxy_private" />
        </el-form-item>
      </div>
      <el-form-item label="routing（出站与规则，YAML；留空用内置默认）">
        <el-input v-model="form.server_proxy_routing" type="textarea" :rows="10" class="code-area" />
      </el-form-item>
    </el-card>

    <el-card shadow="never" class="config-card">
      <template #header>
        <div class="card-title">心跳</div>
      </template>
      <el-form-item label="启用心跳探测（影响客户端的「延迟」列）">
        <el-switch v-model="form.heartbeat_enabled" />
      </el-form-item>
      <div class="form-row narrow">
        <el-form-item label="探测间隔（如 5s）">
          <el-input v-model="form.heartbeat_interval" />
        </el-form-item>
        <el-form-item label="单次等待上限（如 3s）">
          <el-input v-model="form.heartbeat_timeout" />
        </el-form-item>
      </div>
      <p class="page-hint">心跳只用于测往返时延，不负责断连判定（断连由多路复用 keepalive 兜底）。改动即时作用于在线会话。</p>
    </el-card>

    <el-card shadow="never" class="config-card">
      <template #header>
        <div class="card-title">日志</div>
      </template>
      <div class="form-row narrow">
        <el-form-item label="全局日志级别">
          <el-select v-model="form.log_level">
            <el-option label="debug" value="debug" />
            <el-option label="info" value="info" />
            <el-option label="warn" value="warn" />
            <el-option label="error" value="error" />
          </el-select>
        </el-form-item>
        <el-form-item label="落库周期 / 流量记账（如 10s）">
          <el-input v-model="form.traffic_flush_interval" />
        </el-form-item>
      </div>
      <div class="form-row narrow">
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
      <p v-if="!form.log_to_file" class="page-hint">
        当前日志未输出到文件（输出到终端），轮转参数不生效；改配置文件的 log.output 指向文件后才需要它们。
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
        style="margin-bottom: 14px"
        title="一条链路的两端必须一致"
        description="客户端侧目前固定用内置默认值，只改服务端会与客户端不对称，可能影响存活检测；改动只对之后建立的新连接生效。不确定时请保持默认值。"
      />
      <div class="form-row narrow">
        <el-form-item label="keepalive 探测间隔（如 10s，0 = 默认）">
          <el-input v-model="form.keepalive_interval" />
        </el-form-item>
        <el-form-item label="keepalive 等待上限（如 30s，0 = 默认）">
          <el-input v-model="form.keepalive_timeout" />
        </el-form-item>
        <el-form-item label="单流窗口（KiB，最小 256，0 = 默认）">
          <el-input-number v-model="form.max_stream_window_kb" :min="0" controls-position="right" style="width: 100%" />
        </el-form-item>
      </div>
      <div class="form-row narrow">
        <el-form-item label="入站流积压上限（0 = 默认）">
          <el-input-number v-model="form.accept_backlog" :min="0" controls-position="right" style="width: 100%" />
        </el-form-item>
        <el-form-item label="开流确认上限（如 75s，0 = 不限）">
          <el-input v-model="form.stream_open_timeout" />
        </el-form-item>
      </div>
    </el-card>

    <div class="toolbar">
      <el-button type="primary" :loading="saving" @click="save">保存</el-button>
    </div>
  </el-form>
</template>

<style scoped>
.stack {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1080px;
}

.page-hint {
  font-size: 12px;
  line-height: 1.6;
  color: var(--el-text-color-secondary);
}

.config-card :deep(.el-card__header) {
  padding: 12px 20px;
  background: var(--el-fill-color-light);
}

.card-title {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 15px;
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

.config-card :deep(.el-card__body) {
  padding-top: 18px;
}

.form-row {
  display: flex;
  flex-wrap: wrap;
  gap: 0 16px;
}

.form-row > .el-form-item {
  flex: 1;
  min-width: 220px;
  max-width: 460px;
}

.form-row.narrow > .el-form-item {
  flex: 0 1 auto;
  width: 240px;
  min-width: 0;
}

.form-row .el-select {
  width: 100%;
}

.w-md {
  max-width: 420px;
}

.toolbar {
  display: flex;
  align-items: center;
}

.code-area :deep(textarea) {
  font-family: var(--mono);
  font-size: 12px;
  line-height: 1.6;
}
</style>
