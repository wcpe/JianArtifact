// 仓库管理页 MSW 契约强断言测试（FR-116，ADR-0035）。
//
// 与手工 vi.mock 弱断言不同：组件走真实 client.ts 发请求、被 MSW 有状态内存后端拦截，
// 断言落在「真实 HTTP 请求方法 / 路径 / 体」+「有状态 CRUD 后的响应渲染」：
// - 列表 GET 渲染 store 中仓库；
// - 创建 POST 真实落库 → 后续 GET 查得到（有状态时序）；
// - 删除 DELETE 真实移除 → 列表不再渲染。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { MemoryRouter } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { RepositoriesPage } from './RepositoriesPage';
import { AuthContext, type AuthContextValue } from '../auth/AuthContext';
import { server } from '../test/mocks/server';
import { state, nextId } from '../test/mocks/store';
import { loginAs } from '../test/mocks/auth';
import type { RepositoryDto } from '../api/types';

vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<typeof import('react-router-dom')>('react-router-dom');
  return { ...actual, useNavigate: () => vi.fn() };
});

/** 以管理员身份在 Mantine + Router + 注入认证上下文下渲染仓库页。 */
function renderPage(isAdmin = true) {
  const value: AuthContextValue = {
    user: { id: 'u-admin', username: 'admin', role: isAdmin ? 'admin' : 'user' },
    loading: false,
    isAdmin,
    signIn: vi.fn(),
    signOut: vi.fn(),
  };
  return render(
    <MantineProvider>
      <MemoryRouter>
        <AuthContext.Provider value={value}>
          <RepositoriesPage />
        </AuthContext.Provider>
      </MemoryRouter>
    </MantineProvider>,
  );
}

/** 直接往内存 store 塞一条仓库（绕过 UI，用于准备初始数据）。 */
function seedRepo(name: string, partial: Partial<RepositoryDto> = {}): RepositoryDto {
  const repo: RepositoryDto = {
    id: nextId('r'),
    name,
    format: 'maven',
    type: 'hosted',
    visibility: 'private',
    upstream_url: null,
    created_at: '2026-03-01T00:00:00Z',
    ...partial,
  };
  state.repositories.push(repo);
  return repo;
}

describe('RepositoriesPage 走 MSW 的契约强断言（FR-116）', () => {
  beforeEach(async () => {
    await loginAs(); // 写入管理员令牌，后续组件请求自动带 Bearer
  });
  afterEach(() => vi.restoreAllMocks());

  it('GET /repositories 的真实响应被渲染为表格行', async () => {
    seedRepo('maven-releases', { visibility: 'public' });
    seedRepo('npm-proxy', {
      format: 'npm',
      type: 'proxy',
      upstream_url: 'https://registry.npmjs.org',
    });
    renderPage();

    expect(await screen.findByText('maven-releases')).toBeInTheDocument();
    expect(screen.getByText('npm-proxy')).toBeInTheDocument();
  });

  it('创建仓库：真实 POST 落库 → 重新 GET 后列表出现新仓库（有状态）', async () => {
    const user = userEvent.setup();
    renderPage();

    // 初始为空态
    await waitFor(() => expect(state.repositories).toHaveLength(0));

    // 等工具栏「创建仓库」按钮随初始加载完成渲染后再点（findBy 异步等待）：
    // 空列表无行文本可锚定，若用同步 getByRole 在较慢环境（CI Node 20）会在加载态尚未结束时取不到按钮而失败。
    await user.click(await screen.findByRole('button', { name: '创建仓库' }));
    await user.type(await screen.findByPlaceholderText('如 maven-releases'), 'demo-raw');
    await user.click(screen.getByRole('button', { name: '创建' }));

    // 断言新仓库经真实 POST 落入内存 store
    await waitFor(() => expect(state.repositories.map((r) => r.name)).toContain('demo-raw'));
    // 且创建后重新拉取的列表里渲染出该仓库
    expect(await screen.findByText('demo-raw')).toBeInTheDocument();
  });

  it('重名创建：MSW 按后端契约返回 409，store 不重复写入', async () => {
    const user = userEvent.setup();
    seedRepo('maven-releases');
    renderPage();
    await screen.findByText('maven-releases');

    await user.click(screen.getByRole('button', { name: '创建仓库' }));
    await user.type(await screen.findByPlaceholderText('如 maven-releases'), 'maven-releases');
    await user.click(screen.getByRole('button', { name: '创建' }));

    // 409 被拒：store 中仍只有一条同名仓库
    await waitFor(() =>
      expect(state.repositories.filter((r) => r.name === 'maven-releases')).toHaveLength(1),
    );
  });

  it('删除仓库：真实 DELETE 移除 store 记录 → 列表不再渲染', async () => {
    const user = userEvent.setup();
    const repo = seedRepo('to-delete');
    vi.spyOn(window, 'confirm').mockReturnValue(true);
    renderPage();

    const row = (await screen.findByText('to-delete')).closest('tr')!;
    await user.click(within(row).getByLabelText('删除仓库'));

    await waitFor(() => expect(state.repositories.find((r) => r.id === repo.id)).toBeUndefined());
    await waitFor(() => expect(screen.queryByText('to-delete')).not.toBeInTheDocument());
  });

  it('非管理员：列表照常渲染但无创建 / 删除入口', async () => {
    seedRepo('readonly-repo', { visibility: 'public' });
    renderPage(false);

    expect(await screen.findByText('readonly-repo')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '创建仓库' })).not.toBeInTheDocument();
    expect(screen.queryByLabelText('删除仓库')).not.toBeInTheDocument();
  });

  it('GET /repositories 失败（500）时展示错误提示', async () => {
    // 临时覆盖 handler：返回服务端错误，验证错误路径经 client.ts 解析后渲染
    server.use(
      http.get('/api/v1/repositories', () =>
        HttpResponse.json({ error: { code: 'internal', message: '内部错误' } }, { status: 500 }),
      ),
    );
    renderPage();
    expect(await screen.findByText('内部错误')).toBeInTheDocument();
  });
});
