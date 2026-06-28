// 登录流集成测试（FR-116，ADR-0035）：真实 AuthProvider + LoginPage 经 client.ts 走 MSW，
// 断言登录端点的真实请求 / 响应契约与会话恢复，替代对 endpoints 的手工打桩。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { MemoryRouter } from 'react-router-dom';
import { AuthProvider } from './AuthContext';
import { useAuth } from './useAuth';
import { LoginPage } from '../pages/LoginPage';
import { getToken, setToken } from '../api/client';

/** 探针：把当前登录用户名渲染出来，便于断言会话状态。 */
function AuthProbe() {
  const { user, loading } = useAuth();
  if (loading) return <div>loading</div>;
  return <div data-testid="who">{user ? user.username : 'anon'}</div>;
}

/** 在真实 AuthProvider 下渲染登录页 + 探针。 */
function renderApp() {
  return render(
    <MantineProvider>
      <MemoryRouter>
        <AuthProvider>
          <AuthProbe />
          <LoginPage />
        </AuthProvider>
      </MemoryRouter>
    </MantineProvider>,
  );
}

describe('登录流走 MSW 的契约强断言（FR-116）', () => {
  afterEach(() => vi.restoreAllMocks());

  it('正确凭据：POST /auth/login 成功 → 写入令牌、会话变为已登录', async () => {
    const user = userEvent.setup();
    renderApp();

    await waitFor(() => expect(screen.getByTestId('who')).toHaveTextContent('anon'));

    await user.type(screen.getByPlaceholderText('请输入用户名'), 'admin');
    await user.type(screen.getByPlaceholderText('请输入口令'), 'admin123');
    await user.click(screen.getByRole('button', { name: '登录' }));

    // 登录成功：真实令牌写入 client 存储、会话用户更新
    await waitFor(() => expect(getToken()).toBeTruthy());
    await waitFor(() => expect(screen.getByTestId('who')).toHaveTextContent('admin'));
  });

  it('错误凭据：MSW 返回 401 → 展示后端错误文案、不写入令牌', async () => {
    const user = userEvent.setup();
    renderApp();

    await user.type(screen.getByPlaceholderText('请输入用户名'), 'admin');
    await user.type(screen.getByPlaceholderText('请输入口令'), 'wrong-pass');
    await user.click(screen.getByRole('button', { name: '登录' }));

    expect(await screen.findByText('用户名或口令错误')).toBeInTheDocument();
    expect(getToken()).toBeNull();
  });

  it('已有有效令牌：首屏经 GET /me 恢复会话', async () => {
    // 先登录拿到一个真实 session 令牌写入存储，再渲染（模拟刷新后恢复）
    const { login } = await import('../api/endpoints');
    const resp = await login('admin', 'admin123');
    setToken(resp.access_token);

    renderApp();
    // AuthProvider 首屏据令牌探测 /me，恢复为已登录
    await waitFor(() => expect(screen.getByTestId('who')).toHaveTextContent('admin'));
  });
});
