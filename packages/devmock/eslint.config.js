// devmock 的 ESLint 入口：复用共享严格配置，并为本包内的构建期 Node 脚本
// （scripts/*.mjs，如 emit-licenses.mjs）声明 Node 全局对象。
import config from "@jianartifact/eslint-config";

export default [
  ...config,
  {
    files: ["scripts/**/*.mjs"],
    languageOptions: {
      globals: { console: "readonly", process: "readonly" },
    },
  },
];
