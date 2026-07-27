// 路由表：自举为公开路由，其余业务页由 AppLayout 包裹。
// FR-67：删除整页登录，未登录访问受保护页弹登录模态框（取消回仓库列表）；
// /login 与 catch-all 统一落 /repositories（不分登录态）。
import { Navigate, Route, Routes, useNavigate, useParams } from "react-router-dom";
import { useEffect } from "react";

import { AppLayout } from "./AppLayout";
import { AclPage } from "../pages/AclPage";
import { DashboardPage } from "../pages/DashboardPage";
import { LicensesPage } from "../pages/LicensesPage";
import { MigrationDetailPage } from "../pages/MigrationDetailPage";
import { MigrationsPage } from "../pages/MigrationsPage";
import { MigrationWizardPage } from "../pages/MigrationWizardPage";
import { RepositoriesPage } from "../pages/RepositoriesPage";
import { RepositoryDetailPage } from "../pages/RepositoryDetailPage";
import { SearchPage } from "../pages/SearchPage";
import { SetupPage } from "../pages/SetupPage";
import { TokensPage } from "../pages/TokensPage";
import { UsersPage } from "../pages/UsersPage";
import { useAuth } from "../auth/AuthContext";
import { useLoginModal } from "../auth/LoginModal";
import type { ReactNode } from "react";

/** 鉴权守卫（FR-67）：未登录弹登录模态框（不整页跳转），取消则回落仓库列表。 */
function RequireAuth({ children }: { children: ReactNode }) {
  const { isAuthenticated } = useAuth();
  const { openLogin } = useLoginModal();
  const navigate = useNavigate();

  useEffect(() => {
    if (!isAuthenticated) {
      openLogin({ onCancel: () => navigate("/repositories", { replace: true }) });
    }
  }, [isAuthenticated, openLogin, navigate]);

  if (!isAuthenticated) {
    return null;
  }
  return <>{children}</>;
}

/** catch-all 与 / 统一落仓库列表（FR-67：不分登录态）。 */
function CatchAllRedirect() {
  return <Navigate to="/repositories" replace />;
}

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/setup" element={<SetupPage />} />
      {/* FR-67：整页登录已删除，/login 兼容重定向到仓库列表 */}
      <Route path="/login" element={<Navigate to="/repositories" replace />} />
      {/* /p/:name 公开链接别名：重定向到内嵌布局的仓库详情页 */}
      <Route path="/p/:name" element={<PublicRedirect />} />
      {/* 所有业务路由在 AppLayout 内（含未登录的公开仓库浏览） */}
      <Route element={<AppLayout />}>
        {/* 公开路由（未登录可访问；FR-67 匿名可直接看仓库列表） */}
        <Route path="/repositories" element={<RepositoriesPage />} />
        <Route path="/repositories/:name" element={<RepositoryDetailPage />} />
        <Route path="/search" element={<SearchPage />} />
        {/* FR-72：开源协议页（公开，匿名可访问） */}
        <Route path="/licenses" element={<LicensesPage />} />
        {/* 受保护路由 */}
        <Route
          path="/dashboard"
          element={
            <RequireAuth>
              <DashboardPage />
            </RequireAuth>
          }
        />
        <Route
          path="/users"
          element={
            <RequireAuth>
              <UsersPage />
            </RequireAuth>
          }
        />
        <Route
          path="/tokens"
          element={
            <RequireAuth>
              <TokensPage />
            </RequireAuth>
          }
        />
        <Route
          path="/repositories/:name/acl"
          element={
            <RequireAuth>
              <AclPage />
            </RequireAuth>
          }
        />
        <Route
          path="/migrations"
          element={
            <RequireAuth>
              <MigrationsPage />
            </RequireAuth>
          }
        />
        <Route
          path="/migrations/new"
          element={
            <RequireAuth>
              <MigrationWizardPage />
            </RequireAuth>
          }
        />
        <Route
          path="/migrations/:id"
          element={
            <RequireAuth>
              <MigrationDetailPage />
            </RequireAuth>
          }
        />
      </Route>
      <Route path="*" element={<CatchAllRedirect />} />
    </Routes>
  );
}

/** /p/:name 别名重定向：将旧的公开链接导向内嵌布局的仓库详情页。 */
function PublicRedirect() {
  const { name = "" } = useParams();
  return <Navigate to={`/repositories/${name}`} replace />;
}
