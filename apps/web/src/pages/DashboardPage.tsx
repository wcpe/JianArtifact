// 业务仪表盘：v0.8.0 起一律读取真实业务读模型（GetOperationsDashboard）。
// 页面定位（概览 / 业务仪表盘）由全局页眉面包屑（AppLayout）表达，页内不再重复渲染标题。
import { DashboardLive } from "../components/observability/DashboardLive";

export function DashboardPage() {
  return <DashboardLive />;
}
