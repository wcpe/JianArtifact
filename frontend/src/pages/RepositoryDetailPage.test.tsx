// 仓库详情页「文件浏览」标签测试（FR-76）：渲染目录树、逐级展开、点文件跳详情。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { MemoryRouter } from 'react-router-dom';
import { RepositoryDetailPage } from './RepositoryDetailPage';
import * as api from '../api/endpoints';
import type { ArtifactDto, RepositoryDto } from '../api/types';

// 桩掉端点模块与通知
vi.mock('../api/endpoints');
vi.mock('../lib/notify', () => ({ notifySuccess: vi.fn(), notifyError: vi.fn() }));

// 桩掉鉴权上下文：非管理员即可（文件浏览对所有可读用户开放）
vi.mock('../auth/useAuth', () => ({
  useAuth: () => ({ isAdmin: false }),
}));

// 捕获导航跳转
const navigateMock = vi.fn();
vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<typeof import('react-router-dom')>('react-router-dom');
  return { ...actual, useNavigate: () => navigateMock };
});

const mockedApi = vi.mocked(api);

const REPO: RepositoryDto = {
  id: 'r1',
  name: 'files',
  format: 'raw',
  type: 'hosted',
  visibility: 'public',
  upstream_url: null,
  created_at: '2026-01-01T00:00:00Z',
};

function art(path: string): ArtifactDto {
  return {
    path,
    size: 12,
    sha256: 'abc',
    content_type: null,
    cached: false,
    created_at: '2026-01-01T00:00:00Z',
  };
}

/** 在 Mantine + Router 下渲染详情页，定位到仓库并切到文件浏览标签。 */
function renderPage() {
  return render(
    <MantineProvider>
      <MemoryRouter initialEntries={['/repository?id=r1']}>
        <RepositoryDetailPage />
      </MemoryRouter>
    </MantineProvider>,
  );
}

describe('RepositoryDetailPage 文件浏览', () => {
  beforeEach(() => {
    navigateMock.mockReset();
    mockedApi.getRepository.mockResolvedValue(REPO);
    mockedApi.listArtifacts.mockResolvedValue([
      art('dir/a.txt'),
      art('dir/sub/b.txt'),
      art('top.txt'),
    ]);
  });
  afterEach(() => vi.restoreAllMocks());

  /** 切到「文件浏览」标签并返回其活动 tabpanel（作用域查询，避开制品浏览标签的同名文本）。 */
  async function gotoBrowse(user: ReturnType<typeof userEvent.setup>): Promise<HTMLElement> {
    await waitFor(() => expect(screen.getByText('文件浏览')).toBeInTheDocument());
    await user.click(screen.getByText('文件浏览'));
    // 活动面板可见（非活动面板带 hidden，被 role 查询排除）
    return await screen.findByRole('tabpanel');
  }

  it('切到文件浏览标签后展示根目录一层（子目录 + 文件）', async () => {
    const user = userEvent.setup();
    renderPage();
    const panel = await gotoBrowse(user);

    // 根目录：dir/（folder）与 top.txt（file）；不直接出现深层 sub/b.txt
    await waitFor(() => expect(within(panel).getByText('dir/')).toBeInTheDocument());
    expect(within(panel).getByText('top.txt')).toBeInTheDocument();
    expect(within(panel).queryByText('b.txt')).not.toBeInTheDocument();
  });

  it('点子目录逐级进入下一层，点文件跳详情', async () => {
    const user = userEvent.setup();
    renderPage();
    const panel = await gotoBrowse(user);

    // 进入 dir/
    await waitFor(() => expect(within(panel).getByText('dir/')).toBeInTheDocument());
    await user.click(within(panel).getByText('dir/'));

    // 现在层内应见 a.txt（文件）与 sub/（子目录）
    await waitFor(() => expect(within(panel).getByText('a.txt')).toBeInTheDocument());
    expect(within(panel).getByText('sub/')).toBeInTheDocument();

    // 点文件 a.txt → 导航到制品详情页，带 repo 与 path
    await user.click(within(panel).getByText('a.txt'));
    expect(navigateMock).toHaveBeenCalledWith(
      expect.stringContaining('/artifact?repo=r1&path=dir%2Fa.txt'),
    );
  });

  it('空仓库展示空目录文案', async () => {
    mockedApi.listArtifacts.mockResolvedValue([]);
    const user = userEvent.setup();
    renderPage();
    const panel = await gotoBrowse(user);

    await waitFor(() => expect(within(panel).getByText('该目录为空。')).toBeInTheDocument());
  });
});
