import { tokens } from "@jianartifact/ui";

import { describeStatus } from "./status";

// 0.1.0 管理端骨架：仅验证 React + Vite + 工作区包（@jianartifact/ui）链路可构建可内嵌。
// Mantine 7 全量页面、i18next 与业务视图随 0.2.0 引入。
export function App() {
  return (
    <main style={{ fontFamily: "system-ui, sans-serif", padding: tokens.spacing.lg }}>
      <h1 style={{ color: tokens.brandColor }}>JianArtifact</h1>
      <p>自托管多格式制品仓库 · 管理端骨架（0.1.0）</p>
      <p>后端就绪状态：{describeStatus("ok")}</p>
    </main>
  );
}
