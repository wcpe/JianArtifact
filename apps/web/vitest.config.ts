// Vitest 配置：jsdom 环境 + 全局装置（MSW Node server 与 jsdom 垫片）。
import { availableParallelism } from "node:os";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./test/setup.ts"],
    // ViteMockIsolation 会 spawn 一次**真实 vite build**（60–120s，取决于 CPU 争抢），
    // 与 281 个单元测试并行时既拖慢整体、又因抢不到 CPU 而超时（多次实测 120s 撞线）。
    // 它本质是构建集成验证而非单元测试，故移出并行套件，由 check.sh 在单元测试**之后**
    // 串行单独执行（那时 CPU 空闲）。
    exclude: ["**/node_modules/**", "**/dist/**", "test/ViteMockIsolation.test.ts"],
    // 并行全量跑时，含图表（recharts）的套件在低配/负载下可能逼近默认 5s 阈值；
    // 放宽到 15s 避免偶发超时误报（单个用例仍会稳定失败，不掩盖回归）。
    testTimeout: 15_000,
    hookTimeout: 15_000,
    // 并发上限：默认按 CPU 数开 fork（本机 32 核 → 31 个），而每个 fork 都要建 jsdom、
    // 加载 Mantine 8 与中英双语资源（各 1093 键）。实测 31 个并发时内存/CPU 峰值过高，
    // 会把需要渲染与请求的用例拖过等待上限，出现"找不到文本/构建超时"这类**负载敏感抖动**；
    // 本机限到 16 后全量稳定通过。
    // 注意 maxWorkers 是**直接设定的并发上限**，vitest 不会按 CPU 数夹取：写死 16 会让 4 核
    // CI 也开 16 个 fork、互相抢 CPU——实测 CI 上 14 个用例因此撞 15s 超时而全红。故取
    // min(16, CPU−1)：CI 回到 3 个，本机仍是 16。
    maxWorkers: Math.min(16, Math.max(1, availableParallelism() - 1)),
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
