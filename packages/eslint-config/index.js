// 前端共享严格 ESLint 扁平配置：@eslint/js 推荐集 + typescript-eslint 推荐集。
// 以 --max-warnings 0 运行，警告即失败；生成产物与构建输出不纳入检查。
// 参考：https://typescript-eslint.io/getting-started
import js from "@eslint/js";
import tseslint from "typescript-eslint";

export default [
  {
    ignores: ["dist/**", "coverage/**", "node_modules/**", "**/*.gen.ts", ".turbo/**"],
  },
  js.configs.recommended,
  ...tseslint.configs.recommended,
];
