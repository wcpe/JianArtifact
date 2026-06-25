// Nexus 迁移管理页面组件测试（FR-81）：
// 在线 / 离线两形态预览 → 勾选 → 执行搬运 → 展示报告；凭据引用不回显明文；错误展示文案。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { Notifications } from '@mantine/notifications';
import { MigrationPage } from './MigrationPage';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type { MigrationReport, NexusRepoSummary, OfflineRepoSummary } from '../api/types';

/** 在 Provider 下渲染迁移页（含通知容器）。 */
function renderPage() {
  return render(
    <MantineProvider>
      <Notifications />
      <MigrationPage />
    </MantineProvider>,
  );
}

const 在线仓库: NexusRepoSummary[] = [
  { name: 'maven-proxy', format: 'maven2', type: 'proxy', upstream_url: 'https://repo1.example' },
  { name: 'npm-hosted', format: 'npm', type: 'hosted', upstream_url: null },
];

const 离线仓库: OfflineRepoSummary[] = [
  {
    repo_name: 'raw-hosted',
    blob_count: 2,
    blobs: [
      { blob_name: 'a/b.txt', sha1: 'abc', size: 12 },
      { blob_name: 'c/d.txt', sha1: null, size: null },
    ],
  },
];

const 迁移报告: MigrationReport = {
  repos: [
    {
      repo_name: 'maven-proxy',
      format: 'maven',
      created: true,
      migrated_artifacts: 5,
      skipped_artifacts: 1,
    },
  ],
  skipped_repos: ['npm-hosted'],
};

describe('MigrationPage', () => {
  afterEach(() => vi.restoreAllMocks());

  it('在线预览：填源地址后枚举仓库并展示列表', async () => {
    const spy = vi.spyOn(api, 'previewNexusRepositories').mockResolvedValue(在线仓库);
    const user = userEvent.setup();
    renderPage();

    await user.type(screen.getByPlaceholderText('https://nexus.example'), 'https://nexus.example');
    await user.click(screen.getByRole('button', { name: '预览仓库' }));

    await waitFor(() => expect(screen.getByText('maven-proxy')).toBeInTheDocument());
    expect(screen.getByText('npm-hosted')).toBeInTheDocument();
    // 仅引用、不回显明文：auth_ref 字段为口令型输入
    expect(spy).toHaveBeenCalledWith({ base_url: 'https://nexus.example', auth_ref: undefined });
  });

  it('凭据引用输入框为口令型（不回显明文）', async () => {
    renderPage();
    const input = screen.getByPlaceholderText('例如 NEXUS_SRC');
    expect(input).toHaveAttribute('type', 'password');
  });

  it('离线预览：填本地路径后枚举 blob 分组', async () => {
    const spy = vi.spyOn(api, 'previewNexusOffline').mockResolvedValue(离线仓库);
    const user = userEvent.setup();
    renderPage();

    // 切到离线形态
    await user.click(screen.getByRole('radio', { name: /离线/ }));
    await user.type(screen.getByPlaceholderText('/data/nexus/blobs/default'), '/data/blobs');
    await user.click(screen.getByRole('button', { name: '预览仓库' }));

    await waitFor(() => expect(screen.getByText('raw-hosted')).toBeInTheDocument());
    expect(screen.getByText('2')).toBeInTheDocument();
    expect(spy).toHaveBeenCalledWith({ path: '/data/blobs' });
  });

  it('预览后勾选仓库、执行 proxy 搬运并展示报告', async () => {
    vi.spyOn(api, 'previewNexusRepositories').mockResolvedValue(在线仓库);
    const migrateSpy = vi.spyOn(api, 'migrateNexusProxy').mockResolvedValue(迁移报告);
    const user = userEvent.setup();
    renderPage();

    await user.type(screen.getByPlaceholderText('https://nexus.example'), 'https://nexus.example');
    await user.click(screen.getByRole('button', { name: '预览仓库' }));
    await waitFor(() => expect(screen.getByText('maven-proxy')).toBeInTheDocument());

    // 进入勾选步骤
    await user.click(screen.getByRole('button', { name: '下一步：勾选执行' }));
    // 勾选第一个仓库
    const checkbox = screen.getByRole('checkbox', { name: /maven-proxy/ });
    await user.click(checkbox);
    // 填离线路径（搬运需要 blob 本体来源）
    await user.type(screen.getByPlaceholderText('/data/nexus/blobs/default'), '/data/blobs');
    // 执行 proxy 搬运
    await user.click(screen.getByRole('button', { name: '执行 proxy 搬运' }));

    await waitFor(() => expect(migrateSpy).toHaveBeenCalled());
    expect(migrateSpy).toHaveBeenCalledWith({
      base_url: 'https://nexus.example',
      auth_ref: undefined,
      offline_path: '/data/blobs',
    });
    // 报告展示：等待报告表格中的仓库行出现，校验已迁制品数
    await waitFor(() =>
      expect(screen.getByText('maven-proxy', { selector: 'td' })).toBeInTheDocument(),
    );
    const reportRegion = screen.getByText('maven-proxy', { selector: 'td' }).closest('table')!;
    expect(within(reportRegion).getByText('5')).toBeInTheDocument();
    // 整仓跳过项以徽章呈现
    expect(screen.getByText('npm-hosted')).toBeInTheDocument();
  });

  it('预览失败展示错误文案', async () => {
    vi.spyOn(api, 'previewNexusRepositories').mockRejectedValue(
      new ApiError(502, 'bad_gateway', '连接源 Nexus 失败'),
    );
    const user = userEvent.setup();
    renderPage();

    await user.type(screen.getByPlaceholderText('https://nexus.example'), 'https://nexus.example');
    await user.click(screen.getByRole('button', { name: '预览仓库' }));

    await waitFor(() => expect(screen.getByText('连接源 Nexus 失败')).toBeInTheDocument());
  });
});
