// 应用根：组合共享 Mantine Provider（含通知 / 弹窗）、路由与鉴权上下文。
import { AppProvider } from "@jianartifact/ui";
import { ModalsProvider } from "@mantine/modals";
import { Notifications } from "@mantine/notifications";
import { BrowserRouter } from "react-router-dom";

import { AppRoutes } from "./app/router";
import { AuthProvider } from "./auth/AuthContext";
import { LoginModalProvider } from "./auth/LoginModal";

export function App() {
  return (
    <AppProvider>
      <Notifications position="top-right" />
      <ModalsProvider>
        <AuthProvider>
          <LoginModalProvider>
            <BrowserRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
              <AppRoutes />
            </BrowserRouter>
          </LoginModalProvider>
        </AuthProvider>
      </ModalsProvider>
    </AppProvider>
  );
}
