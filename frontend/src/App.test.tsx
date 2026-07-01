// 三层路由守卫测试（FR-95 角色感知 UI）：
// 公开层（匿名可达，只读）/ 需登录层（user+）/ 需管理员层（admin）。
// 验证匿名、普通用户、管理员三种身份在各层路由上的可达性与重定向行为。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { MemoryRouter } from 'react-router-dom';
import { App } from './App';
import { AuthContext, type AuthContextValue } from './auth/AuthContext';

// 桩掉端点模块：各页面挂载时会调用 api.*，统一返回空，避免真实网络
vi.mock('./api/endpoints');
// 桩掉通知，避免依赖 Notifications Provider
vi.mock('./lib/notify', () => ({
  notifySuccess: vi.fn(),
  notifyError: vi.fn(),
}));

import * as api from './api/endpoints';
const mockedApi = vi.mocked(api);

/** 匿名上下文：未登录、会话已恢复完毕（loading=false）。 */
function 匿名上下文(): AuthContextValue {
  return {
    user: null,
    loading: false,
    isAdmin: false,
    signIn: async () => {},
    signOut: async () => {},
  };
}

/** 普通用户上下文。 */
function 普通用户上下文(): AuthContextValue {
  return {
    user: { id: 'u-user', username: 'alice', role: 'user' },
    loading: false,
    isAdmin: false,
    signIn: async () => {},
    signOut: async () => {},
  };
}

/** 管理员上下文。 */
function 管理员上下文(): AuthContextValue {
  return {
    user: { id: 'u-admin', username: 'admin', role: 'admin' },
    loading: false,
    isAdmin: true,
    signIn: async () => {},
    signOut: async () => {},
  };
}

/** 在指定初始路由与认证上下文下渲染整个应用路由表。 */
function renderApp(initialPath: string, ctx: AuthContextValue) {
  return render(
    <MantineProvider>
      <AuthContext.Provider value={ctx}>
        <MemoryRouter initialEntries={[initialPath]}>
          <App />
        </MemoryRouter>
      </AuthContext.Provider>
    </MantineProvider>,
  );
}

describe('App 三层路由守卫（FR-95）', () => {
  beforeEach(() => {
    // 列表 / 搜索类端点统一返回空，页面据此渲染空态而非报错
    mockedApi.listRepositories.mockResolvedValue([]);
    mockedApi.listTokens.mockResolvedValue([]);
    mockedApi.listUsers.mockResolvedValue([]);
    mockedApi.search.mockResolvedValue({
      items: [],
      total: 0,
      offset: 0,
      limit: 20,
      has_more: false,
    });
    // 开源许可页（FR-102）匿名可达，端点返回未生成空清单即可渲染
    mockedApi.getLicenses.mockResolvedValue({
      generated: false,
      entries: [],
      summary: { total: 0, runtime: 0, dev: 0, licenses: 0 },
    });
    // 外壳版本展示（FR-101）：底部版本号取自 /health；更新检查留存默认为空（FR-126 只读留存）不显徽标
    mockedApi.getHealth.mockResolvedValue({ status: 'ok', version: '0.4.0', port: 9999 });
    mockedApi.getCachedCheck.mockResolvedValue({ result: null, checked_at: null });
    mockedApi.listUpdateJobs.mockResolvedValue([]);
    mockedApi.listTasks.mockResolvedValue([]);
    // 管理员仪表盘（FR-108）落地 / 时各数据源端点：统一返回最简空 / 默认值，仅验证落地可达
    mockedApi.getDashboardSummary.mockResolvedValue({
      repo_count: 0,
      artifact_count: 0,
      total_bytes: 0,
      user_count: 0,
    });
    mockedApi.getHostMonitor.mockResolvedValue({
      cpu: { usage_percent: 0, logical_cores: 1 },
      memory: { total_bytes: 1, used_bytes: 0, swap_total_bytes: 0, swap_used_bytes: 0 },
      disk: { total_bytes: 1, available_bytes: 1, disks: [] },
      uptime_secs: 0,
    });
    mockedApi.listAudit.mockResolvedValue({
      items: [],
      total: 0,
      offset: 0,
      limit: 8,
      has_more: false,
    });
    mockedApi.getDynamicConfig.mockResolvedValue({
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
        enabled: false,
        source_base_url: '',
        ecosystems: [],
        refresh_interval_secs: 3600,
        download_timeout_secs: 60,
      },
      auth: { session_ttl_secs: 3600, login_max_failures: 5, login_lockout_secs: 900 },
    });
    mockedApi.protectionStatus.mockResolvedValue({
      alerts_enabled: false,
      window_secs: 60,
      window_counts: [],
      active_banned_ips: 0,
      dropped_alerts: 0,
      recent_alerts: [],
    });
  });
  afterEach(() => vi.clearAllMocks());

  describe('匿名访客', () => {
    it('访问公开路由 /repositories：不跳登录，渲染仓库浏览（标题可见）', async () => {
      renderApp('/repositories', 匿名上下文());
      // 公开浏览页标题；不应出现登录页的「登录 JianArtifact」
      expect(await screen.findByText('仓库管理')).toBeInTheDocument();
      expect(screen.queryByText('登录 JianArtifact')).not.toBeInTheDocument();
    });

    it('访问公开路由 /search：不跳登录，渲染搜索页', async () => {
      renderApp('/search', 匿名上下文());
      expect(await screen.findByText('制品搜索')).toBeInTheDocument();
      expect(screen.queryByText('登录 JianArtifact')).not.toBeInTheDocument();
    });

    it('落地路由 / 重定向到公开浏览 /repositories', async () => {
      renderApp('/', 匿名上下文());
      expect(await screen.findByText('仓库管理')).toBeInTheDocument();
    });

    it('访问公开路由 /licenses：不跳登录，渲染开源许可页', async () => {
      renderApp('/licenses', 匿名上下文());
      expect(await screen.findByText('开源许可')).toBeInTheDocument();
      expect(screen.queryByText('登录 JianArtifact')).not.toBeInTheDocument();
    });

    it('访问需登录路由 /tokens：重定向到登录页', async () => {
      renderApp('/tokens', 匿名上下文());
      expect(await screen.findByText('登录 JianArtifact')).toBeInTheDocument();
    });

    it('访问需登录路由 /upload：重定向到登录页', async () => {
      renderApp('/upload', 匿名上下文());
      expect(await screen.findByText('登录 JianArtifact')).toBeInTheDocument();
    });

    it('访问管理路由 /users：重定向到登录页', async () => {
      renderApp('/users', 匿名上下文());
      expect(await screen.findByText('登录 JianArtifact')).toBeInTheDocument();
    });

    it('访问管理路由 /settings：重定向到登录页', async () => {
      renderApp('/settings', 匿名上下文());
      expect(await screen.findByText('登录 JianArtifact')).toBeInTheDocument();
    });
  });

  describe('普通用户', () => {
    it('访问公开路由 /repositories：可达', async () => {
      renderApp('/repositories', 普通用户上下文());
      expect(await screen.findByText('仓库管理')).toBeInTheDocument();
    });

    it('访问需登录路由 /tokens：可达（自助管理自己的 Token）', async () => {
      renderApp('/tokens', 普通用户上下文());
      expect(await screen.findByText('Token 管理')).toBeInTheDocument();
    });

    it('访问管理路由 /users：重定向到落地（不可达，不渲染用户管理）', async () => {
      renderApp('/users', 普通用户上下文());
      // 被 RequireAdmin 重定向到 /，登录用户落地到仪表盘；不应渲染用户管理
      expect(await screen.findByText('仪表盘')).toBeInTheDocument();
      expect(mockedApi.listUsers).not.toHaveBeenCalled();
    });

    it('访问管理路由 /settings：重定向到落地（不可达）', async () => {
      renderApp('/settings', 普通用户上下文());
      expect(await screen.findByText('仪表盘')).toBeInTheDocument();
    });
  });

  describe('管理员', () => {
    it('落地路由 / 渲染仪表盘', async () => {
      renderApp('/', 管理员上下文());
      expect(await screen.findByText('仪表盘')).toBeInTheDocument();
    });

    it('访问管理路由 /users：可达', async () => {
      renderApp('/users', 管理员上下文());
      expect(await screen.findByText('用户管理')).toBeInTheDocument();
    });

    it('访问需登录路由 /tokens：可达', async () => {
      renderApp('/tokens', 管理员上下文());
      expect(await screen.findByText('Token 管理')).toBeInTheDocument();
    });
  });
});
