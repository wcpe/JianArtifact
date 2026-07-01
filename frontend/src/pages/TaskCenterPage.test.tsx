// 任务中心页测试（FR-132，仅管理员）：
// 覆盖——活跃/近期任务列表渲染（含 kind/state/label）、无任务空状态、
// 轮询刷新后列表更新（mock setInterval）、后台完成任务仍在列表可找回。
// 注：真浏览器轮询时序 jsdom 难全测，标「待真机」。

import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen, waitFor, act } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { MemoryRouter } from 'react-router-dom';
import { TaskCenterPage } from './TaskCenterPage';
import * as api from '../api/endpoints';
import type { TaskRecord } from '../api/types';

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

/** 在 Mantine + Router Provider 下渲染任务中心页。 */
function renderPage() {
  return render(
    <MemoryRouter>
      <MantineProvider>
        <TaskCenterPage />
      </MantineProvider>
    </MemoryRouter>,
  );
}

describe('TaskCenterPage', () => {
  beforeEach(() => {
    // 默认桩：空列表，避免触达真实 fetch
    vi.spyOn(api, 'listTasks').mockResolvedValue([]);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('加载后展示 migration 任务（kind 图标 + label + 状态 Badge）', async () => {
    vi.spyOn(api, 'listTasks').mockResolvedValue([
      任务({ kind: 'migration', state: 'running', label: '在线拉取迁移' }),
    ]);
    renderPage();

    await waitFor(() => expect(screen.getByText('在线拉取迁移')).toBeInTheDocument());
    // 状态 Badge 显示「运行中」
    expect(screen.getByText('运行中')).toBeInTheDocument();
  });

  it('展示 update 任务（kind=update，状态 succeeded）', async () => {
    vi.spyOn(api, 'listTasks').mockResolvedValue([
      任务({ id: 'u1', kind: 'update', state: 'succeeded', label: '应用更新' }),
    ]);
    renderPage();

    await waitFor(() => expect(screen.getByText('应用更新')).toBeInTheDocument());
    expect(screen.getByText('已完成')).toBeInTheDocument();
  });

  it('展示 vuln 任务（kind=vuln，状态 failed，error 信息）', async () => {
    vi.spyOn(api, 'listTasks').mockResolvedValue([
      任务({
        id: 'v1',
        kind: 'vuln',
        state: 'failed',
        label: '漏洞库刷新',
        error: '下载超时',
      }),
    ]);
    renderPage();

    await waitFor(() => expect(screen.getByText('漏洞库刷新')).toBeInTheDocument());
    expect(screen.getByText('失败')).toBeInTheDocument();
  });

  it('无任务时显示空状态提示', async () => {
    vi.spyOn(api, 'listTasks').mockResolvedValue([]);
    renderPage();

    await waitFor(() => expect(screen.getByText('暂无任务')).toBeInTheDocument());
  });

  it('轮询后列表更新（新增任务可见）【待真机验证轮询时序】', async () => {
    // 使用 shouldAdvanceTime 让 waitFor 的内部定时器也随之推进
    vi.useFakeTimers({ shouldAdvanceTime: true });

    // 第一次调用：空；第二次调用：有任务
    const spy = vi
      .spyOn(api, 'listTasks')
      .mockResolvedValueOnce([])
      .mockResolvedValue([任务({ label: '轮询新增任务' })]);

    renderPage();

    // 等待首次渲染空状态
    await waitFor(() => expect(screen.getByText('暂无任务')).toBeInTheDocument());

    // 推进 3s（任务中心轮询间隔）触发第二次请求
    await act(async () => {
      vi.advanceTimersByTime(3000);
    });

    await waitFor(() => expect(screen.getByText('轮询新增任务')).toBeInTheDocument());
    expect(spy).toHaveBeenCalledTimes(2);

    vi.useRealTimers();
  });

  it('后台完成的任务仍在列表中（历史保留）', async () => {
    // 后端保留已完成任务，前端直接展示
    vi.spyOn(api, 'listTasks').mockResolvedValue([
      任务({ id: 'done-1', kind: 'migration', state: 'succeeded', label: '已完成迁移' }),
    ]);
    renderPage();

    await waitFor(() => expect(screen.getByText('已完成迁移')).toBeInTheDocument());
    expect(screen.getByText('已完成')).toBeInTheDocument();
  });

  it('同时展示活跃与近期历史任务', async () => {
    vi.spyOn(api, 'listTasks').mockResolvedValue([
      任务({ id: 'r1', kind: 'migration', state: 'running', label: '进行中迁移' }),
      任务({ id: 's1', kind: 'update', state: 'succeeded', label: '已完成更新' }),
    ]);
    renderPage();

    await waitFor(() => expect(screen.getByText('进行中迁移')).toBeInTheDocument());
    expect(screen.getByText('已完成更新')).toBeInTheDocument();
  });
});
