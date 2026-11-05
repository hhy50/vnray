import { createApp } from 'vue'
import { createPinia } from 'pinia'
import ElementPlus from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import en from 'element-plus/es/locale/lang/en'
import 'element-plus/dist/index.css'
import 'element-plus/theme-chalk/dark/css-vars.css'
import * as ElementPlusIconsVue from '@element-plus/icons-vue'

import App from './App.vue'
import { router } from './router'
import { getLocale } from './i18n'
import './style.css'

const app = createApp(App)

app.use(createPinia())

app.use(router)

// 全局 locale 只是命令式组件（ElMessage 等）的初值；界面内组件的语言由
// App.vue 的 el-config-provider 按当前语言响应式注入。
app.use(ElementPlus, { locale: getLocale() === 'zh-CN' ? zhCn : en })

for (const [key, component] of Object.entries(ElementPlusIconsVue)) {
  app.component(key, component as any)
}

app.mount('#app')
