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

const root = document.getElementById('root');
if (!root) {
  throw new Error('未找到挂载节点 #root');
}

createRoot(root).render(
  <StrictMode>
    <MantineProvider defaultColorScheme="auto">
      <Notifications />
      <BrowserRouter>
        <AuthProvider>
          <App />
        </AuthProvider>
      </BrowserRouter>
    </MantineProvider>
  </StrictMode>,
);
