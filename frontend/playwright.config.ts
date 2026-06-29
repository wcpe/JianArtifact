import { defineConfig, devices } from '@playwright/test';

// Playwright E2E 配置（FR-118，ADR-0036）。
//
// E2E 目标 = 前端 Mock 模式：webServer 先 `vite build` 再 `vite preview`（贴近生产产物、
// 含 service worker），并注入 env `VITE_MOCK=true`——由 FR-119/ADR-0035 的浏览器内有状态
// mock 后端拦截全部 /api/v1/*，全操作走内存 CRUD。故 E2E 自包含、无须起 Rust 后端、
// 种子数据固定、确定性强；仅 chromium 一个 project。
const PORT = 4173;

export default defineConfig({
  // E2E 规格目录（与 src 下的 vitest 单测分离；vite.config.ts 已把 e2e/** 从 vitest 排除）。
  testDir: './e2e',
  // CI 上禁止用例残留 test.only（防误提交只跑单条）。
  forbidOnly: !!process.env.CI,
  // CI 失败重试一次以容忍偶发抖动；本地不重试便于直面失败。
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    // 失败时留痕便于排障。
    trace: 'on-first-retry',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  // 启动被测前端：构建后用 vite preview 起静态产物，注入 VITE_MOCK=true 开启 Mock 模式。
  // 显式 --host 127.0.0.1：Windows 下 vite preview 默认绑 localhost（可能解析为 IPv6 ::1），
  // 而 webServer.url 探测 IPv4 127.0.0.1，host 不一致会导致就绪探测超时——绑定到 IPv4 对齐。
  webServer: {
    command: `npm run build && npm run preview -- --host 127.0.0.1 --port ${PORT} --strictPort`,
    url: `http://127.0.0.1:${PORT}`,
    env: { VITE_MOCK: 'true' },
    // 复用已在运行的实例（本地反复跑时省构建）；CI 始终全新起。
    reuseExistingServer: !process.env.CI,
    // 构建 + 起服务较慢，给足超时。
    timeout: 180_000,
  },
});
