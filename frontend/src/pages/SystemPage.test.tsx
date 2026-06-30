// 系统管理页组件测试（FR-109，仅管理员）：
// 三 tab（在线更新 / 重启 / 关闭），覆盖——
// 默认在「在线更新」tab、应用更新卡片可见；在线更新保存只发 update 块的部分 PATCH；
// 检查更新展示版本对比；切到重启 / 关闭 tab 二次确认后调系统操作端点并通知；
// 系统操作 409（更新进行中）的错误提示文案。
// 注：notify 依赖 Notifications Provider，这里整体 mock 掉，用 vi.mocked 断言被调用（参考 App.test.tsx）。

import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { SystemPage } from './SystemPage';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type {
  SettingsView,
  UpdateCheck,
  UpdateJob,
  CachedCheck,
  SystemActionResponse,
} from '../api/types';

// 桩掉通知，避免依赖 Notifications Provider；用 vi.mocked 断言被调用
vi.mock('../lib/notify', () => ({
  notifySuccess: vi.fn(),
  notifyError: vi.fn(),
}));

import { notifySuccess, notifyError } from '../lib/notify';

// FR-126 异步化：进页会调用 getCachedCheck（读留存检查结果）与 listUpdateJobs（重连续看）。
// 各测试默认桩为「无留存 / 无 job」，避免触达真实 fetch；个别测试再覆盖。
beforeEach(() => {
  vi.spyOn(api, 'getCachedCheck').mockResolvedValue({ result: null, checked_at: null });
  vi.spyOn(api, 'listUpdateJobs').mockResolvedValue([]);
});

/** 在 Mantine Provider 下渲染系统页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <SystemPage />
    </MantineProvider>,
  );
}

/** 一份启用在线更新、含可回滚备份的设置样例（参考 SettingsPage.test.tsx）。 */
const 启用样例: SettingsView = {
  current_version: '0.3.0',
  network_proxy: {
    http: { url: null, username: null, has_password: false },
    https: { url: null, username: null, has_password: false },
    all: { url: null, username: null, has_password: false },
    no_proxy: null,
  },
  update: {
    enabled: true,
    repo: 'wcpe/JianArtifact',
    api_base_url: 'https://api.github.com',
    restart_mode: 'self',
    channel: 'stable',
    has_token: true,
    rollback_available: true,
  },
};

/** 一份「有可用更新」的检查结果样例。 */
const 有更新样例: UpdateCheck = {
  current_version: '0.3.0',
  latest_version: '0.4.0',
  update_available: true,
  asset_name: 'jianartifact-x86_64.tar.gz',
  notes: '修复若干问题',
};

/** 系统操作成功响应。 */
const 系统操作成功: SystemActionResponse = { status: 'ok' };

describe('SystemPage', () => {
  afterEach(() => vi.restoreAllMocks());

  it('加载后默认在「在线更新」tab：应用更新卡片可见', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    // 卡片标题「应用更新」可见，启用开关与检查更新按钮可见
    await waitFor(() => expect(screen.getByText('应用更新')).toBeInTheDocument());
    expect(screen.getByText('启用在线更新（出站开关）')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '检查更新' })).toBeInTheDocument();
  });

  it('在线更新保存：只发 update 块、不含 network_proxy，且 enabled 为切换后的值', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('应用更新')).toBeInTheDocument());
    // 启用开关初始为 true（启用样例），切换为 false
    fireEvent.click(screen.getByLabelText('启用在线更新（出站开关）'));
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    const payload = update.mock.calls[0][0];
    // SettingsPatch 两块可选：只发 update 块，network_proxy 缺省
    expect(payload.update).toBeDefined();
    expect(payload.network_proxy).toBeUndefined();
    expect(payload.update!.enabled).toBe(false);
  });

  it('检查更新（异步）：触发检查 job 后轮询到终态，展示版本对比与立即更新按钮', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    // 触发检查返回 job_id；轮询返回「检查完成、带结果」的终态 job
    vi.spyOn(api, 'triggerCheckUpdate').mockResolvedValue({ job_id: 'job-check' });
    const checkJob: UpdateJob = {
      job_id: 'job-check',
      kind: 'check',
      phase: 'done',
      current_version: '0.3.0',
      latest_version: '0.4.0',
      check: 有更新样例,
    };
    vi.spyOn(api, 'getUpdateJob').mockResolvedValue(checkJob);
    renderPage();

    await waitFor(() => expect(screen.getByText('应用更新')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: '检查更新' }));

    // 轮询到 done 终态后填检查结果
    await waitFor(() => expect(screen.getByText('有可用更新')).toBeInTheDocument());
    expect(screen.getByText('0.4.0')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '立即更新并重启' })).toBeInTheDocument();
  });

  it('进页读留存检查结果：getCachedCheck 有结果时直接展示，无需重点检查', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    const cached: CachedCheck = { result: 有更新样例, checked_at: 1_700_000_000 };
    vi.mocked(api.getCachedCheck).mockResolvedValue(cached);
    renderPage();

    // 不点「检查更新」也应显示上次留存的版本对比
    await waitFor(() => expect(screen.getByText('有可用更新')).toBeInTheDocument());
    expect(screen.getByText('0.4.0')).toBeInTheDocument();
  });

  it('重连续看：listUpdateJobs 含重启后回填终态时，展示「上次更新结果」', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    const restartedJob: UpdateJob = {
      job_id: 'last-apply',
      kind: 'apply',
      phase: 'restarting',
      current_version: '0.3.0',
      new_version: '0.4.0',
      restarted: true,
    };
    vi.mocked(api.listUpdateJobs).mockResolvedValue([restartedJob]);
    renderPage();

    await waitFor(() => expect(screen.getByText('上次更新结果')).toBeInTheDocument());
  });

  it('切到「重启」tab：确认后调 systemRestart 并 notifySuccess', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    const restart = vi.spyOn(api, 'systemRestart').mockResolvedValue(系统操作成功);
    renderPage();

    await waitFor(() => expect(screen.getByText('应用更新')).toBeInTheDocument());
    // 切到重启 tab 后，重启服务按钮才渲染
    fireEvent.click(screen.getByRole('tab', { name: '重启' }));
    fireEvent.click(await screen.findByRole('button', { name: '重启服务' }));

    // 弹出二次确认 Modal
    const dialog = await screen.findByRole('dialog', { name: '确认重启服务' });
    fireEvent.click(within(dialog).getByRole('button', { name: '确认重启' }));

    await waitFor(() => expect(restart).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(vi.mocked(notifySuccess)).toHaveBeenCalledWith(
        '正在重启…当前连接将断开，请稍候片刻后手动刷新页面',
      ),
    );
  });

  it('重启失败 409：notifyError 以「更新进行中，请稍后」被调', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'systemRestart').mockRejectedValue(new ApiError(409, 'conflict', '更新进行中'));
    renderPage();

    await waitFor(() => expect(screen.getByText('应用更新')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('tab', { name: '重启' }));
    fireEvent.click(await screen.findByRole('button', { name: '重启服务' }));

    const dialog = await screen.findByRole('dialog', { name: '确认重启服务' });
    fireEvent.click(within(dialog).getByRole('button', { name: '确认重启' }));

    await waitFor(() => expect(vi.mocked(notifyError)).toHaveBeenCalledWith('更新进行中，请稍后'));
  });

  it('切到「关闭」tab：确认弹窗含警告，确认后调 systemShutdown 并 notifySuccess', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    const shutdown = vi.spyOn(api, 'systemShutdown').mockResolvedValue(系统操作成功);
    renderPage();

    await waitFor(() => expect(screen.getByText('应用更新')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('tab', { name: '关闭' }));
    fireEvent.click(await screen.findByRole('button', { name: '关闭服务' }));

    // 弹出二次确认 Modal，含警告 Alert 标题「警告」
    const dialog = await screen.findByRole('dialog', { name: '确认关闭服务' });
    expect(within(dialog).getByText('警告')).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole('button', { name: '确认关闭' }));

    await waitFor(() => expect(shutdown).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(vi.mocked(notifySuccess)).toHaveBeenCalledWith(
        '正在关闭…服务将停止，需在服务器上重新启动',
      ),
    );
  });

  it('关闭失败 409：notifyError 以「更新进行中，请稍后」被调', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'systemShutdown').mockRejectedValue(new ApiError(409, 'conflict', '更新进行中'));
    renderPage();

    await waitFor(() => expect(screen.getByText('应用更新')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('tab', { name: '关闭' }));
    fireEvent.click(await screen.findByRole('button', { name: '关闭服务' }));

    const dialog = await screen.findByRole('dialog', { name: '确认关闭服务' });
    fireEvent.click(within(dialog).getByRole('button', { name: '确认关闭' }));

    await waitFor(() => expect(vi.mocked(notifyError)).toHaveBeenCalledWith('更新进行中，请稍后'));
  });
});
