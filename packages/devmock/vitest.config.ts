// Vitest 配置：本包沿用 vitest 默认的 node 环境，只需放宽超时。
// 起因：契约测试要对整份 OpenAPI 逐个断言，单例耗时在负载下会超过 vitest 默认的 5s——
// 实测在负载高的机器上出现过 5–7s 的超时误报（同一用例单独复跑均在 1s 内通过），
// 而此前本包**没有**配置文件、吃的是默认 5s。放宽到 20s 留出余量：
// 真正的回归仍会稳定失败，不会因为放宽而被掩盖。
import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    testTimeout: 20_000,
    hookTimeout: 20_000,
  },
});
