// 应用路由与守卫。
//
// 路由设计约束：后端格式 API 占用了 catch-all `/{repo}/{*path}`（两段及以上），
// 故前端可深链的路由一律为单段路径（/login、/repositories、/users、/tokens、/search），
// 详情视图用查询参数承载（如 /artifacts?repo=..&path=..），
// 确保任意前端 URL 都落到后端 SPA 回退而不被格式路由拦截。

import { Routes, Route, Navigate, useLocation } from 'react-router-dom';
import { Center, Loader } from '@mantine/core';
import { useAuth } from './auth/useAuth';
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
import { AnalyticsPage } from './pages/AnalyticsPage';

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

/** 管理员守卫：非管理员重定向到仪表盘。 */
function RequireAdmin({ children }: { children: React.ReactNode }) {
  const { isAdmin } = useAuth();
  if (!isAdmin) {
    return <Navigate to="/" replace />;
  }
  return <>{children}</>;
}

/** 应用路由表。 */
export function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        path="/"
        element={
          <RequireAuth>
            <AppLayout />
          </RequireAuth>
        }
      >
        <Route index element={<DashboardPage />} />
        <Route path="repositories" element={<RepositoriesPage />} />
        <Route path="repository" element={<RepositoryDetailPage />} />
        <Route path="artifact" element={<ArtifactDetailPage />} />
        <Route path="search" element={<SearchPage />} />
        <Route path="tokens" element={<TokensPage />} />
        <Route
          path="users"
          element={
            <RequireAdmin>
              <UsersPage />
            </RequireAdmin>
          }
        />
        <Route
          path="groups"
          element={
            <RequireAdmin>
              <GroupsPage />
            </RequireAdmin>
          }
        />
        <Route
          path="analytics"
          element={
            <RequireAdmin>
              <AnalyticsPage />
            </RequireAdmin>
          }
        />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
