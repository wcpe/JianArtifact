// 应用根：共享 AppProvider（Mantine 主题）+ 确认弹窗 + 通知 + 验收站画廊。
// 复用 packages/ui 的 AppProvider，验证共享主题在验收站与管理端一致；
// ModalsProvider 与管理端一致，用于核验「危险操作确认弹窗」业务模式。
import { ModalsProvider } from "@mantine/modals";
import { Notifications } from "@mantine/notifications";
import { AppProvider } from "@jianartifact/ui";

import { Gallery } from "./Gallery";

export function App() {
  return (
    <AppProvider>
      <ModalsProvider>
        <Notifications position="top-right" />
        <Gallery />
      </ModalsProvider>
    </AppProvider>
  );
}
