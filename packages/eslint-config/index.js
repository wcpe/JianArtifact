// 前端共享严格 ESLint 扁平配置。随工具链就绪（M1/0.1.0）接入 typescript-eslint、
// react、react-hooks 等规则集；lint 以 --max-warnings 0 运行，警告即失败。
// 参考：https://eslint.org/docs/latest/use/configure/configuration-files
export default [
  {
    ignores: ["dist/**", "coverage/**", "node_modules/**"],
  },
];
