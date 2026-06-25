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
import type {
  MigrationReport,
  NexusRepoSummary,
  OfflineRepoSummary,
  OnlineMigrationReport,
} from '../api/types';

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

const 在线仓库含maven: NexusRepoSummary[] = [
  { name: 'maven-releases', format: 'maven2', type: 'hosted', upstream_url: null },
  { name: 'npm-hosted', format: 'npm', type: 'hosted', upstream_url: null },
];

const 在线迁移报告: OnlineMigrationReport = {
  repos: [
    {
      source_repo: 'maven-releases',
      target_repo: 'maven-mirror',
      format: 'maven',
      created: true,
      migrated_artifacts: 7,
      skipped_artifacts: 0,
    },
  ],
  skipped_repos: ['npm-hosted'],
};

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
    // 切到「离线目录」迁移方式（默认是在线拉取）
    await user.click(screen.getByRole('radio', { name: /离线目录/ }));
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

  it('在线拉取：勾选仓库并改名，按所选仓库发起请求并渲染报告', async () => {
    vi.spyOn(api, 'previewNexusRepositories').mockResolvedValue(在线仓库含maven);
    const onlineSpy = vi.spyOn(api, 'migrateNexusOnline').mockResolvedValue(在线迁移报告);
    const user = userEvent.setup();
    renderPage();

    await user.type(screen.getByPlaceholderText('https://nexus.example'), 'https://nexus.example');
    await user.click(screen.getByRole('button', { name: '预览仓库' }));
    await waitFor(() => expect(screen.getByText('maven-releases')).toBeInTheDocument());

    // 进入勾选步骤（默认迁移方式即为「在线拉取」）
    await user.click(screen.getByRole('button', { name: '下一步：勾选执行' }));
    // 勾选 maven 仓库并填目标改名
    await user.click(screen.getByRole('checkbox', { name: /maven-releases/ }));
    await user.type(
      screen.getByRole('textbox', { name: 'maven-releases 目标仓库名' }),
      'maven-mirror',
    );
    // 执行在线拉取
    await user.click(screen.getByRole('button', { name: '执行在线拉取' }));

    await waitFor(() => expect(onlineSpy).toHaveBeenCalled());
    // 仅发起所选仓库（含改名），未选仓库不出现在请求里
    expect(onlineSpy).toHaveBeenCalledWith({
      base_url: 'https://nexus.example',
      auth_ref: undefined,
      repositories: [{ source: 'maven-releases', target: 'maven-mirror' }],
    });

    // 报告展示源→目标与已迁制品数
    await waitFor(() =>
      expect(screen.getByText('maven-releases', { selector: 'td' })).toBeInTheDocument(),
    );
    const reportTable = screen.getByText('maven-releases', { selector: 'td' }).closest('table')!;
    expect(within(reportTable).getByText('maven-mirror')).toBeInTheDocument();
    expect(within(reportTable).getByText('7')).toBeInTheDocument();
    // 整仓跳过项以徽章呈现
    expect(screen.getByText('npm-hosted')).toBeInTheDocument();
  });

  it('在线拉取：未填目标改名时省略 target（与源同名）', async () => {
    vi.spyOn(api, 'previewNexusRepositories').mockResolvedValue(在线仓库含maven);
    const onlineSpy = vi.spyOn(api, 'migrateNexusOnline').mockResolvedValue(在线迁移报告);
    const user = userEvent.setup();
    renderPage();

    await user.type(screen.getByPlaceholderText('https://nexus.example'), 'https://nexus.example');
    await user.click(screen.getByRole('button', { name: '预览仓库' }));
    await waitFor(() => expect(screen.getByText('maven-releases')).toBeInTheDocument());

    await user.click(screen.getByRole('button', { name: '下一步：勾选执行' }));
    await user.click(screen.getByRole('checkbox', { name: /maven-releases/ }));
    // 不填目标名，直接执行
    await user.click(screen.getByRole('button', { name: '执行在线拉取' }));

    await waitFor(() => expect(onlineSpy).toHaveBeenCalled());
    expect(onlineSpy).toHaveBeenCalledWith({
      base_url: 'https://nexus.example',
      auth_ref: undefined,
      repositories: [{ source: 'maven-releases' }],
    });
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
