// 路由表：登录/自举/公开浏览为公开路由，其余业务页由 AppLayout 包裹并受鉴权守卫保护。
import { Navigate, Route, Routes } from "react-router-dom";
import type { ReactNode } from "react";

import { useAuth } from "../auth/AuthContext";
import { AppLayout } from "./AppLayout";
import { AclPage } from "../pages/AclPage";
import { DashboardPage } from "../pages/DashboardPage";
import { LoginPage } from "../pages/LoginPage";
import { MigrationDetailPage } from "../pages/MigrationDetailPage";
import { MigrationsPage } from "../pages/MigrationsPage";
import { MigrationWizardPage } from "../pages/MigrationWizardPage";
import { PublicRepoPage } from "../pages/PublicRepoPage";
import { RepositoriesPage } from "../pages/RepositoriesPage";
import { RepositoryDetailPage } from "../pages/RepositoryDetailPage";
import { SetupPage } from "../pages/SetupPage";
import { TokensPage } from "../pages/TokensPage";
import { UsersPage } from "../pages/UsersPage";

/** 鉴权守卫：未登录跳转登录页。 */
function RequireAuth({ children }: { children: ReactNode }) {
  const { isAuthenticated } = useAuth();
  if (!isAuthenticated) {
    return <Navigate to="/login" replace />;
  }
  return <>{children}</>;
}

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/setup" element={<SetupPage />} />
      <Route path="/login" element={<LoginPage />} />
      {/* 公开仓库浏览：无需登录 */}
      <Route path="/p/:name" element={<PublicRepoPage />} />
      <Route
        element={
          <RequireAuth>
            <AppLayout />
          </RequireAuth>
        }
      >
        <Route path="/dashboard" element={<DashboardPage />} />
        <Route path="/users" element={<UsersPage />} />
        <Route path="/tokens" element={<TokensPage />} />
        <Route path="/repositories" element={<RepositoriesPage />} />
        <Route path="/repositories/:name" element={<RepositoryDetailPage />} />
        <Route path="/repositories/:name/acl" element={<AclPage />} />
        <Route path="/migrations" element={<MigrationsPage />} />
        <Route path="/migrations/new" element={<MigrationWizardPage />} />
        <Route path="/migrations/:id" element={<MigrationDetailPage />} />
      </Route>
      <Route path="*" element={<Navigate to="/dashboard" replace />} />
    </Routes>
  );
}
