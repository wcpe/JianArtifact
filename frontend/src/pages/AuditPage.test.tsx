// 审计日志查询页面测试（FR-77）：加载后分页表格展示，支持过滤、行详情；失败展示错误文案。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { AuditPage } from './AuditPage';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type { AuditEntryDto, Paginated } from '../api/types';

/** 在 Mantine Provider 下渲染审计页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <AuditPage />
    </MantineProvider>,
  );
}

/** 构造一条审计记录。 */
function 记录(overrides: Partial<AuditEntryDto> = {}): AuditEntryDto {
  return {
    id: 1,
    ts: '2026-06-25T08:00:00Z',
    actor: 'alice',
    actor_kind: 'session',
    request_id: 'req-1',
    source_ip: '127.0.0.1',
    action: 'repo.create',
    target_repo: 'libs',
    target: null,
    result: 'success',
    detail: null,
    ...overrides,
  };
}

/** 构造一页响应。 */
function 一页(
  items: AuditEntryDto[],
  total = items.length,
  offset = 0,
  limit = 50,
): Paginated<AuditEntryDto> {
  return { items, total, offset, limit, has_more: offset + items.length < total };
}

describe('AuditPage', () => {
  afterEach(() => vi.restoreAllMocks());

  it('加载后以表格展示审计记录（含主体 / 动作 / 仓库 / 结果）', async () => {
    vi.spyOn(api, 'listAudit').mockResolvedValue(
      一页([记录({ id: 1, actor: 'alice', action: 'repo.create', target_repo: 'libs' })]),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument());
    // 动作经 i18n 显示中文标签（FR-111）：repo.create → 创建仓库
    expect(screen.getByText('创建仓库')).toBeInTheDocument();
    expect(screen.getByText('libs')).toBeInTheDocument();
    // 结果徽章
    expect(screen.getByText('success')).toBeInTheDocument();
  });

  it('无记录时展示空态文案', async () => {
    vi.spyOn(api, 'listAudit').mockResolvedValue(一页([], 0));
    renderPage();

    await waitFor(() => expect(screen.getByText('暂无审计记录。')).toBeInTheDocument());
  });

  it('填写过滤条件并查询时按参数请求', async () => {
    const spy = vi.spyOn(api, 'listAudit').mockResolvedValue(一页([记录()]));
    renderPage();
    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument());

    await userEvent.type(screen.getByLabelText('操作者'), 'bob');
    await userEvent.type(screen.getByLabelText('动作'), 'repo.delete');
    await userEvent.type(screen.getByLabelText('仓库'), 'npm-proxy');
    await userEvent.click(screen.getByRole('button', { name: '查询' }));

    await waitFor(() =>
      expect(spy).toHaveBeenLastCalledWith(
        expect.objectContaining({
          actor: 'bob',
          action: 'repo.delete',
          target_repo: 'npm-proxy',
          offset: 0,
        }),
      ),
    );
  });

  it('点击记录行打开详情，展示请求 ID / 来源 IP / target 等', async () => {
    vi.spyOn(api, 'listAudit').mockResolvedValue(
      一页([
        记录({
          id: 7,
          request_id: 'req-xyz',
          source_ip: '10.0.0.5',
          target: 'a/b/c.txt',
          detail: '{"k":"v"}',
        }),
      ]),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument());

    // 动作列经 i18n 显示中文标签（FR-111）：repo.create → 创建仓库
    await userEvent.click(screen.getByText('创建仓库'));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('req-xyz')).toBeInTheDocument();
    expect(within(dialog).getByText('10.0.0.5')).toBeInTheDocument();
    expect(within(dialog).getByText('a/b/c.txt')).toBeInTheDocument();
    expect(within(dialog).getByText(/"k":"v"/)).toBeInTheDocument();
  });

  it('翻页时按 offset 请求下一页', async () => {
    // 总数 60，单页 50：应出现分页控件，点第二页按 offset=50 请求
    const spy = vi.spyOn(api, 'listAudit').mockResolvedValue(一页([记录({ id: 1 })], 60, 0, 50));
    renderPage();
    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument());

    spy.mockResolvedValue(一页([记录({ id: 60, actor: 'zoe' })], 60, 50, 50));
    await userEvent.click(screen.getByRole('button', { name: '2' }));

    await waitFor(() =>
      expect(spy).toHaveBeenLastCalledWith(expect.objectContaining({ offset: 50 })),
    );
  });

  it('请求失败时展示错误提示', async () => {
    vi.spyOn(api, 'listAudit').mockRejectedValue(new ApiError(403, 'forbidden', '无权执行该操作'));
    renderPage();

    await waitFor(() => expect(screen.getByText('无权执行该操作')).toBeInTheDocument());
  });
});
