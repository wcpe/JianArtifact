// 应用根：组合共享 Mantine Provider（含通知 / 弹窗）、路由与鉴权上下文。
import { AppProvider } from "@jianartifact/ui";
import { ModalsProvider } from "@mantine/modals";
import { Notifications } from "@mantine/notifications";
import { Suspense, lazy } from "react";
import { BrowserRouter } from "react-router-dom";

import { AppRoutes } from "./app/router";
import { AuthProvider } from "./auth/AuthContext";
import { LoginModalProvider } from "./auth/LoginModal";

// 开发态 Mock 控制台（悬浮球）：懒加载 + DEV 条件，生产构建不会请求该 chunk。
const DevMockConsole = lazy(() =>
  import("./mocks/DevMockConsole").then((m) => ({ default: m.DevMockConsole })),
);

export function App() {
  return (
    <AppProvider>
      <Notifications position="top-right" />
      <ModalsProvider>
        <AuthProvider>
          <LoginModalProvider>
            <BrowserRouter
              // v7 起 `future` 开关已并入正式行为：`v7_startTransition` 移除、
              // `v7_relativeSplatPath` 成为默认，等价配置改为 `useTransitions`。
              // 仍明确关闭 transition（原 `v7_startTransition: false` 的同一决策）：
              // 开启后路由更新走 transition，React 会**保留旧页面内容**直到新页面 chunk
              // 就绪，期间没有任何反馈——慢网速下用户看到的就是"点了导航切不过去"。
              // 关闭后 Suspense 立即回退到页面骨架，配合 React.lazy 预取（app/preloadRoute）
              // 把骨架窗口压到最短。
              useTransitions={false}
            >
              <AppRoutes />
            </BrowserRouter>
            {import.meta.env.DEV ? (
              <Suspense fallback={null}>
                <DevMockConsole />
              </Suspense>
            ) : null}
          </LoginModalProvider>
        </AuthProvider>
      </ModalsProvider>
    </AppProvider>
  );
}
