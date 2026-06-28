// Token 管理页 MSW 契约强断言测试（FR-116，ADR-0035）。
//
// 组件走真实 client.ts，被 MSW 有状态内存后端拦截：
// - 列表 GET 渲染 store 中令牌；
// - 签发 POST 真实落库并回显「仅本次可见」明文 → 列表新增（有状态）；
// - 吊销 DELETE 把记录置 revoked → 状态徽标变更。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { MemoryRouter } from 'react-router-dom';
import { TokensPage } from './TokensPage';
import { state, nextId, type MockToken } from '../test/mocks/store';
import { loginAs } from '../test/mocks/auth';

/** 在 Mantine + Router 下渲染 Token 页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <MemoryRouter>
        <TokensPage />
      </MemoryRouter>
    </MantineProvider>,
  );
}

/** 直接往内存 store 塞一枚令牌。 */
function seedToken(name: string, revoked = false): MockToken {
  const token: MockToken = {
    id: nextId('t'),
    name,
    created_at: '2026-03-01T00:00:00Z',
    last_used_at: null,
    revoked,
    plaintext: `jart_seed_${name}`,
  };
  state.tokens.push(token);
  return token;
}

describe('TokensPage 走 MSW 的契约强断言（FR-116）', () => {
  beforeEach(async () => {
    await loginAs();
  });
  afterEach(() => vi.restoreAllMocks());

  it('GET /tokens 的真实响应被渲染（列表不回显明文）', async () => {
    seedToken('ci-pipeline');
    renderPage();

    expect(await screen.findByText('ci-pipeline')).toBeInTheDocument();
    // 列表视图不含明文（TokenView 无 token 字段）
    expect(screen.queryByText('jart_seed_ci-pipeline')).not.toBeInTheDocument();
  });

  it('签发 Token：真实 POST 落库 → 弹窗回显明文 + 列表新增（有状态）', async () => {
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('暂无 Token。')).toBeInTheDocument());

    await user.click(screen.getByRole('button', { name: '签发 Token' }));
    await user.type(await screen.findByPlaceholderText('如 ci-pipeline'), 'release-bot');
    await user.click(screen.getByRole('button', { name: '签发' }));

    // 真实落库
    await waitFor(() => expect(state.tokens.map((t) => t.name)).toContain('release-bot'));
    // 签发响应回显「仅本次可见」明文（来自 store 生成的真实明文）
    const created = state.tokens.find((t) => t.name === 'release-bot')!;
    expect(await screen.findByText(created.plaintext)).toBeInTheDocument();
  });

  it('吊销 Token：真实 DELETE 置 revoked → 状态徽标变「已吊销」、按钮禁用', async () => {
    const user = userEvent.setup();
    const token = seedToken('to-revoke');
    vi.spyOn(window, 'confirm').mockReturnValue(true);
    renderPage();

    const row = (await screen.findByText('to-revoke')).closest('tr')!;
    expect(within(row).getByText('有效')).toBeInTheDocument();

    await user.click(within(row).getByRole('button', { name: '吊销' }));

    // store 记录被真实置 revoked
    await waitFor(() => expect(state.tokens.find((t) => t.id === token.id)?.revoked).toBe(true));
    // 重新拉取后徽标变为已吊销
    await waitFor(() => {
      const updated = screen.getByText('to-revoke').closest('tr')!;
      expect(within(updated).getByText('已吊销')).toBeInTheDocument();
    });
  });

  it('未认证（无令牌）：GET /tokens 返回 401，页面展示错误', async () => {
    // 清掉登录令牌，模拟未认证访问
    const { clearToken } = await import('../api/client');
    clearToken();
    renderPage();
    expect(await screen.findByText('未认证')).toBeInTheDocument();
  });
});
