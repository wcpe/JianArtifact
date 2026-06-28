// 仪表盘全局状态概览测试（FR-108）：
// 1) 管理员：4 张 KPI 卡（仓库 / 制品 / 存储用量 / 用户数），存储字节人类可读、计数千分位；
// 2) 主机健康三条进度条按 monitor/host 数据渲染；
// 3) 近期活动列出审计最近事件（带相对时间）；
// 4) 系统状态四项（更新 / 防护 / 漏洞库 / 运行时长）；
// 5) 在线更新未启用（409）静默不报错；
// 6) 非管理员降级：仅见基础信息（可见仓库数），不调管理端点、不报 403。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { DashboardPage } from './DashboardPage';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type {
  DashboardSummary,
  HostMetrics,
  AuditEntryDto,
  RepositoryDto,
  UpdateCheck,
  DynamicConfig,
  ProtectionStatusDto,
  Paginated,
} from '../api/types';

vi.mock('../api/endpoints');

// 默认桩为管理员；个别用例覆盖为普通用户
let mockIsAdmin = true;
let mockUsername = 'admin';
vi.mock('../auth/useAuth', () => ({
  useAuth: () => ({
    isAdmin: mockIsAdmin,
    user: { id: 'u1', username: mockUsername, role: mockIsAdmin ? 'admin' : 'user' },
  }),
}));

const mockedApi = vi.mocked(api);

const SUMMARY: DashboardSummary = {
  repo_count: 12,
  artifact_count: 3456,
  // 13 GB 左右的字节，确认渲染为人类可读而非原始字节
  total_bytes: 13_958_643_712,
  user_count: 7,
};

const HOST: HostMetrics = {
  cpu: { usage_percent: 42, logical_cores: 8 },
  memory: { total_bytes: 100, used_bytes: 60, swap_total_bytes: 0, swap_used_bytes: 0 },
  disk: {
    total_bytes: 200,
    available_bytes: 50,
    disks: [{ mount_point: '/', total_bytes: 200, available_bytes: 50 }],
  },
  uptime_secs: 3 * 86400 + 4 * 3600,
};

function auditPage(entries: AuditEntryDto[]): Paginated<AuditEntryDto> {
  return { items: entries, total: entries.length, offset: 0, limit: 10, has_more: false };
}

const RECENT: AuditEntryDto[] = [
  {
    id: 2,
    ts: new Date(Date.now() - 5 * 60 * 1000).toISOString(),
    actor: 'alice',
    actor_kind: 'user',
    request_id: null,
    source_ip: null,
    action: 'artifact.upload',
    target_repo: 'maven-hosted',
    target: 'com/x/a.jar',
    result: 'ok',
    detail: null,
  },
];

const UPDATE_AVAILABLE: UpdateCheck = {
  current_version: '0.4.0',
  latest_version: '0.5.0',
  update_available: true,
  asset_name: 'x',
  notes: '',
};

function dynamicWithVuln(enabled: boolean): DynamicConfig {
  return {
    limits: { max_artifact_size: null },
    audit: { retention_days: 30, max_rows: 100000 },
    usage: { detail_enabled: false, max_detail_rows: 100000 },
    metrics: { enabled: false, allow_anonymous: false },
    metrics_timeseries: {
      enabled: true,
      sample_interval_secs: 60,
      retention_days: 7,
      max_rows: 100000,
    },
    vuln: {
      enabled,
      source_base_url: '',
      ecosystems: [],
      refresh_interval_secs: 3600,
      download_timeout_secs: 60,
    },
    auth: { session_ttl_secs: 3600, login_max_failures: 5, login_lockout_secs: 900 },
  };
}

const PROTECTION_OK: ProtectionStatusDto = {
  alerts_enabled: true,
  window_secs: 60,
  window_counts: [],
  active_banned_ips: 0,
  dropped_alerts: 0,
  recent_alerts: [],
};

const REPOS: RepositoryDto[] = [
  {
    id: 'r1',
    name: 'maven-hosted',
    format: 'maven',
    type: 'hosted',
    visibility: 'public',
    upstream_url: null,
    created_at: '2026-01-01T00:00:00Z',
  },
];

function renderPage() {
  return render(
    <MantineProvider>
      <DashboardPage />
    </MantineProvider>,
  );
}

describe('DashboardPage（全局状态概览）', () => {
  beforeEach(() => {
    mockIsAdmin = true;
    mockUsername = 'admin';
    // 管理员路径默认桩齐全部端点为成功
    mockedApi.getDashboardSummary.mockResolvedValue(SUMMARY);
    mockedApi.getHostMonitor.mockResolvedValue(HOST);
    mockedApi.listAudit.mockResolvedValue(auditPage(RECENT));
    mockedApi.checkUpdate.mockResolvedValue(UPDATE_AVAILABLE);
    mockedApi.getDynamicConfig.mockResolvedValue(dynamicWithVuln(true));
    mockedApi.protectionStatus.mockResolvedValue(PROTECTION_OK);
    mockedApi.listRepositories.mockResolvedValue(REPOS);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('管理员：渲染 4 张 KPI 卡，存储用量人类可读、计数千分位', async () => {
    renderPage();

    await waitFor(() => expect(screen.getByText('仓库数')).toBeInTheDocument());
    expect(screen.getByText('制品数')).toBeInTheDocument();
    expect(screen.getByText('存储用量')).toBeInTheDocument();
    expect(screen.getByText('用户数')).toBeInTheDocument();

    // 计数千分位
    expect(screen.getByText('3,456')).toBeInTheDocument();
    expect(screen.getByText('12')).toBeInTheDocument();
    expect(screen.getByText('7')).toBeInTheDocument();

    // 存储用量人类可读（13 GB 量级），且绝不是原始字节串
    expect(screen.getByText('13.00 GB')).toBeInTheDocument();
    expect(screen.queryByText('13958643712 B')).not.toBeInTheDocument();
    expect(screen.queryByText(String(SUMMARY.total_bytes))).not.toBeInTheDocument();
  });

  it('管理员：主机健康渲染 CPU/内存/磁盘三条进度条', async () => {
    const { container } = renderPage();
    await waitFor(() => expect(screen.getByText('主机健康')).toBeInTheDocument());

    expect(screen.getByText('CPU')).toBeInTheDocument();
    expect(screen.getByText('内存')).toBeInTheDocument();
    expect(screen.getByText('磁盘')).toBeInTheDocument();
    // Mantine Progress 渲染为带 role="progressbar" 的元素，应有 3 条
    await waitFor(() =>
      expect(container.querySelectorAll('[role="progressbar"]').length).toBeGreaterThanOrEqual(3),
    );
  });

  it('管理员：近期活动列出审计事件与相对时间', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('近期活动')).toBeInTheDocument());
    // 动作经 i18n 显示中文标签（FR-111）：artifact.upload → 上传制品（不再显示原始 key）
    expect(screen.getByText('上传制品')).toBeInTheDocument();
    expect(screen.queryByText(/artifact\.upload/)).not.toBeInTheDocument();
    expect(screen.getByText(/alice/)).toBeInTheDocument();
    // 相对时间（5 分钟前）
    expect(screen.getByText('5 分钟前')).toBeInTheDocument();
  });

  it('管理员：系统状态四项（更新有新版 / 防护 / 漏洞库启用 / 运行时长）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('系统状态')).toBeInTheDocument());

    expect(screen.getByText('在线更新')).toBeInTheDocument();
    expect(screen.getByText('七层防护')).toBeInTheDocument();
    expect(screen.getByText('漏洞库')).toBeInTheDocument();
    expect(screen.getByText('运行时长')).toBeInTheDocument();

    // 有新版徽标
    expect(screen.getByText(/0\.5\.0/)).toBeInTheDocument();
    // 漏洞库启用
    expect(screen.getByText('已启用')).toBeInTheDocument();
    // 运行时长人类可读
    expect(screen.getByText('3 天 4 小时')).toBeInTheDocument();
  });

  it('管理员：在线更新未启用（409）静默不报错', async () => {
    mockedApi.checkUpdate.mockRejectedValue(new ApiError(409, 'conflict', '在线更新未启用'));
    renderPage();

    await waitFor(() => expect(screen.getByText('系统状态')).toBeInTheDocument());
    // 不把 409 当错误弹出；更新项显示「未启用」语义
    expect(screen.queryByText('在线更新未启用')).not.toBeInTheDocument();
    expect(screen.getByText('未启用')).toBeInTheDocument();
  });

  it('非管理员：仅见基础信息（可见仓库数），不调任何管理端点', async () => {
    mockIsAdmin = false;
    mockUsername = 'bob';
    renderPage();

    await waitFor(() => expect(screen.getByText('可见仓库数')).toBeInTheDocument());
    // 可见仓库数取自 listRepositories
    expect(screen.getByText('1')).toBeInTheDocument();

    // 不调用任何仅管理员端点
    expect(mockedApi.getDashboardSummary).not.toHaveBeenCalled();
    expect(mockedApi.getHostMonitor).not.toHaveBeenCalled();
    expect(mockedApi.listAudit).not.toHaveBeenCalled();
    expect(mockedApi.checkUpdate).not.toHaveBeenCalled();
    expect(mockedApi.getDynamicConfig).not.toHaveBeenCalled();
    expect(mockedApi.protectionStatus).not.toHaveBeenCalled();
    // 富区不渲染
    expect(screen.queryByText('主机健康')).not.toBeInTheDocument();
    expect(screen.queryByText('系统状态')).not.toBeInTheDocument();
  });
});

// FR-112：仪表盘加载体验——顶部进度条 / 骨架占位 / 内容淡入 / 主机 5s 实时轮询（离开页面暂停）。
describe('DashboardPage（FR-112 加载体验）', () => {
  beforeEach(() => {
    mockIsAdmin = true;
    mockUsername = 'admin';
    mockedApi.getDashboardSummary.mockResolvedValue(SUMMARY);
    mockedApi.getHostMonitor.mockResolvedValue(HOST);
    mockedApi.listAudit.mockResolvedValue(auditPage(RECENT));
    mockedApi.checkUpdate.mockResolvedValue(UPDATE_AVAILABLE);
    mockedApi.getDynamicConfig.mockResolvedValue(dynamicWithVuln(true));
    mockedApi.protectionStatus.mockResolvedValue(PROTECTION_OK);
    mockedApi.listRepositories.mockResolvedValue(REPOS);
    // 默认页面可见
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => 'visible',
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('加载中显示顶部进度条，加载完成后展示内容', async () => {
    // 用假定时器接管进度条内部的伪进度 tick，避免其 setTimeout 在断言后逸出 act
    vi.useFakeTimers();
    // 把首批端点挂起，制造可观测的加载态
    let resolveSummary: (v: DashboardSummary) => void = () => {};
    mockedApi.getDashboardSummary.mockReturnValue(
      new Promise<DashboardSummary>((res) => {
        resolveSummary = res;
      }),
    );

    renderPage();

    // 加载态：顶部进度条存在（自研组件 aria-label）
    expect(screen.getByLabelText('页面加载进度')).toBeInTheDocument();
    // 加载态：KPI 内容尚未出现
    expect(screen.queryByText('仓库数')).not.toBeInTheDocument();

    resolveSummary(SUMMARY);

    // 加载完成后内容淡入出现
    await vi.waitFor(() => expect(screen.getByText('仓库数')).toBeInTheDocument());
    expect(screen.getByText('主机健康')).toBeInTheDocument();
  });

  it('主机健康每 5 秒轮询刷新 getHostMonitor', async () => {
    vi.useFakeTimers();
    renderPage();

    // 等首批取数（含首帧 getHostMonitor）落地
    await vi.waitFor(() => expect(mockedApi.getHostMonitor).toHaveBeenCalledTimes(1));

    // 推进 5 秒 → 轮询补一帧
    await vi.advanceTimersByTimeAsync(5000);
    expect(mockedApi.getHostMonitor).toHaveBeenCalledTimes(2);

    // 再推进 5 秒 → 再补一帧
    await vi.advanceTimersByTimeAsync(5000);
    expect(mockedApi.getHostMonitor).toHaveBeenCalledTimes(3);
  });

  it('页面不可见时暂停轮询，回到前台立即补一帧并恢复', async () => {
    vi.useFakeTimers();
    renderPage();
    await vi.waitFor(() => expect(mockedApi.getHostMonitor).toHaveBeenCalledTimes(1));

    // 切到后台
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => 'hidden',
    });
    document.dispatchEvent(new Event('visibilitychange'));

    // 后台期间不再轮询
    await vi.advanceTimersByTimeAsync(10000);
    expect(mockedApi.getHostMonitor).toHaveBeenCalledTimes(1);

    // 回到前台：立即补一帧并恢复周期
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => 'visible',
    });
    document.dispatchEvent(new Event('visibilitychange'));
    await vi.waitFor(() => expect(mockedApi.getHostMonitor).toHaveBeenCalledTimes(2));

    await vi.advanceTimersByTimeAsync(5000);
    expect(mockedApi.getHostMonitor).toHaveBeenCalledTimes(3);
  });

  it('组件卸载时清理轮询定时器', async () => {
    vi.useFakeTimers();
    const { unmount } = renderPage();
    await vi.waitFor(() => expect(mockedApi.getHostMonitor).toHaveBeenCalledTimes(1));

    unmount();
    await vi.advanceTimersByTimeAsync(15000);
    // 卸载后不再轮询
    expect(mockedApi.getHostMonitor).toHaveBeenCalledTimes(1);
  });
});
