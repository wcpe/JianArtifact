// 应用根：共享 AppProvider（Mantine 主题）+ 通知 + 验收站画廊。
// 复用 packages/ui 的 AppProvider，验证共享主题在验收站与管理端一致。
import { Notifications } from "@mantine/notifications";
import { AppProvider } from "@jianartifact/ui";

import { Gallery } from "./Gallery";

export function App() {
  return (
    <AppProvider>
      <Notifications position="top-right" />
      <Gallery />
    </AppProvider>
  );
}
