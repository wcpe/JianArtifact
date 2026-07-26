// 测试渲染夹具：在共享 Provider（Mantine 主题 / i18n / 路由 / 鉴权）下挂载被测组件。
import { AppProvider } from "@jianartifact/ui";
import { ModalsProvider } from "@mantine/modals";
import { render } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { AuthProvider } from "../src/auth/AuthContext";
import { LoginModalProvider } from "../src/auth/LoginModal";
import { setToken } from "../src/api/client";
import "../src/i18n";

interface RenderOptions {
  /** 初始路由。 */
  route?: string;
  /** 预置会话令牌以通过受保护端点鉴权。 */
  authenticated?: boolean;
}

function Providers({ children, route }: { children: ReactNode; route: string }) {
  return (
    <AppProvider>
      <ModalsProvider>
        <AuthProvider>
          <LoginModalProvider>
            <MemoryRouter
              initialEntries={[route]}
              future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
            >
              {children}
            </MemoryRouter>
          </LoginModalProvider>
        </AuthProvider>
      </ModalsProvider>
    </AppProvider>
  );
}

/** 与 devmock 种子一致的用户快照；AuthContext 从 localStorage 恢复会话。 */
const MOCK_USER = {
  id: 1,
  username: "admin",
  role: "admin",
  status: "active",
  createdAt: "2026-01-01T00:00:00Z",
};

/** 在完整 Provider 链下渲染组件；authenticated=true 时预置令牌与用户快照。 */
export function renderWithProviders(ui: ReactElement, options: RenderOptions = {}) {
  const { route = "/", authenticated = false } = options;
  if (authenticated) {
    setToken("mock.jwt.token");
    localStorage.setItem("jianartifact.user", JSON.stringify(MOCK_USER));
  } else {
    setToken(null);
    localStorage.removeItem("jianartifact.user");
  }
  return render(<Providers route={route}>{ui}</Providers>);
}
