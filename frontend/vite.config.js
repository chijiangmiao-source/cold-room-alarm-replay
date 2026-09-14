import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 本地联调：/api 与 /events 代理到 Go API（默认 8080，可用 API_PORT 覆盖）。
// Docker 内由 nginx 反代，见 nginx.conf。
const apiTarget = `http://localhost:${process.env.API_PORT || '8080'}`

export default defineConfig({
  plugins: [vue()],
  server: {
    host: true,
    port: Number(process.env.WEB_PORT || 5173),
    proxy: {
      '/api': { target: apiTarget, changeOrigin: true }
    }
  },
  preview: {
    host: true,
    port: Number(process.env.WEB_PORT || 5173),
    proxy: {
      '/api': { target: apiTarget, changeOrigin: true }
    }
  }
})
