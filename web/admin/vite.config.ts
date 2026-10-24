import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import AutoImport from 'unplugin-auto-import/vite'
import Components from 'unplugin-vue-components/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'

// 产物挂在 /admin/ui/ 下；开发时只把接口前缀代理到后端，避免把 /admin/ui/ 自己代理走。
export default defineConfig({
  base: '/admin/ui/',
  plugins: [
    vue(),
    // Element Plus 按需引入：模板里的 <el-*>（含 v-loading 指令）与脚本里的
    // ElMessage/ElMessageBox 都按实际使用逐个引入，并自动带上该组件的样式。
    // 生成的 d.ts 入库，保证没有构建产物时 tsc 也能解析这些名字。
    AutoImport({ resolvers: [ElementPlusResolver()], dts: 'src/auto-imports.d.ts' }),
    Components({ resolvers: [ElementPlusResolver()], dts: 'src/components.d.ts', directives: true }),
  ],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    chunkSizeWarningLimit: 800,
  },
  server: {
    port: 5173,
    proxy: {
      '/admin/ops': { target: 'http://127.0.0.1:8080', changeOrigin: false },
      '/admin/auth': { target: 'http://127.0.0.1:8080', changeOrigin: false },
    },
  },
})
