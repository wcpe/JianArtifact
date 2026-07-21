// Vitest 配置：加载 jsdom 垫片（matchMedia/ResizeObserver），供 Mantine 组件测试使用。
// 各测试文件通过 `// @vitest-environment jsdom` docblock 选择环境；纯逻辑测试仍走默认 node。
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  test: {
    globals: true,
    setupFiles: ["./vitest.setup.ts"],
  },
});
