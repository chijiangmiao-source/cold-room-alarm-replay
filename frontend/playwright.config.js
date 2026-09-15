import { defineConfig } from '@playwright/test'

// Playwright 自带 dev server：vite 已把 /api 代理到 Go API（API_PORT，默认 8080）。
const webPort = Number(process.env.WEB_PORT || 5173)

export default defineConfig({
  testDir: './e2e',
  timeout: 60_000,
  fullyParallel: false,
  // 所有 spec 共享同一个 API/数据库，且断言涉及全局序号连续性，
  // 必须单 worker 串行，跨文件也不能并发提交事件。
  workers: 1,
  reporter: [['list']],
  use: {
    baseURL: `http://localhost:${webPort}`,
  },
  webServer: process.env.PLAYWRIGHT_NO_WEBSERVER
    ? undefined
    : {
        command: 'npm run dev',
        url: `http://localhost:${webPort}`,
        timeout: 60_000,
        reuseExistingServer: true,
      },
})
