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
    coverage: {
      // 覆盖率只产出、不做阈值门禁：存量覆盖水平未知，一上来设阈值会卡死质量门。
      provider: "v8",
      // 与后端 .tmp/go-coverage.out 对齐，统一落在仓库根 .tmp/（已被 .gitignore 忽略）。
      // 注意：路径相对 vitest root（apps/web），故需两级上溯才是仓库根。
      reportsDirectory: "../../.tmp/web-coverage",
      reporter: ["text", "json-summary"],
      // 排除不该计入的文件：测试自身、契约生成产物、预览夹具与仅开发态的控制台。
      exclude: [
        "node_modules/**",
        // MSW 的 service worker 是第三方运行时脚本，不属被测源码
        "public/**",
        "test/**",
        "src/**/*.test.{ts,tsx}",
        "src/mocks/**",
        "src/**/*.gen.ts",
        "**/*.config.{ts,js,mjs}",
        "dist/**",
      ],
    },
  },
});
