import { createApp } from 'vue'
import { createPinia } from 'pinia'
import ElementPlus from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import 'element-plus/dist/index.css'
// 深色模式需要单独一份 CSS 变量；不引入时 <html class="dark"> 只是换了类名、颜色不会变。
import 'element-plus/theme-chalk/dark/css-vars.css'
import * as ElementPlusIconsVue from '@element-plus/icons-vue'

import App from './App.vue'
import { router } from './router'
import './style.css'

const app = createApp(App)

// Pinia
app.use(createPinia())

// Vue Router
app.use(router)

// Element Plus
app.use(ElementPlus, { locale: zhCn })

// 全部图标
for (const [key, component] of Object.entries(ElementPlusIconsVue)) {
  app.component(key, component as any)
}

app.mount('#app')
