import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import AutoImport from 'unplugin-auto-import/vite'
import Components from 'unplugin-vue-components/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'

// 临时本地验证用配置：接口代理到本机已运行的 admin(8092)。
export default defineConfig({
  base: '/admin/ui/',
  plugins: [
    vue(),
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
      '/admin/ops': { target: 'http://127.0.0.1:8092', changeOrigin: false },
      '/admin/auth': { target: 'http://127.0.0.1:8092', changeOrigin: false },
    },
  },
})
