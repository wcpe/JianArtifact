// 导航预取：hover / 聚焦导航项时提前把目标页面的 chunk 拉下来。
//
// 懒加载的等待窗口正好是「切换页面时看到骨架」的那段时间；预取能把它压到几乎不可感知。
// 独立成模块是为了避开循环依赖——router.tsx 要 import AppLayout，AppLayout 又要用预取，
// 放在 router 里会形成 router ⇄ AppLayout 环。
//
// 预取失败静默忽略：真正的加载失败仍由路由的 React.lazy 兜底并报错，预取只是加速手段。
const ROUTE_PRELOADERS: Record<string, () => Promise<unknown>> = {
  "/dashboard": () => import("../pages/DashboardPage"),
  "/repositories": () => import("../pages/RepositoriesPage"),
  "/audit-logs": () => import("../pages/AuditLogPage"),
  "/host-monitoring": () => import("../pages/HostMonitoringPage"),
  "/users": () => import("../pages/UsersPage"),
  "/tokens": () => import("../pages/TokensPage"),
  "/migrations": () => import("../pages/MigrationsPage"),
  "/settings": () => import("../pages/SettingsPage"),
  "/licenses": () => import("../pages/LicensesPage"),
  "/search": () => import("../pages/SearchPage"),
};

/** 预取某个导航路径对应的页面 chunk；无对应页面时不做任何事。 */
export function preloadRoute(path: string): void {
  // 仓库详情是动态路径，按前缀匹配（匿名侧栏的公开仓库项也会走到这里）。
  if (path.startsWith("/repositories/")) {
    void import("../pages/RepositoryDetailPage").catch(() => {});
    return;
  }
  const preload = ROUTE_PRELOADERS[path];
  if (preload) void preload().catch(() => {});
}
