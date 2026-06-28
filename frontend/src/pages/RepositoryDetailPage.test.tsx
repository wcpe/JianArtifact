// 仓库详情页「浏览」标签测试（FR-93）：左侧文件树渲染 + 逐级展开、点文件 → 右侧详情面板、
// 多格式坐标下拉切换、HTML View 外链存在且指向正确；沿用既有鉴权门控（非管理员不见配置 / ACL）。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { MemoryRouter } from 'react-router-dom';
import { RepositoryDetailPage } from './RepositoryDetailPage';
import * as api from '../api/endpoints';
import type { ArtifactDetailDto, ArtifactDto, RepositoryDto } from '../api/types';

// 桩掉端点模块与通知
vi.mock('../api/endpoints');
vi.mock('../lib/notify', () => ({ notifySuccess: vi.fn(), notifyError: vi.fn() }));

// 桩掉鉴权上下文：非管理员（浏览对所有可读用户开放，配置 / ACL 仅管理员）
vi.mock('../auth/useAuth', () => ({
  useAuth: () => ({ isAdmin: false }),
}));

const navigateMock = vi.fn();
vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<typeof import('react-router-dom')>('react-router-dom');
  return { ...actual, useNavigate: () => navigateMock };
});

const mockedApi = vi.mocked(api);

const REPO: RepositoryDto = {
  id: 'r1',
  name: 'maven-hosted',
  format: 'maven',
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

function detailOf(path: string): ArtifactDetailDto {
  return {
    repo_id: 'r1',
    repo_name: 'maven-hosted',
    format: 'maven',
    path,
    size: 12,
    content_type: 'application/java-archive',
    cached: false,
    created_at: '2026-01-01T00:00:00Z',
    checksums: { sha256: 'aa', sha1: 'bb', md5: 'cc', sha512: 'dd' },
    usage: [],
  };
}

/** 在 Mantine + Router 下渲染详情页，定位到仓库。 */
function renderPage() {
  return render(
    <MantineProvider>
      <MemoryRouter initialEntries={['/repository?id=r1']}>
        <RepositoryDetailPage />
      </MemoryRouter>
    </MantineProvider>,
  );
}

describe('RepositoryDetailPage 浏览（文件树 + 右侧详情）', () => {
  beforeEach(() => {
    navigateMock.mockReset();
    mockedApi.getRepository.mockResolvedValue(REPO);
    mockedApi.listArtifacts.mockResolvedValue([
      art('com/example/lib/1.0/lib-1.0.jar'),
      art('top.txt'),
    ]);
    mockedApi.getArtifactDetail.mockResolvedValue(detailOf('com/example/lib/1.0/lib-1.0.jar'));
  });
  afterEach(() => vi.restoreAllMocks());

  it('渲染文件树根层（顶层目录 + 顶层文件）', async () => {
    renderPage();
    // 根层：com/（目录）与 top.txt（文件）；深层 lib-1.0.jar 默认不展开
    await waitFor(() => expect(screen.getByText('com')).toBeInTheDocument());
    expect(screen.getByText('top.txt')).toBeInTheDocument();
    expect(screen.queryByText('lib-1.0.jar')).not.toBeInTheDocument();
  });

  it('折叠父目录再重展开，子层展开态保留（不丢失逐级展开）', async () => {
    const user = userEvent.setup();
    mockedApi.listArtifacts.mockResolvedValue([art('com/example/lib/1.0/lib-1.0.jar')]);
    renderPage();

    // 逐级展开 com → example → lib → 1.0，直到文件可见
    await waitFor(() => expect(screen.getByText('com')).toBeInTheDocument());
    await user.click(screen.getByText('com'));
    await user.click(await screen.findByText('example'));
    await user.click(await screen.findByText('lib'));
    await user.click(await screen.findByText('1.0'));
    expect(await screen.findByText('lib-1.0.jar')).toBeInTheDocument();

    // 折叠顶层 com → 整个子树隐藏
    await user.click(screen.getByText('com'));
    expect(screen.queryByText('example')).not.toBeInTheDocument();
    expect(screen.queryByText('lib-1.0.jar')).not.toBeInTheDocument();

    // 重新展开 com → 之前逐级展开的子层一次性恢复（无需再逐级点开）
    await user.click(screen.getByText('com'));
    expect(await screen.findByText('example')).toBeInTheDocument();
    expect(screen.getByText('lib')).toBeInTheDocument();
    expect(screen.getByText('1.0')).toBeInTheDocument();
    expect(screen.getByText('lib-1.0.jar')).toBeInTheDocument();
  });

  it('点目录逐级展开到文件，点文件 → 右侧详情面板加载', async () => {
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('com')).toBeInTheDocument());
    await user.click(screen.getByText('com'));
    await user.click(await screen.findByText('example'));
    await user.click(await screen.findByText('lib'));
    await user.click(await screen.findByText('1.0'));

    // 展开到主构件文件
    await user.click(await screen.findByText('lib-1.0.jar'));

    // 右侧详情加载：调用了详情端点，并出现校验和标题
    await waitFor(() =>
      expect(mockedApi.getArtifactDetail).toHaveBeenCalledWith(
        'r1',
        'com/example/lib/1.0/lib-1.0.jar',
      ),
    );
    expect(await screen.findByText('校验和')).toBeInTheDocument();
  });

  it('Maven 制品详情含多格式坐标下拉，切换片段内容随之变化', async () => {
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('com')).toBeInTheDocument());
    await user.click(screen.getByText('com'));
    await user.click(await screen.findByText('example'));
    await user.click(await screen.findByText('lib'));
    await user.click(await screen.findByText('1.0'));
    await user.click(await screen.findByText('lib-1.0.jar'));

    // 默认 Apache Maven 片段
    await waitFor(() => expect(screen.getByText('依赖坐标')).toBeInTheDocument());
    expect(screen.getByText(/<artifactId>lib<\/artifactId>/)).toBeInTheDocument();

    // 切到 Gradle Groovy DSL
    const selectInput = screen.getByRole('textbox', { name: '选择依赖坐标格式' });
    await user.click(selectInput);
    await user.click(await screen.findByText('Gradle Groovy DSL'));
    await waitFor(() =>
      expect(screen.getByText("implementation 'com.example:lib:1.0'")).toBeInTheDocument(),
    );
  });

  it('详情面板含 HTML View 外链且指向制品目录索引', async () => {
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('com')).toBeInTheDocument());
    await user.click(screen.getByText('com'));
    await user.click(await screen.findByText('example'));
    await user.click(await screen.findByText('lib'));
    await user.click(await screen.findByText('1.0'));
    await user.click(await screen.findByText('lib-1.0.jar'));

    const htmlView = await screen.findByRole('link', { name: /HTML View/ });
    expect(htmlView).toHaveAttribute('href', '/maven-hosted/com/example/lib/1.0/');
    expect(htmlView).toHaveAttribute('target', '_blank');

    const download = screen.getByRole('link', { name: /下载/ });
    expect(download).toHaveAttribute('href', '/maven-hosted/com/example/lib/1.0/lib-1.0.jar');
  });

  it('非管理员不显示配置 / ACL 标签（沿用既有鉴权门控）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('浏览')).toBeInTheDocument());
    expect(screen.queryByText('配置')).not.toBeInTheDocument();
    expect(screen.queryByText('权限（ACL）')).not.toBeInTheDocument();
  });

  it('空仓库展示空文案', async () => {
    mockedApi.listArtifacts.mockResolvedValue([]);
    renderPage();
    await waitFor(() => expect(screen.getByText('该仓库暂无制品。')).toBeInTheDocument());
  });
});

describe('RepositoryDetailPage 浏览布局重构（FR-115）', () => {
  beforeEach(() => {
    navigateMock.mockReset();
    mockedApi.getRepository.mockResolvedValue(REPO);
    mockedApi.getArtifactDetail.mockResolvedValue(detailOf('com/example/lib/1.0/lib-1.0.jar'));
  });
  afterEach(() => vi.restoreAllMocks());

  it('左树为主、右详情为辅：两栏为各自独立滚动容器', async () => {
    mockedApi.listArtifacts.mockResolvedValue([art('top.txt')]);
    renderPage();
    await waitFor(() => expect(screen.getByText('top.txt')).toBeInTheDocument());

    // 左树与右详情各有独立滚动容器
    expect(screen.getByTestId('browse-tree-scroll')).toBeInTheDocument();
    expect(screen.getByTestId('browse-detail-scroll')).toBeInTheDocument();
    // 浏览区为固定高度的两栏布局容器（不随内容整页滚动）
    expect(screen.getByTestId('browse-layout')).toBeInTheDocument();
  });

  it('文件树逐层缩进递进：深层条目缩进大于浅层', async () => {
    const user = userEvent.setup();
    mockedApi.listArtifacts.mockResolvedValue([art('com/example/lib-1.0.jar')]);
    renderPage();

    // 根层目录 com 缩进最小
    await waitFor(() => expect(screen.getByText('com')).toBeInTheDocument());
    const comFolder = screen.getByText('com').closest('[data-testid="tree-folder"]') as HTMLElement;
    const comIndent = parseInt(comFolder.style.paddingLeft, 10);

    // 展开 com → 次层目录 example 缩进更大
    await user.click(screen.getByText('com'));
    const exampleFolder = (await screen.findByText('example')).closest(
      '[data-testid="tree-folder"]',
    ) as HTMLElement;
    const exampleIndent = parseInt(exampleFolder.style.paddingLeft, 10);

    // 展开 example → 文件叶子缩进再更大
    await user.click(screen.getByText('example'));
    const fileRow = (await screen.findByText('lib-1.0.jar')).closest(
      '[data-testid="tree-file"]',
    ) as HTMLElement;
    const fileIndent = parseInt(fileRow.style.paddingLeft, 10);

    expect(exampleIndent).toBeGreaterThan(comIndent);
    expect(fileIndent).toBeGreaterThan(exampleIndent);
  });

  it('文件名不截断：同目录多 sidecar 全名可辨（无 truncate 类）', async () => {
    const user = userEvent.setup();
    mockedApi.listArtifacts.mockResolvedValue([
      art('v/lib-1.0.jar'),
      art('v/lib-1.0.jar.md5'),
      art('v/lib-1.0.jar.sha1'),
      art('v/lib-1.0.jar.sha256'),
      art('v/lib-1.0.jar.sha512'),
    ]);
    renderPage();

    await waitFor(() => expect(screen.getByText('v')).toBeInTheDocument());
    await user.click(screen.getByText('v'));

    // 同目录五个全名各自完整出现，互不截断
    for (const name of [
      'lib-1.0.jar',
      'lib-1.0.jar.md5',
      'lib-1.0.jar.sha1',
      'lib-1.0.jar.sha256',
      'lib-1.0.jar.sha512',
    ]) {
      const node = await screen.findByText(name);
      expect(node).toBeInTheDocument();
      // 名称节点不再带 truncate（Mantine truncate 产出该 data 属性 / mod 类）
      expect(node).not.toHaveAttribute('data-truncate', 'true');
    }
  });
});
