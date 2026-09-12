// Vitest 配置：jsdom 环境 + 全局装置（MSW Node server 与 jsdom 垫片）。
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./test/setup.ts"],
    // 并行全量跑时，含图表（recharts）的套件在低配/负载下可能逼近默认 5s 阈值；
    // 放宽到 15s 避免偶发超时误报（单个用例仍会稳定失败，不掩盖回归）。
    testTimeout: 15_000,
    hookTimeout: 15_000,
  },
});
