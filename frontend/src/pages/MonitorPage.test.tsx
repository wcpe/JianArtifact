// 统一监控页测试（FR-99）：
// 1) 渲染四个 tab，默认主机监控 tab 展示 CPU/内存/磁盘/uptime 结构（mock host 端点）；
// 2) 切到使用分析 / 审计 / 防护 tab 时复用既有页组件、数据可渲染（mock 各自端点）；
// 3) 主机 tab 刷新按钮再次拉取。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { MonitorPage } from './MonitorPage';
import * as api from '../api/endpoints';
import type {
  HostMetrics,
  Paginated,
  AuditEntryDto,
  ProtectionStatusDto,
  ProtectionAlertDto,
  UsageAnalyticsDto,
} from '../api/types';

/** 在 Mantine Provider 下渲染监控页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <MonitorPage />
    </MantineProvider>,
  );
}

const 样例主机指标: HostMetrics = {
  cpu: { usage_percent: 42.5, logical_cores: 8 },
  memory: {
    total_bytes: 16 * 1024 * 1024 * 1024,
    used_bytes: 8 * 1024 * 1024 * 1024,
    swap_total_bytes: 2 * 1024 * 1024 * 1024,
    swap_used_bytes: 0,
  },
  disk: {
    total_bytes: 500 * 1024 * 1024 * 1024,
    available_bytes: 200 * 1024 * 1024 * 1024,
    disks: [
      {
        mount_point: 'C:\\',
        total_bytes: 500 * 1024 * 1024 * 1024,
        available_bytes: 200 * 1024 * 1024 * 1024,
      },
    ],
  },
  uptime_secs: 3661,
};

const 样例使用分析: UsageAnalyticsDto = {
  total_access: 12,
  total_download: 34,
  top_downloads: [
    { repo_name: 'libs', repo_path: 'a/b.jar', count: 9, last_at: '2026-06-24T00:00:00Z' },
  ],
  repo_usage: [{ repo_name: 'libs', count: 9 }],
};

const 空审计: Paginated<AuditEntryDto> = {
  items: [],
  total: 0,
  offset: 0,
  limit: 50,
  has_more: false,
};

const 样例防护状态: ProtectionStatusDto = {
  alerts_enabled: true,
  window_secs: 300,
  window_counts: [{ dimension: 'rate_limit', count: 7 }],
  active_banned_ips: 3,
  dropped_alerts: 0,
  recent_alerts: [],
};

const 空告警: Paginated<ProtectionAlertDto> = {
  items: [],
  total: 0,
  offset: 0,
  limit: 50,
  has_more: false,
};

/** 统一桩：mock 四个 tab 各自端点，避免 useEffect 触发未处理拒绝。 */
function 桩全部端点() {
  vi.spyOn(api, 'getHostMonitor').mockResolvedValue(样例主机指标);
  vi.spyOn(api, 'usageAnalytics').mockResolvedValue(样例使用分析);
  vi.spyOn(api, 'listAudit').mockResolvedValue(空审计);
  vi.spyOn(api, 'protectionStatus').mockResolvedValue(样例防护状态);
  vi.spyOn(api, 'listProtectionAlerts').mockResolvedValue(空告警);
}

describe('MonitorPage', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('渲染四个 tab', async () => {
    桩全部端点();
    renderPage();

    expect(screen.getByRole('tab', { name: '主机监控' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: '使用分析' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: '审计' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: '防护' })).toBeInTheDocument();
  });

  it('默认主机监控 tab：展示 CPU 核数、内存与磁盘结构、uptime', async () => {
    桩全部端点();
    renderPage();

    // 逻辑核数
    await waitFor(() => expect(screen.getByText(/8 核/)).toBeInTheDocument());
    // CPU / 内存 / 磁盘环形标签
    expect(screen.getByRole('img', { name: /CPU/ })).toBeInTheDocument();
    expect(screen.getByRole('img', { name: /内存/ })).toBeInTheDocument();
    expect(screen.getByRole('img', { name: /磁盘/ })).toBeInTheDocument();
    // 磁盘挂载点
    expect(screen.getByText('C:\\')).toBeInTheDocument();
  });

  it('主机 tab 刷新按钮再次拉取指标', async () => {
    桩全部端点();
    const spy = vi.mocked(api.getHostMonitor);
    renderPage();

    await waitFor(() => expect(spy).toHaveBeenCalledTimes(1));
    await userEvent.click(screen.getByRole('button', { name: '刷新' }));
    await waitFor(() => expect(spy.mock.calls.length).toBeGreaterThanOrEqual(2));
  });

  it('切到使用分析 tab 复用既有页，展示总量', async () => {
    桩全部端点();
    renderPage();

    await userEvent.click(screen.getByRole('tab', { name: '使用分析' }));
    await waitFor(() => expect(screen.getByText('12')).toBeInTheDocument());
    expect(screen.getByText('34')).toBeInTheDocument();
  });

  it('切到审计 tab 复用既有页，展示空态', async () => {
    桩全部端点();
    renderPage();

    await userEvent.click(screen.getByRole('tab', { name: '审计' }));
    await waitFor(() => expect(screen.getByText('暂无审计记录。')).toBeInTheDocument());
  });

  it('切到防护 tab 复用既有页，展示封禁 IP 数', async () => {
    桩全部端点();
    renderPage();

    await userEvent.click(screen.getByRole('tab', { name: '防护' }));
    await waitFor(() => expect(screen.getByText('当前封禁 IP 数')).toBeInTheDocument());
  });
});
