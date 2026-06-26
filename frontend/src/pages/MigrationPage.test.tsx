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
  OnlinePullJob,
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

/** 任务进行中快照（downloading）：进度过半、含当前仓库 / 当前文件。 */
const 任务进行中: OnlinePullJob = {
  job_id: 'job-1',
  phase: 'downloading',
  total_assets: 7,
  done_assets: 3,
  migrated: 3,
  skipped: 0,
  current_repo: 'maven-releases',
  current_path: 'com/example/app/1.0/app-1.0.jar',
  paused: false,
  repos: [],
  skipped_repos: [],
  error: null,
};

/** 任务暂停态快照（paused）：用于断言「继续」按钮可用、「暂停」按钮被「继续」替换。 */
const 任务已暂停: OnlinePullJob = {
  ...任务进行中,
  phase: 'paused',
  paused: true,
};

/** 任务已取消终态快照（cancelled）：不算失败、保留已搬运。 */
const 任务已取消: OnlinePullJob = {
  ...任务进行中,
  phase: 'cancelled',
  paused: false,
};

/** 任务终态快照（done）：含每仓库明细与整仓跳过列表。 */
const 任务完成: OnlinePullJob = {
  job_id: 'job-1',
  phase: 'done',
  total_assets: 7,
  done_assets: 7,
  migrated: 7,
  skipped: 0,
  current_repo: null,
  current_path: null,
  paused: false,
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
  error: null,
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
  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
    // 清理在线任务存档，避免跨用例触发重连副作用。
    localStorage.clear();
  });

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

  it('在线拉取：发起异步任务后轮询进度，downloading→done 渲染队列进度与最终报告', async () => {
    vi.spyOn(api, 'previewNexusRepositories').mockResolvedValue(在线仓库含maven);
    const onlineSpy = vi.spyOn(api, 'migrateNexusOnline').mockResolvedValue({ job_id: 'job-1' });
    // 任务进度顺序：首次返回 downloading，下一周期返回 done。
    const jobSpy = vi
      .spyOn(api, 'getMigrationJob')
      .mockResolvedValueOnce(任务进行中)
      .mockResolvedValue(任务完成);
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
    // 发起在线拉取（异步，立即返回 job_id）
    await user.click(screen.getByRole('button', { name: '执行在线拉取' }));

    await waitFor(() => expect(onlineSpy).toHaveBeenCalled());
    // 仅发起所选仓库（含改名），未选仓库不出现在请求里
    expect(onlineSpy).toHaveBeenCalledWith({
      base_url: 'https://nexus.example',
      auth_ref: undefined,
      repositories: [{ source: 'maven-releases', target: 'maven-mirror' }],
    });
    // job_id 存档供重连
    expect(localStorage.getItem('jian.migrate.online.jobId')).toBe('job-1');

    // 首次轮询（发起后立即拉一次）：展示进行中队列（当前仓库 / 当前文件 / 进度计数）
    await waitFor(() => expect(jobSpy).toHaveBeenCalledWith('job-1'));
    await waitFor(() => expect(screen.getByText('下载搬运中')).toBeInTheDocument());
    expect(screen.getByText(/com\/example\/app\/1\.0\/app-1\.0\.jar/)).toBeInTheDocument();
    expect(screen.getByText(/进度 3 \/ 7/)).toBeInTheDocument();

    // 经下一个轮询周期（约 1.5s）转入终态 done，展示最终报告
    await waitFor(() => expect(screen.getByText('已完成')).toBeInTheDocument(), { timeout: 3000 });
    const reportTable = screen.getByText('maven-releases', { selector: 'td' }).closest('table')!;
    expect(within(reportTable).getByText('maven-mirror')).toBeInTheDocument();
    expect(within(reportTable).getByText('7')).toBeInTheDocument();
    // 整仓跳过项以徽章呈现
    expect(screen.getByText('npm-hosted')).toBeInTheDocument();
    // 终态后清理存档
    expect(localStorage.getItem('jian.migrate.online.jobId')).toBeNull();
  });

  it('在线拉取：未填目标改名时省略 target（与源同名）', async () => {
    vi.spyOn(api, 'previewNexusRepositories').mockResolvedValue(在线仓库含maven);
    const onlineSpy = vi.spyOn(api, 'migrateNexusOnline').mockResolvedValue({ job_id: 'job-2' });
    vi.spyOn(api, 'getMigrationJob').mockResolvedValue({ ...任务完成, job_id: 'job-2' });
    const user = userEvent.setup();
    renderPage();

    await user.type(screen.getByPlaceholderText('https://nexus.example'), 'https://nexus.example');
    await user.click(screen.getByRole('button', { name: '预览仓库' }));
    await waitFor(() => expect(screen.getByText('maven-releases')).toBeInTheDocument());

    await user.click(screen.getByRole('button', { name: '下一步：勾选执行' }));
    await user.click(screen.getByRole('checkbox', { name: /maven-releases/ }));
    // 不填目标名，直接发起
    await user.click(screen.getByRole('button', { name: '执行在线拉取' }));

    await waitFor(() => expect(onlineSpy).toHaveBeenCalled());
    expect(onlineSpy).toHaveBeenCalledWith({
      base_url: 'https://nexus.example',
      auth_ref: undefined,
      repositories: [{ source: 'maven-releases' }],
    });
  });

  it('客户端重连：页面加载时存档 job_id 仍进行中则恢复轮询续看', async () => {
    // 预置存档：模拟刷新页面前已发起的任务。
    localStorage.setItem('jian.migrate.online.jobId', 'job-9');
    const jobSpy = vi
      .spyOn(api, 'getMigrationJob')
      .mockResolvedValue({ ...任务进行中, job_id: 'job-9' });
    renderPage();

    // 加载即拉取存档任务并切到报告步骤、展示进行中队列
    await waitFor(() => expect(jobSpy).toHaveBeenCalledWith('job-9'));
    await waitFor(() => expect(screen.getByText('下载搬运中')).toBeInTheDocument());
    expect(screen.getByText(/进度 3 \/ 7/)).toBeInTheDocument();
  });

  it('客户端重连：存档 job_id 已失效（404）则清理存档', async () => {
    localStorage.setItem('jian.migrate.online.jobId', 'job-gone');
    vi.spyOn(api, 'getMigrationJob').mockRejectedValue(
      new ApiError(404, 'not_found', '任务不存在'),
    );
    renderPage();

    await waitFor(() => expect(localStorage.getItem('jian.migrate.online.jobId')).toBeNull());
  });

  it('任务控制：进行中展示暂停 / 取消，点暂停调用暂停端点', async () => {
    localStorage.setItem('jian.migrate.online.jobId', 'job-1');
    // 稳定返回进行中态：避免轮询期间快照漂移，确保按钮态确定。
    vi.spyOn(api, 'getMigrationJob').mockResolvedValue(任务进行中);
    const pauseSpy = vi.spyOn(api, 'pauseMigrationJob').mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('下载搬运中')).toBeInTheDocument());
    // 进行中：展示「暂停」而非「继续」，暂停 / 取消均可用
    const pauseBtn = screen.getByRole('button', { name: '暂停' });
    expect(pauseBtn).toBeEnabled();
    expect(screen.getByRole('button', { name: '取消' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: '继续' })).toBeNull();
    await user.click(pauseBtn);

    await waitFor(() => expect(pauseSpy).toHaveBeenCalledWith('job-1'));
  });

  it('任务控制：已暂停展示继续，点继续调用继续端点', async () => {
    localStorage.setItem('jian.migrate.online.jobId', 'job-1');
    vi.spyOn(api, 'getMigrationJob').mockResolvedValue(任务已暂停);
    const resumeSpy = vi.spyOn(api, 'resumeMigrationJob').mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('已暂停')).toBeInTheDocument());
    // 暂停态：展示「继续」而非「暂停」
    const resumeBtn = screen.getByRole('button', { name: '继续' });
    expect(resumeBtn).toBeEnabled();
    expect(screen.queryByRole('button', { name: '暂停' })).toBeNull();
    await user.click(resumeBtn);

    await waitFor(() => expect(resumeSpy).toHaveBeenCalledWith('job-1'));
  });

  it('任务控制：点取消调用取消端点', async () => {
    localStorage.setItem('jian.migrate.online.jobId', 'job-1');
    vi.spyOn(api, 'getMigrationJob').mockResolvedValue(任务进行中);
    const cancelSpy = vi.spyOn(api, 'cancelMigrationJob').mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('下载搬运中')).toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: '取消' }));

    await waitFor(() => expect(cancelSpy).toHaveBeenCalledWith('job-1'));
  });

  it('任务控制：终态（已取消）下控制按钮全禁用', async () => {
    localStorage.setItem('jian.migrate.online.jobId', 'job-1');
    vi.spyOn(api, 'getMigrationJob').mockResolvedValue(任务已取消);
    renderPage();

    await waitFor(() => expect(screen.getByText('已取消')).toBeInTheDocument());
    // 终态：取消禁用；展示的是「暂停」（paused=false）且禁用
    expect(screen.getByRole('button', { name: '取消' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '暂停' })).toBeDisabled();
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
