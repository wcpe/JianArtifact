// 系统日志页面测试（FR-107）：加载后分页表格展示，支持级别过滤、刷新；空态与失败展示文案。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { SystemLogsPage } from './SystemLogsPage';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type { SystemLogEntryDto, Paginated } from '../api/types';

/** 在 Mantine Provider 下渲染系统日志页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <SystemLogsPage />
    </MantineProvider>,
  );
}

/** 构造一条日志记录。 */
function 记录(overrides: Partial<SystemLogEntryDto> = {}): SystemLogEntryDto {
  return {
    timestamp: '2026-06-27T08:00:00.000000Z',
    level: 'INFO',
    message: 'jianartifact: 服务启动',
    ...overrides,
  };
}

/** 构造一页响应。 */
function 一页(
  items: SystemLogEntryDto[],
  total = items.length,
  offset = 0,
  limit = 200,
): Paginated<SystemLogEntryDto> {
  return { items, total, offset, limit, has_more: offset + items.length < total };
}

describe('SystemLogsPage', () => {
  afterEach(() => vi.restoreAllMocks());

  it('加载后以表格展示日志记录（含时间 / 级别 / 消息）', async () => {
    vi.spyOn(api, 'listSystemLogs').mockResolvedValue(
      一页([记录({ level: 'ERROR', message: 'jianartifact: 出错了' })]),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('jianartifact: 出错了')).toBeInTheDocument());
    // 级别徽章：scope 到该消息所在行（避免与级别下拉的隐藏 option 同名冲突）
    const row = screen.getByText('jianartifact: 出错了').closest('tr')!;
    expect(within(row).getByText('ERROR')).toBeInTheDocument();
    // 时间列
    expect(within(row).getByText('2026-06-27T08:00:00.000000Z')).toBeInTheDocument();
  });

  it('无记录时展示空态文案', async () => {
    vi.spyOn(api, 'listSystemLogs').mockResolvedValue(一页([], 0));
    renderPage();

    await waitFor(() => expect(screen.getByText('暂无日志记录。')).toBeInTheDocument());
  });

  it('选择级别后按 level 参数请求', async () => {
    const spy = vi.spyOn(api, 'listSystemLogs').mockResolvedValue(一页([记录()]));
    renderPage();
    await waitFor(() => expect(screen.getByText('jianartifact: 服务启动')).toBeInTheDocument());

    // 打开级别下拉（点击显示当前值的输入）并选 ERROR
    await userEvent.click(screen.getByDisplayValue('全部级别'));
    await userEvent.click(await screen.findByRole('option', { name: 'ERROR' }));

    await waitFor(() =>
      expect(spy).toHaveBeenLastCalledWith(expect.objectContaining({ level: 'ERROR', offset: 0 })),
    );
  });

  it('点击刷新时重新请求', async () => {
    const spy = vi.spyOn(api, 'listSystemLogs').mockResolvedValue(一页([记录()]));
    renderPage();
    await waitFor(() => expect(screen.getByText('jianartifact: 服务启动')).toBeInTheDocument());
    const before = spy.mock.calls.length;

    await userEvent.click(screen.getByRole('button', { name: '刷新' }));

    await waitFor(() => expect(spy.mock.calls.length).toBeGreaterThan(before));
  });

  it('无级别的行以占位符展示级别与时间', async () => {
    vi.spyOn(api, 'listSystemLogs').mockResolvedValue(
      一页([记录({ timestamp: null, level: null, message: '一行无法解析的输出' })]),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('一行无法解析的输出')).toBeInTheDocument());
    const row = screen.getByText('一行无法解析的输出').closest('tr')!;
    // 时间与级别两列均以 — 占位
    expect(within(row).getAllByText('—').length).toBeGreaterThanOrEqual(2);
  });

  it('请求失败时展示错误提示', async () => {
    vi.spyOn(api, 'listSystemLogs').mockRejectedValue(
      new ApiError(403, 'forbidden', '无权执行该操作'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('无权执行该操作')).toBeInTheDocument());
  });
});
