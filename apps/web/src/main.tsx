// 入口：加载 Mantine 样式与 i18n，开发态启动 MSW 后再挂载应用。
import "@mantine/core/styles.css";
import "@mantine/notifications/styles.css";
import "./global.css";

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { App } from "./App";
import "./i18n";
import { enableMocking } from "./mocks/enableMocking";

const container = document.getElementById("root");
if (!container) {
  throw new Error("未找到 #root 挂载点");
}

void enableMocking().then(() => {
  createRoot(container).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
});
