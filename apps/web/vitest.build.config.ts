// 构建验证专用配置：只跑 ViteMockIsolation（spawn 一次**真实 vite build** 校验产物隔离）。
//
// 为什么独立：该用例耗时 60–120s（取决于 CPU 争抢），与 281 个单元测试并行时既拖慢整体、
// 又因抢不到 CPU 而撞 120s 超时（多次实测）。它本质是构建集成验证，故由 check.sh 在单元
// 测试**之后**用它单独串行执行——那时 CPU 空闲，实测 20–45s 即可完成。
// 环境用 node 而非 jsdom：它只依赖 node:fs / node:child_process / fetch，不需要 DOM。
import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    globals: true,
    environment: "node",
    include: ["test/ViteMockIsolation.test.ts"],
    // 真实 build 的等待上限（与用例内 timeout 一致）
    testTimeout: 180_000,
    hookTimeout: 30_000,
  },
});
