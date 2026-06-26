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
