// 应用路由与守卫。
//
// 路由设计约束：后端格式 API 占用了 catch-all `/{repo}/{*path}`（两段及以上），
// 故前端可深链的路由一律为单段路径（/login、/repositories、/users、/tokens、/search），
// 详情视图用查询参数承载（如 /artifacts?repo=..&path=..），
// 确保任意前端 URL 都落到后端 SPA 回退而不被格式路由拦截。

import { Routes, Route, Navigate, useLocation } from 'react-router-dom';
import { useEffect, useRef } from 'react';
import { Center, Loader } from '@mantine/core';
import { useAuth } from './auth/useAuth';
import { useGlobalProgress } from './hooks/useGlobalProgress';
import { AppLayout } from './components/AppLayout';
import { LoginPage } from './pages/LoginPage';
import { DashboardPage } from './pages/DashboardPage';
import { RepositoriesPage } from './pages/RepositoriesPage';
import { RepositoryDetailPage } from './pages/RepositoryDetailPage';
import { UsersPage } from './pages/UsersPage';
import { GroupsPage } from './pages/GroupsPage';
import { TokensPage } from './pages/TokensPage';
import { SearchPage } from './pages/SearchPage';
import { ArtifactDetailPage } from './pages/ArtifactDetailPage';
import { UploadPage } from './pages/UploadPage';
import { MonitorPage } from './pages/MonitorPage';
import { AnalyticsPage } from './pages/AnalyticsPage';
import { AuditPage } from './pages/AuditPage';
import { SystemLogsPage } from './pages/SystemLogsPage';
import { ProtectionMonitorPage } from './pages/ProtectionMonitorPage';
import { MigrationPage } from './pages/MigrationPage';
import { SettingsPage } from './pages/SettingsPage';
import { SystemPage } from './pages/SystemPage';
import { LicensesPage } from './pages/LicensesPage';

/** 登录守卫：未登录跳登录页（带回跳路径）；恢复会话期间显示加载态。 */
function RequireAuth({ children }: { children: React.ReactNode }) {
  const { user, loading } = useAuth();
  const location = useLocation();
  if (loading) {
    return (
      <Center h="100vh">
        <Loader />
      </Center>
    );
  }
  if (!user) {
    return <Navigate to="/login" state={{ from: location.pathname + location.search }} replace />;
  }
  return <>{children}</>;
}

/** 管理员守卫：非管理员重定向到落地路由。 */
function RequireAdmin({ children }: { children: React.ReactNode }) {
  const { isAdmin } = useAuth();
  if (!isAdmin) {
    return <Navigate to="/" replace />;
  }
  return <>{children}</>;
}

/**
 * 路由切换进度触发器（FR-127）：监听 location 变化，每次路由切换短暂置全局进度为 true，
 * 再在下一个宏任务帧置 false（给「瞬切」页面也带来进度反馈感）。
 * 使用 useRef 记上一次 pathname，避免同路径无效触发（如搜索 q 参数变化不算切页）。
 */
function RouteProgressTrigger() {
  const location = useLocation();
  const { setRouteLoading } = useGlobalProgress();
  const prevPathRef = useRef(location.pathname);

  useEffect(() => {
    if (prevPathRef.current === location.pathname) return;
    prevPathRef.current = location.pathname;
    setRouteLoading(true);
    const id = setTimeout(() => setRouteLoading(false), 400);
    return () => clearTimeout(id);
  }, [location.pathname, setRouteLoading]);

  return null;
}

/**
 * 落地路由分流（FR-95）：登录用户看仪表盘，匿名访客重定向到公开浏览。
 * 仪表盘读取当前用户信息、不能匿名渲染，故匿名落地到只读的公开仓库浏览。
 * 恢复会话期间显示加载态，避免据未恢复的空登录态误判。
 */
function HomeRoute() {
  const { user, loading } = useAuth();
  if (loading) {
    return (
      <Center h="100vh">
        <Loader />
      </Center>
    );
  }
  return user ? <DashboardPage /> : <Navigate to="/repositories" replace />;
}

/**
 * 应用路由表（三层守卫，FR-95）。
 *
 * 外壳 `AppLayout` 挂在 `/` 且**不包 RequireAuth**——匿名访客也能进入外壳浏览公开内容；
 * 由 `AppLayout` 自身据登录态渲染匿名 / 登录页眉与导航。各子路由按层加守卫：
 * - 公开层（匿名可达，只读公开内容）：`/`（落地分流）、`/repositories`、`/repository`、`/search`、`/artifact`、`/licenses`。
 * - 需登录层（user+）：`/tokens`、`/upload`——`RequireAuth`。
 * - 需管理员层（admin）：`/users`、`/groups`、`/monitor`、`/analytics`、`/audit`、`/system-logs`、`/protection-monitor`、`/migration`、`/settings`、`/system`——`RequireAuth` + `RequireAdmin`。
 * - `/protection` 重定向到 `/settings`（防护配置已并入设置页，FR-110）。
 */
export function App() {
  return (
    <Routes>
      {/* 路由切换进度触发器（FR-127）：监听 pathname 变化，短暂触发全局进度条。 */}
      <Route path="*" element={<RouteProgressTrigger />} />
      <Route path="/login" element={<LoginPage />} />
      <Route path="/" element={<AppLayout />}>
        {/* 公开层：匿名可达，只读浏览 / 搜索公开制品 */}
        <Route index element={<HomeRoute />} />
        <Route path="repositories" element={<RepositoriesPage />} />
        <Route path="repository" element={<RepositoryDetailPage />} />
        <Route path="artifact" element={<ArtifactDetailPage />} />
        <Route path="search" element={<SearchPage />} />
        {/* 开源许可页（FR-102）：公开、匿名可直达（导航入口属 FR-101，本 FR 仅保证路由可达） */}
        <Route path="licenses" element={<LicensesPage />} />

        {/* 需登录层：匿名跳登录（带回跳） */}
        <Route
          path="tokens"
          element={
            <RequireAuth>
              <TokensPage />
            </RequireAuth>
          }
        />
        <Route
          path="upload"
          element={
            <RequireAuth>
              <UploadPage />
            </RequireAuth>
          }
        />
        {/* 需管理员层：匿名跳登录、普通用户重定向到落地 */}
        <Route
          path="users"
          element={
            <RequireAuth>
              <RequireAdmin>
                <UsersPage />
              </RequireAdmin>
            </RequireAuth>
          }
        />
        <Route
          path="groups"
          element={
            <RequireAuth>
              <RequireAdmin>
                <GroupsPage />
              </RequireAdmin>
            </RequireAuth>
          }
        />
        {/* FR-110：防护配置已并入「设置」页，独立页 /protection 重定向到设置页（保留旧链接可达） */}
        <Route path="protection" element={<Navigate to="/settings" replace />} />
        <Route
          path="monitor"
          element={
            <RequireAuth>
              <RequireAdmin>
                <MonitorPage />
              </RequireAdmin>
            </RequireAuth>
          }
        />
        {/* FR-99：使用分析 / 审计 / 防护监控恢复为各自独立路由（监控页不再 tab 化整合） */}
        <Route
          path="analytics"
          element={
            <RequireAuth>
              <RequireAdmin>
                <AnalyticsPage />
              </RequireAdmin>
            </RequireAuth>
          }
        />
        <Route
          path="audit"
          element={
            <RequireAuth>
              <RequireAdmin>
                <AuditPage />
              </RequireAdmin>
            </RequireAuth>
          }
        />
        {/* 系统日志（FR-107，仅 Admin）：运行时技术日志，导航入口由 FR-92 外壳添加 */}
        <Route
          path="system-logs"
          element={
            <RequireAuth>
              <RequireAdmin>
                <SystemLogsPage />
              </RequireAdmin>
            </RequireAuth>
          }
        />
        <Route
          path="protection-monitor"
          element={
            <RequireAuth>
              <RequireAdmin>
                <ProtectionMonitorPage />
              </RequireAdmin>
            </RequireAuth>
          }
        />
        <Route
          path="migration"
          element={
            <RequireAuth>
              <RequireAdmin>
                <MigrationPage />
              </RequireAdmin>
            </RequireAuth>
          }
        />
        <Route
          path="settings"
          element={
            <RequireAuth>
              <RequireAdmin>
                <SettingsPage />
              </RequireAdmin>
            </RequireAuth>
          }
        />
        {/* 系统管理页（FR-109，仅 Admin）：在线更新 + 重启 / 关闭 */}
        <Route
          path="system"
          element={
            <RequireAuth>
              <RequireAdmin>
                <SystemPage />
              </RequireAdmin>
            </RequireAuth>
          }
        />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
