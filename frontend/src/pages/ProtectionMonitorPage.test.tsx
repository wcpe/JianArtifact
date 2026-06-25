// 防护状态监控页组件测试（FR-78）：
// 加载后展示各维度窗内计数快照、封禁 IP 数与告警列表；轮询定时刷新；失败展示错误文案。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { ProtectionMonitorPage } from './ProtectionMonitorPage';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type { Paginated, ProtectionAlertDto, ProtectionStatusDto } from '../api/types';

/** 在 Mantine Provider 下渲染监控页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <ProtectionMonitorPage />
    </MantineProvider>,
  );
}

const 样例状态: ProtectionStatusDto = {
  alerts_enabled: true,
  window_secs: 300,
  window_counts: [
    { dimension: 'rate_limit', count: 7 },
    { dimension: 'ban', count: 2 },
    { dimension: 'cc_challenge', count: 0 },
    { dimension: 'waf', count: 5 },
    { dimension: 'slowloris', count: 1 },
  ],
  active_banned_ips: 3,
  dropped_alerts: 0,
  recent_alerts: [],
};

const 样例告警: Paginated<ProtectionAlertDto> = {
  items: [
    {
      id: 2,
      ts: '2026-06-25T01:00:00Z',
      dimension: 'waf',
      severity: 'error',
      observed_value: 600,
      threshold: 500,
      window_secs: 300,
      detail: '窗内计数达阈值',
    },
    {
      id: 1,
      ts: '2026-06-25T00:00:00Z',
      dimension: 'rate_limit',
      severity: 'warn',
      observed_value: 1000,
      threshold: 1000,
      window_secs: 300,
      detail: null,
    },
  ],
  total: 2,
  offset: 0,
  limit: 50,
  has_more: false,
};

describe('ProtectionMonitorPage', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('加载后展示各维度窗内计数与封禁 IP 数', async () => {
    vi.spyOn(api, 'protectionStatus').mockResolvedValue(样例状态);
    vi.spyOn(api, 'listProtectionAlerts').mockResolvedValue(样例告警);
    renderPage();

    // 维度中文标签（限流既出现在维度卡片，也可能出现在告警行，至少存在一处）
    await waitFor(() => expect(screen.getAllByText('限流').length).toBeGreaterThan(0));
    expect(screen.getAllByText('WAF 阻断').length).toBeGreaterThan(0);
    // 维度计数（rate_limit=7）
    expect(screen.getByText('7')).toBeInTheDocument();
    // 当前封禁 IP 数
    expect(screen.getByText('3')).toBeInTheDocument();
  });

  it('展示告警列表（含维度、严重度、观测值/阈值）', async () => {
    vi.spyOn(api, 'protectionStatus').mockResolvedValue(样例状态);
    vi.spyOn(api, 'listProtectionAlerts').mockResolvedValue(样例告警);
    renderPage();

    await waitFor(() => expect(screen.getByText('窗内计数达阈值')).toBeInTheDocument());
    // 严重度中文标签
    expect(screen.getByText('错误')).toBeInTheDocument();
    expect(screen.getByText('警告')).toBeInTheDocument();
  });

  it('无告警时展示空态文案', async () => {
    vi.spyOn(api, 'protectionStatus').mockResolvedValue(样例状态);
    vi.spyOn(api, 'listProtectionAlerts').mockResolvedValue({
      items: [],
      total: 0,
      offset: 0,
      limit: 50,
      has_more: false,
    });
    renderPage();

    await waitFor(() => expect(screen.getByText('暂无告警记录')).toBeInTheDocument());
  });

  it('轮询定时刷新状态快照', async () => {
    vi.useFakeTimers();
    const statusSpy = vi.spyOn(api, 'protectionStatus').mockResolvedValue(样例状态);
    vi.spyOn(api, 'listProtectionAlerts').mockResolvedValue(样例告警);
    renderPage();

    // 首次加载调用一次
    await vi.waitFor(() => expect(statusSpy).toHaveBeenCalledTimes(1));

    // 推进一个轮询周期后应再次拉取
    await vi.advanceTimersByTimeAsync(5000);
    expect(statusSpy.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it('请求失败时展示错误提示', async () => {
    vi.spyOn(api, 'protectionStatus').mockRejectedValue(
      new ApiError(403, 'forbidden', '无权执行该操作'),
    );
    vi.spyOn(api, 'listProtectionAlerts').mockRejectedValue(
      new ApiError(403, 'forbidden', '无权执行该操作'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('无权执行该操作')).toBeInTheDocument());
  });
});
