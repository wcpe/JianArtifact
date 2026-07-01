// 通知中心组件测试（FR-132）：
// 覆盖——图标渲染（仅 Admin 可见）、非 Admin 不渲染、
// 状态跃迁推通知（running→succeeded / running→failed / running→cancelled）、
// 新出现 running 任务推「已开始」通知、轮询错误静默忽略。
// 注：真浏览器轮询 jsdom 难全测，标「待真机」。

import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen, waitFor, act } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { Notifications } from '@mantine/notifications';
import { MemoryRouter } from 'react-router-dom';
import { NotificationCenter } from './NotificationCenter';
import * as api from '../api/endpoints';
import type { TaskRecord } from '../api/types';

// 桩掉 notify 库，避免依赖 Notifications Provider
vi.mock('../lib/notify', () => ({
  notifySuccess: vi.fn(),
  notifyError: vi.fn(),
}));

import { notifySuccess, notifyError } from '../lib/notify';

/** 构造一条任务记录。 */
function 任务(overrides: Partial<TaskRecord> = {}): TaskRecord {
  return {
    id: 'task-1',
    kind: 'migration',
    state: 'running',
    label: '在线拉取迁移',
    started_at: Math.floor(Date.now() / 1000) - 60,
    updated_at: Math.floor(Date.now() / 1000),
    ...overrides,
  };
}

/** 在 Provider 下渲染通知中心组件。 */
function renderComponent(isAdmin = true) {
  return render(
    <MemoryRouter>
      <MantineProvider>
        <Notifications />
        <NotificationCenter isAdmin={isAdmin} />
      </MantineProvider>
    </MemoryRouter>,
  );
}

describe('NotificationCenter', () => {
  beforeEach(() => {
    vi.spyOn(api, 'listTasks').mockResolvedValue([]);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('Admin 可见：渲染铃铛图标按钮', async () => {
    renderComponent(true);
    // 组件挂载即可见图标按钮（aria-label）
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /任务通知中心/i })).toBeInTheDocument(),
    );
  });

  it('非 Admin 时不渲染', () => {
    renderComponent(false);
    expect(screen.queryByRole('button', { name: /任务通知中心/i })).not.toBeInTheDocument();
  });

  it('状态跃迁 running→succeeded 推「已完成」通知【待真机验证轮询】', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });

    // 第一次：running；第二次：succeeded（跃迁）
    vi.spyOn(api, 'listTasks')
      .mockResolvedValueOnce([任务({ id: 't1', state: 'running', label: '迁移任务' })])
      .mockResolvedValue([任务({ id: 't1', state: 'succeeded', label: '迁移任务' })]);

    renderComponent(true);

    // 首次加载，建立快照
    await waitFor(() => expect(api.listTasks).toHaveBeenCalledTimes(1));

    // 推进 5s 触发第二次轮询（通知中心间隔 5s）
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });

    await waitFor(() =>
      expect(notifySuccess).toHaveBeenCalledWith(expect.stringContaining('迁移任务')),
    );

    vi.useRealTimers();
  });

  it('状态跃迁 running→failed 推「失败」通知【待真机验证轮询】', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });

    vi.spyOn(api, 'listTasks')
      .mockResolvedValueOnce([任务({ id: 't2', state: 'running', label: '更新任务' })])
      .mockResolvedValue([任务({ id: 't2', state: 'failed', label: '更新任务' })]);

    renderComponent(true);

    await waitFor(() => expect(api.listTasks).toHaveBeenCalledTimes(1));

    await act(async () => {
      vi.advanceTimersByTime(5000);
    });

    await waitFor(() =>
      expect(notifyError).toHaveBeenCalledWith(expect.stringContaining('更新任务')),
    );

    vi.useRealTimers();
  });

  it('状态跃迁 running→cancelled 推「已取消」通知【待真机验证轮询】', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });

    vi.spyOn(api, 'listTasks')
      .mockResolvedValueOnce([任务({ id: 't3', state: 'running', label: '漏洞库刷新' })])
      .mockResolvedValue([任务({ id: 't3', state: 'cancelled', label: '漏洞库刷新' })]);

    renderComponent(true);

    await waitFor(() => expect(api.listTasks).toHaveBeenCalledTimes(1));

    await act(async () => {
      vi.advanceTimersByTime(5000);
    });

    // cancelled 用 notifySuccess（灰色消息）
    await waitFor(() =>
      expect(notifySuccess).toHaveBeenCalledWith(expect.stringContaining('漏洞库刷新')),
    );

    vi.useRealTimers();
  });

  it('新出现 running 任务推「已开始」通知【待真机验证轮询】', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });

    // 第一次：空；第二次：新出现 running 任务
    vi.spyOn(api, 'listTasks')
      .mockResolvedValueOnce([])
      .mockResolvedValue([任务({ id: 'new-1', state: 'running', label: '新任务' })]);

    renderComponent(true);

    await waitFor(() => expect(api.listTasks).toHaveBeenCalledTimes(1));

    await act(async () => {
      vi.advanceTimersByTime(5000);
    });

    await waitFor(() =>
      expect(notifySuccess).toHaveBeenCalledWith(expect.stringContaining('新任务')),
    );

    vi.useRealTimers();
  });

  it('轮询错误静默忽略，不影响渲染', async () => {
    vi.spyOn(api, 'listTasks').mockRejectedValue(new Error('网络错误'));

    renderComponent(true);

    // 即使 API 出错，组件仍正常显示图标按钮
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /任务通知中心/i })).toBeInTheDocument(),
    );
  });
});
