// 用户组管理页测试：渲染组列表、新增组触发创建调用、删除组带二次确认。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { GroupsPage } from './GroupsPage';
import * as api from '../api/endpoints';
import type { GroupView } from '../api/types';

// 桩掉端点模块：组管理页直接调用 api.*
vi.mock('../api/endpoints');
// 桩掉通知，避免依赖 Notifications Provider
vi.mock('../lib/notify', () => ({
  notifySuccess: vi.fn(),
  notifyError: vi.fn(),
}));

const mockedApi = vi.mocked(api);

const GROUPS: GroupView[] = [{ id: 'g1', name: 'dev-team', created_at: '2026-01-01T00:00:00Z' }];

/** 在 Mantine Provider 下渲染组管理页。 */
function renderGroups() {
  return render(
    <MantineProvider>
      <GroupsPage />
    </MantineProvider>,
  );
}

describe('GroupsPage', () => {
  beforeEach(() => {
    mockedApi.listGroups.mockResolvedValue([...GROUPS]);
    mockedApi.createGroup.mockResolvedValue({
      id: 'g2',
      name: 'ops',
      created_at: '2026-01-02T00:00:00Z',
    });
    mockedApi.deleteGroup.mockResolvedValue(undefined);
  });
  afterEach(() => vi.clearAllMocks());

  it('加载后渲染已有用户组', async () => {
    renderGroups();
    expect(await screen.findByText('dev-team')).toBeInTheDocument();
    expect(mockedApi.listGroups).toHaveBeenCalled();
  });

  it('新增用户组填名提交后调用 createGroup', async () => {
    renderGroups();
    await screen.findByText('dev-team');
    const user = userEvent.setup();

    await user.click(screen.getByRole('button', { name: '新增用户组' }));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByPlaceholderText('如 dev-team'), 'ops');
    await user.click(within(dialog).getByRole('button', { name: '创建' }));

    await waitFor(() => expect(mockedApi.createGroup).toHaveBeenCalledWith('ops'));
  });

  it('删除用户组经二次确认后调用 deleteGroup', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
    renderGroups();
    await screen.findByText('dev-team');
    const user = userEvent.setup();

    await user.click(screen.getByRole('button', { name: '删除用户组' }));

    await waitFor(() => expect(mockedApi.deleteGroup).toHaveBeenCalledWith('g1'));
    confirmSpy.mockRestore();
  });
});
