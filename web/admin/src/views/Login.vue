<script setup lang="ts">
// 登录页：独立布局，不走 AdminLayout，登录成功后回跳原地址。
import { reactive, ref } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { User, Lock, View, Hide } from '@element-plus/icons-vue'
import { useUserStore } from '../stores/user'
import { ElMessage } from 'element-plus'

const router = useRouter()
const route = useRoute()
const userStore = useUserStore()

const form = reactive({ username: '', password: '' })
const loading = ref(false)
const showPassword = ref(false)

async function onSubmit() {
  if (!form.username.trim()) {
    ElMessage.warning('请输入用户名')
    return
  }
  if (!form.password.trim()) {
    ElMessage.warning('请输入密码')
    return
  }
  loading.value = true
  try {
    await userStore.login(form.username.trim(), form.password)
    const redirect = route.query.redirect as string | undefined
    router.replace(redirect || '/clients')
  } catch (e) {
    ElMessage.error(e instanceof Error ? e.message : '登录失败')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="login-page">
    <section class="brand-panel">
      <div class="brand-inner">
        <div class="brand-logo">V</div>
        <h1>vnray</h1>
        <p class="brand-sub">流量调度管理台</p>
        <ul class="brand-points">
          <li>客户端接入与会话状态</li>
          <li>用户、限速与端口映射</li>
          <li>入站插件与服务配置</li>
        </ul>
      </div>
      <div class="brand-glow" />
    </section>

    <section class="form-panel">
      <div class="login-card">
        <header class="card-head">
          <div class="logo-mark">V</div>
          <div>
            <h2>登录管理台</h2>
            <p>使用管理员账号继续</p>
          </div>
        </header>

        <el-form :model="form" class="login-form" @keyup.enter="onSubmit">
          <el-form-item>
            <el-input
              v-model="form.username"
              placeholder="用户名"
              size="large"
              :prefix-icon="User"
              autocomplete="username"
            />
          </el-form-item>

          <el-form-item>
            <el-input
              v-model="form.password"
              :type="showPassword ? 'text' : 'password'"
              placeholder="密码"
              size="large"
              :prefix-icon="Lock"
              autocomplete="current-password"
            >
              <template #suffix>
                <el-icon class="password-icon" @click="showPassword = !showPassword">
                  <View v-if="!showPassword" />
                  <Hide v-else />
                </el-icon>
              </template>
            </el-input>
          </el-form-item>

          <el-button type="primary" size="large" class="login-btn" :loading="loading" @click="onSubmit">
            登 录
          </el-button>
        </el-form>

        <footer class="card-foot">
          <span>© {{ new Date().getFullYear() }} vnray</span>
          <span class="dot" />
          <span>流量调度系统</span>
        </footer>
      </div>
    </section>
  </div>
</template>

<style scoped>
.login-page {
  display: flex;
  min-height: 100vh;
  background: var(--el-bg-color-page);
}

.brand-panel {
  position: relative;
  flex: 1 1 46%;
  display: flex;
  align-items: center;
  justify-content: center;
  overflow: hidden;
  color: #fff;
  background: linear-gradient(150deg, #1f6feb 0%, #2f9bff 45%, #14b8a6 100%);
}

.brand-glow {
  position: absolute;
  width: 520px;
  height: 520px;
  right: -160px;
  bottom: -200px;
  border-radius: 50%;
  background: rgba(255, 255, 255, 0.16);
  filter: blur(2px);
}

.brand-inner {
  position: relative;
  z-index: 1;
  max-width: 320px;
  padding: 40px;
}

.brand-logo {
  width: 46px;
  height: 46px;
  border-radius: 12px;
  background: rgba(255, 255, 255, 0.18);
  border: 1px solid rgba(255, 255, 255, 0.35);
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 22px;
  font-weight: 700;
  margin-bottom: 22px;
}

.brand-inner h1 {
  margin: 0;
  font-size: 30px;
  letter-spacing: 0.5px;
}

.brand-sub {
  margin: 6px 0 26px;
  font-size: 13px;
  opacity: 0.85;
  letter-spacing: 1px;
}

.brand-points {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 10px;
  font-size: 13px;
  opacity: 0.9;
}

.brand-points li {
  padding-left: 16px;
  position: relative;
}

.brand-points li::before {
  content: '';
  position: absolute;
  left: 0;
  top: 7px;
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: rgba(255, 255, 255, 0.8);
}

.form-panel {
  flex: 1 1 54%;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 40px 24px;
}

.login-card {
  width: 100%;
  max-width: 400px;
  padding: 34px 32px 28px;
  background: var(--el-bg-color);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 14px;
  box-shadow: 0 12px 32px rgba(15, 23, 42, 0.06);
}

.card-head {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 28px;
}

.logo-mark {
  display: none;
  width: 42px;
  height: 42px;
  border-radius: 10px;
  background: linear-gradient(135deg, #409eff, #36cfc9);
  color: #fff;
  font-weight: 700;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}

.card-head h2 {
  margin: 0;
  font-size: 20px;
  font-weight: 600;
  color: var(--el-text-color-primary);
}

.card-head p {
  margin: 4px 0 0;
  font-size: 12.5px;
  color: var(--el-text-color-secondary);
}

.login-form :deep(.el-form-item) {
  margin-bottom: 18px;
}

.login-btn {
  width: 100%;
  height: 42px;
  font-size: 15px;
  letter-spacing: 2px;
  margin-top: 4px;
}

.password-icon {
  cursor: pointer;
  color: var(--el-text-color-placeholder);
}

.password-icon:hover {
  color: var(--el-color-primary);
}

.card-foot {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 28px;
  font-size: 11.5px;
  color: var(--el-text-color-placeholder);
}

.card-foot .dot {
  width: 3px;
  height: 3px;
  border-radius: 50%;
  background: currentColor;
}

@media (max-width: 860px) {
  .brand-panel {
    display: none;
  }

  .logo-mark {
    display: flex;
  }
}
</style>
