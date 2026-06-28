// 跨仓库制品搜索页测试（FR-94）：经页眉跳转的 ?q= 自动搜索、结果按仓库分组 → 路径层级树、
// 每个仓库分组 / 文件按格式渲染专属 icon、点击文件叶子进入详情、空结果文案。
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

  it('结果按仓库分组 → 路径层级树渲染（目录节点 + 文件叶子，默认全展开）', async () => {
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
    // 路径折叠为层级树：出现中间目录节点（com 合并、a/b 子目录、left-pad 目录）
    expect(screen.getByText('com')).toBeInTheDocument();
    expect(screen.getByText('a')).toBeInTheDocument();
    expect(screen.getByText('b')).toBeInTheDocument();
    expect(screen.getByText('left-pad')).toBeInTheDocument();
    // 默认全展开 → 文件叶子以文件名可见（非整条路径）
    expect(screen.getByText('a.jar')).toBeInTheDocument();
    expect(screen.getByText('b.jar')).toBeInTheDocument();
    expect(screen.getByText('index.js')).toBeInTheDocument();
    // 不再以整条路径平铺
    expect(screen.queryByText('com/a/a.jar')).not.toBeInTheDocument();
  });

  it('点击目录节点可折叠 / 展开其子树', async () => {
    const user = userEvent.setup();
    mockedApi.search.mockResolvedValue(paged([hit('r1', 'maven-hosted', 'maven', 'com/a/a.jar')]));
    renderAt('?q=x');

    // 默认展开 → a.jar 可见
    await waitFor(() => expect(screen.getByText('a.jar')).toBeInTheDocument());
    // 折叠顶层目录 com → 其下子树（含 a.jar）隐藏
    await user.click(screen.getByText('com'));
    expect(screen.queryByText('a.jar')).not.toBeInTheDocument();
    expect(screen.queryByText('a')).not.toBeInTheDocument();
    // 再展开 com → 子层展开态保留（a 与 a.jar 重新可见，无需逐级重展）
    await user.click(screen.getByText('com'));
    expect(await screen.findByText('a')).toBeInTheDocument();
    expect(screen.getByText('a.jar')).toBeInTheDocument();
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

  it('点击文件叶子 → 跳转制品详情（repo + 完整 path 参数）', async () => {
    const user = userEvent.setup();
    mockedApi.search.mockResolvedValue(paged([hit('r1', 'maven-hosted', 'maven', 'com/a/a.jar')]));
    renderAt('?q=x');

    // 叶子以文件名渲染，但跳转仍带完整路径
    await user.click(await screen.findByText('a.jar'));
    expect(navigateMock).toHaveBeenCalledWith('/artifact?repo=r1&path=com%2Fa%2Fa.jar');
  });

  it('空结果展示空文案，不泄露任何制品', async () => {
    mockedApi.search.mockResolvedValue(paged([]));
    renderAt('?q=nothing');
    await waitFor(() => expect(screen.getByText('未找到匹配的制品。')).toBeInTheDocument());
    // 前端只渲染后端返回项：无私有仓库制品被渲染
    expect(screen.queryByText('private-secret.jar')).not.toBeInTheDocument();
  });

  it('结果树逐层缩进递进 + 文件名不截断（FR-115）', async () => {
    mockedApi.search.mockResolvedValue(
      paged([hit('r1', 'maven-hosted', 'maven', 'com/example/lib-1.0.jar.sha256')]),
    );
    renderAt('?q=x');

    // 默认全展开：根层目录 com、次层 example、文件叶子均可见
    await waitFor(() => expect(screen.getByText('com')).toBeInTheDocument());
    const comFolder = screen
      .getByText('com')
      .closest('[data-testid="search-tree-folder"]') as HTMLElement;
    const exampleFolder = screen
      .getByText('example')
      .closest('[data-testid="search-tree-folder"]') as HTMLElement;
    const fileRow = screen
      .getByText('lib-1.0.jar.sha256')
      .closest('[data-testid="search-tree-file"]') as HTMLElement;

    const comIndent = parseInt(comFolder.style.paddingLeft, 10);
    const exampleIndent = parseInt(exampleFolder.style.paddingLeft, 10);
    const fileIndent = parseInt(fileRow.style.paddingLeft, 10);

    // 逐层缩进递增
    expect(exampleIndent).toBeGreaterThan(comIndent);
    expect(fileIndent).toBeGreaterThan(exampleIndent);
    // 文件名不截断（无 Mantine truncate 标记）
    expect(screen.getByText('lib-1.0.jar.sha256')).not.toHaveAttribute('data-truncate', 'true');
  });
});
