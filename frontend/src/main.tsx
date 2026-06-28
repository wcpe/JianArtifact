// 前端入口：挂载 Mantine 主题、通知、认证上下文与路由。

import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { MantineProvider } from '@mantine/core';
import { Notifications } from '@mantine/notifications';
import { BrowserRouter } from 'react-router-dom';
import '@mantine/core/styles.css';
import '@mantine/notifications/styles.css';
import './global.css';
import './i18n';
import { AuthProvider } from './auth/AuthContext';
import { App } from './App';
import { startMockRuntime } from './mock/runtime';
import { MockModeBadge } from './mock/MockModeBadge';

const root = document.getElementById('root');
if (!root) {
  throw new Error('未找到挂载节点 #root');
}

/**
 * 渲染入口：先按需启动运行时 Mock 模式（FR-119，默认关闭则立即返回、零影响），
 * 再挂载应用——确保 Mock 模式下 worker 在首个 API 请求前已就绪、拦截全部 /api/v1/*。
 */
async function bootstrap(): Promise<void> {
  await startMockRuntime();
  createRoot(root!).render(
    <StrictMode>
      <MantineProvider defaultColorScheme="auto">
        <Notifications />
        <BrowserRouter>
          <AuthProvider>
            <App />
          </AuthProvider>
        </BrowserRouter>
        <MockModeBadge />
      </MantineProvider>
    </StrictMode>,
  );
}

void bootstrap();
