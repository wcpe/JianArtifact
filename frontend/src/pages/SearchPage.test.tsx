// 跨仓库制品搜索页测试（FR-94）：经页眉跳转的 ?q= 自动搜索、结果按仓库分组成树、
// 每个仓库分组 / 命中按格式渲染专属 icon、点击命中进入详情、空结果文案。
// 结果集由后端按读权限过滤，前端只渲染返回项（断言不渲染未返回的私有制品）。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { MemoryRouter } from 'react-router-dom';
import { SearchPage } from './SearchPage';
import * as api from '../api/endpoints';
import type { Paginated, RepoFormat, SearchHit } from '../api/types';

vi.mock('../api/endpoints');

const navigateMock = vi.fn();
vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<typeof import('react-router-dom')>('react-router-dom');
  return { ...actual, useNavigate: () => navigateMock };
});

const mockedApi = vi.mocked(api);

function hit(repoId: string, repoName: string, format: RepoFormat, path: string): SearchHit {
  return {
    repo_id: repoId,
    repo_name: repoName,
    format,
    path,
    sha256: 'abc',
    size: 12,
    created_at: '2026-01-01T00:00:00Z',
  };
}

function paged(items: SearchHit[]): Paginated<SearchHit> {
  return { items, total: items.length, offset: 0, limit: 20, has_more: false };
}

/** 在 Mantine + Router 下渲染搜索页，携带初始查询参数。 */
function renderAt(search: string) {
  return render(
    <MantineProvider>
      <MemoryRouter initialEntries={[`/search${search}`]}>
        <SearchPage />
      </MemoryRouter>
    </MantineProvider>,
  );
}

describe('SearchPage 页眉驱动的自动搜索（FR-94）', () => {
  beforeEach(() => {
    navigateMock.mockReset();
    mockedApi.search.mockReset();
  });
  afterEach(() => vi.restoreAllMocks());

  it('URL 带 ?q= 时自动以该关键字发起搜索', async () => {
    mockedApi.search.mockResolvedValue(paged([]));
    renderAt('?q=lib-core');
    await waitFor(() =>
      expect(mockedApi.search).toHaveBeenCalledWith('lib-core', expect.objectContaining({})),
    );
  });

  it('无 q 时不自动搜索，显示初始提示', async () => {
    renderAt('');
    expect(mockedApi.search).not.toHaveBeenCalled();
    expect(screen.getByText('输入关键字开始搜索。')).toBeInTheDocument();
  });

  it('结果按仓库分组成树渲染（仓库名作为分组节点）', async () => {
    mockedApi.search.mockResolvedValue(
      paged([
        hit('r1', 'maven-hosted', 'maven', 'com/a/a.jar'),
        hit('r1', 'maven-hosted', 'maven', 'com/b/b.jar'),
        hit('r2', 'npm-proxy', 'npm', 'left-pad/index.js'),
      ]),
    );
    renderAt('?q=x');

    // 两个仓库分组节点
    await waitFor(() => expect(screen.getByText('maven-hosted')).toBeInTheDocument());
    expect(screen.getByText('npm-proxy')).toBeInTheDocument();
    // 默认展开 → 命中制品路径可见
    expect(screen.getByText('com/a/a.jar')).toBeInTheDocument();
    expect(screen.getByText('com/b/b.jar')).toBeInTheDocument();
    expect(screen.getByText('left-pad/index.js')).toBeInTheDocument();
  });

  it('每个命中项按格式渲染专属 icon（带格式无障碍标签）', async () => {
    mockedApi.search.mockResolvedValue(
      paged([
        hit('r1', 'maven-hosted', 'maven', 'com/a/a.jar'),
        hit('r2', 'docker-hosted', 'docker', 'app/latest'),
      ]),
    );
    renderAt('?q=x');

    // 分组节点按格式标注无障碍名（maven / docker 各一处仓库节点）
    await waitFor(() =>
      expect(screen.getByLabelText('maven 仓库 maven-hosted')).toBeInTheDocument(),
    );
    expect(screen.getByLabelText('docker 仓库 docker-hosted')).toBeInTheDocument();
  });

  it('点击命中制品 → 跳转制品详情（repo + path 参数）', async () => {
    const user = userEvent.setup();
    mockedApi.search.mockResolvedValue(paged([hit('r1', 'maven-hosted', 'maven', 'com/a/a.jar')]));
    renderAt('?q=x');

    await user.click(await screen.findByText('com/a/a.jar'));
    expect(navigateMock).toHaveBeenCalledWith('/artifact?repo=r1&path=com%2Fa%2Fa.jar');
  });

  it('空结果展示空文案，不泄露任何制品', async () => {
    mockedApi.search.mockResolvedValue(paged([]));
    renderAt('?q=nothing');
    await waitFor(() => expect(screen.getByText('未找到匹配的制品。')).toBeInTheDocument());
    // 前端只渲染后端返回项：无私有仓库制品被渲染
    expect(screen.queryByText('private-secret.jar')).not.toBeInTheDocument();
  });
});
