// 登录页组件测试：渲染表单、提交触发登录、失败展示错误文案。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { MemoryRouter } from 'react-router-dom';
import { LoginPage } from './LoginPage';
import { AuthContext, type AuthContextValue } from '../auth/AuthContext';

/** 在 Mantine + Router + 注入的认证上下文下渲染登录页。 */
function renderLogin(ctx: Partial<AuthContextValue>) {
  const value: AuthContextValue = {
    user: null,
    loading: false,
    isAdmin: false,
    signIn: vi.fn(),
    signOut: vi.fn(),
    ...ctx,
  };
  return render(
    <MantineProvider>
      <MemoryRouter>
        <AuthContext.Provider value={value}>
          <LoginPage />
        </AuthContext.Provider>
      </MemoryRouter>
    </MantineProvider>,
  );
}

describe('LoginPage', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.restoreAllMocks());

  it('渲染用户名与口令输入框及登录按钮', () => {
    renderLogin({});
    // 经占位文案定位输入框，避免依赖 Mantine 生成的 label↔input id 关联
    expect(screen.getByPlaceholderText('请输入用户名')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('请输入口令')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '登录' })).toBeInTheDocument();
  });

  it('提交表单调用 signIn 并带上输入值', async () => {
    const signIn = vi.fn().mockResolvedValue(undefined);
    renderLogin({ signIn });
    const user = userEvent.setup();

    await user.type(screen.getByPlaceholderText('请输入用户名'), 'admin');
    await user.type(screen.getByPlaceholderText('请输入口令'), 'secret');
    await user.click(screen.getByRole('button', { name: '登录' }));

    await waitFor(() => expect(signIn).toHaveBeenCalledWith('admin', 'secret'));
  });

  it('登录失败时展示错误文案', async () => {
    const signIn = vi.fn().mockRejectedValue(new Error('用户名或口令错误'));
    renderLogin({ signIn });
    const user = userEvent.setup();

    await user.type(screen.getByPlaceholderText('请输入用户名'), 'admin');
    await user.type(screen.getByPlaceholderText('请输入口令'), 'wrong');
    await user.click(screen.getByRole('button', { name: '登录' }));

    expect(await screen.findByText('用户名或口令错误')).toBeInTheDocument();
  });
});
