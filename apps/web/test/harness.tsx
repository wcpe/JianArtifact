// 测试渲染夹具：在共享 Provider（Mantine 主题 / i18n / 路由 / 鉴权）下挂载被测组件。
import { AppProvider } from "@jianartifact/ui";
import { ModalsProvider } from "@mantine/modals";
import { render } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { AuthProvider } from "../src/auth/AuthContext";
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
          <MemoryRouter
            initialEntries={[route]}
            future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
          >
            {children}
          </MemoryRouter>
        </AuthProvider>
      </ModalsProvider>
    </AppProvider>
  );
}

/** 在完整 Provider 链下渲染组件；authenticated=true 时预置令牌。 */
export function renderWithProviders(ui: ReactElement, options: RenderOptions = {}) {
  const { route = "/", authenticated = false } = options;
  if (authenticated) {
    setToken("mock.jwt.token");
  }
  return render(<Providers route={route}>{ui}</Providers>);
}
