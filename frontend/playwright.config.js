import { defineConfig } from '@playwright/test'

// Playwright 自带 dev server：vite 已把 /api 代理到 Go API（API_PORT，默认 8080）。
const webPort = Number(process.env.WEB_PORT || 5173)

export default defineConfig({
  testDir: './e2e',
  timeout: 60_000,
  fullyParallel: false,
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
