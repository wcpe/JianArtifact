// 路由表：自举为公开路由，其余业务页由 AppLayout 包裹。
// FR-67：删除整页登录，未登录访问受保护页弹登录模态框（取消回仓库列表）；
// /login 与 catch-all 统一落 /repositories（不分登录态）。
// FR-70：页面组件全部 React.lazy 按路由分割 chunk；AppLayout 保持静态，
// 页内懒加载挂到 AppLayout 内的 Suspense（布局不闪），/setup 等布局外路由由此处兜底。
import { Navigate, Route, Routes, useNavigate, useParams } from "react-router-dom";
import { Suspense, lazy, useEffect } from "react";

import { AppLayout } from "./AppLayout";
import { RouteFallback } from "../components/RouteFallback";
import { useAuth } from "../auth/AuthContext";
import { useLoginModal } from "../auth/LoginModal";
import type { ReactNode } from "react";

const AclPage = lazy(() => import("../pages/AclPage").then((m) => ({ default: m.AclPage })));
const DashboardPage = lazy(() =>
  import("../pages/DashboardPage").then((m) => ({ default: m.DashboardPage })),
);
const LicensesPage = lazy(() =>
  import("../pages/LicensesPage").then((m) => ({ default: m.LicensesPage })),
);
const MigrationDetailPage = lazy(() =>
  import("../pages/MigrationDetailPage").then((m) => ({ default: m.MigrationDetailPage })),
);
const MigrationsPage = lazy(() =>
  import("../pages/MigrationsPage").then((m) => ({ default: m.MigrationsPage })),
);
const MigrationWizardPage = lazy(() =>
  import("../pages/MigrationWizardPage").then((m) => ({ default: m.MigrationWizardPage })),
);
const RepositoriesPage = lazy(() =>
  import("../pages/RepositoriesPage").then((m) => ({ default: m.RepositoriesPage })),
);
const RepositoryDetailPage = lazy(() =>
  import("../pages/RepositoryDetailPage").then((m) => ({ default: m.RepositoryDetailPage })),
);
const SearchPage = lazy(() =>
  import("../pages/SearchPage").then((m) => ({ default: m.SearchPage })),
);
const SetupPage = lazy(() => import("../pages/SetupPage").then((m) => ({ default: m.SetupPage })));
const TokensPage = lazy(() =>
  import("../pages/TokensPage").then((m) => ({ default: m.TokensPage })),
);
const UsersPage = lazy(() => import("../pages/UsersPage").then((m) => ({ default: m.UsersPage })));

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
    // 布局外路由（/setup 等）的懒加载兜底；布局内路由由 AppLayout 的 Suspense 承接
    <Suspense fallback={<RouteFallback />}>
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
          {/* 受保护路由 */}
          {/* FR-72：开源协议页收敛为登录可见（导航入口仅 admin；清单数据由 admin 端点返回） */}
          <Route
            path="/licenses"
            element={
              <RequireAuth>
                <LicensesPage />
              </RequireAuth>
            }
          />
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
    </Suspense>
  );
}

/** /p/:name 别名重定向：将旧的公开链接导向内嵌布局的仓库详情页。 */
function PublicRedirect() {
  const { name = "" } = useParams();
  return <Navigate to={`/repositories/${name}`} replace />;
}
