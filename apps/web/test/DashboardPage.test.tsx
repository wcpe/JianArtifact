// 业务仪表盘（真实读模型，v0.8.0）：数据经 devmock 的 observability/dashboard 接口提供，
// 验证真实 KPI 渲染、上游自动阻止去重面板与时间范围切换。
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpResponse, http } from "msw";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useLocation } from "react-router-dom";

import { DashboardPage } from "../src/pages/DashboardPage";
import { TrendChart } from "../src/components/observability/TrendChart";
import { renderWithProviders } from "./harness";
import { server } from "@jianartifact/devmock/node";

afterEach(() => vi.restoreAllMocks());

function LocationProbe() {
  const location = useLocation();
  return <span data-testid="location-probe">{`${location.pathname}${location.search}`}</span>;
}

describe("业务仪表盘（真实读模型）", () => {
  it("使用服务端 KPI，不从趋势数据重新汇总", async () => {
    server.use(
      http.get("*/api/v1/observability/dashboard", () =>
        HttpResponse.json({
          from: "2026-08-30T00:00:00.000Z",
          to: "2026-08-31T00:00:00.000Z",
          effectiveBucket: "minute",
          current: {
            from: "2026-08-31T00:00:00.000Z",
            to: "2026-08-31T01:00:00.000Z",
            repositoryCount: 1,
            assetCount: 2,
            logicalBytes: 3,
          },
          kpi: {
            repositoryCount: 7,
            assetCount: 8,
            logicalBytes: 9,
            requestCount: 10,
            downloadCount: 11,
            failureCount: 12,
            cacheHitRate: 0.25,
          },
          requestTrend: [
            {
              from: "2026-08-30T00:00:00.000Z",
              to: "2026-08-30T00:01:00.000Z",
              requestCount: 999,
              downloadCount: 999,
              failureCount: 999,
              cacheHitCount: 999,
              cacheMissCount: 0,
            },
          ],
          capacityTrend: [],
          alerts: [],
        }),
      ),
    );
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    // KPI 值限定在核心指标带内断言（趋势卡的区间剖析条可能统计出相同数字，属预期）
    const kpiBand = await screen.findByLabelText("核心指标带");
    expect(within(kpiBand).getByText("10")).toBeTruthy();
    expect(within(kpiBand).getByText("11")).toBeTruthy();
    expect(within(kpiBand).getByText("12")).toBeTruthy();
    expect(within(kpiBand).getByText("25.0%")).toBeTruthy();
    expect(within(kpiBand).queryByText("999")).toBeNull();
  });

  it("悬停趋势图同时展示时间与两个系列的数值", () => {
    renderWithProviders(
      <TrendChart
        title="双序列趋势"
        summary="同单位比较"
        primary={[
          { label: "10:00", value: 10 },
          { label: "10:05", value: 20 },
        ]}
        secondary={[
          { label: "10:00", value: 4 },
          { label: "10:05", value: 8 },
        ]}
        primaryLabel="请求"
        secondaryLabel="下载"
      />,
    );

    fireEvent.mouseMove(screen.getByRole("img", { name: "双序列趋势：同单位比较" }), {
      clientX: 100,
    });

    expect(screen.getByText("10:05 · 请求：20 · 下载：8")).toBeTruthy();
  });

  it("拖选聚焦子时段：胶囊出现、剖析条跟随、双击还原", () => {
    renderWithProviders(
      <TrendChart
        title="拖选聚焦趋势"
        summary="拖选聚焦"
        primary={[
          { label: "10:00", value: 10 },
          { label: "10:05", value: 20 },
          { label: "10:10", value: 90 },
        ]}
        primaryLabel="请求"
      />,
    );

    const chart = screen.getByRole("img", { name: "拖选聚焦趋势：拖选聚焦" });
    // jsdom 无布局：mock rect 让 clientX → 索引比例可控（宽 100，3 点）。
    vi.spyOn(chart, "getBoundingClientRect").mockReturnValue({
      left: 0,
      width: 100,
      height: 200,
      top: 0,
      right: 100,
      bottom: 200,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    } as DOMRect);
    fireEvent.mouseDown(chart, { clientX: 5 }); // index 0
    fireEvent.mouseMove(chart, { clientX: 60 }); // index 1
    fireEvent.mouseUp(chart, { clientX: 60 });

    expect(screen.getByText("已聚焦 10:00 – 10:05")).toBeTruthy();
    // 剖析条切换为聚焦段统计（10:00–10:05 两点：均值 15 / 峰值 20 / 谷值 10）
    expect(screen.getByText("15")).toBeTruthy();
    expect(screen.getByText("20")).toBeTruthy();

    fireEvent.doubleClick(chart);
    expect(screen.queryByText(/已聚焦/)).toBeNull();
  });

  it("始终保留趋势悬停信息区域，避免鼠标移入时顶开后续内容", () => {
    renderWithProviders(
      <TrendChart
        title="稳定布局趋势"
        summary="悬停不改变图表卡高度"
        primary={[{ label: "10:00", value: 10 }]}
        primaryLabel="请求"
      />,
    );

    const tooltip = screen.getByTestId("trend-hover-summary");
    expect(tooltip.style.minHeight).not.toBe("");

    fireEvent.mouseMove(screen.getByRole("img", { name: "稳定布局趋势：悬停不改变图表卡高度" }), {
      clientX: 100,
    });

    expect(screen.getByTestId("trend-hover-summary")).toBe(tooltip);
    expect(screen.getByText("10:00 · 请求：10")).toBeTruthy();
  });

  it("展示真实业务 KPI 与趋势，不混入主机指标", async () => {
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    await screen.findByText("请求与下载趋势", undefined, { timeout: 5000 });
    // 页面定位（概览 / 业务仪表盘）由全局页眉面包屑（AppLayout）表达，页内不再渲染标题。
    // devmock 种子：9 仓库 / 28416 制品 / 128 请求 / 92 下载 / 1 失败 / 命中率 78/(78+6)
    // KPI 值限定在核心指标带内断言（趋势卡区间剖析条可能统计出相同数字，属预期）
    const kpiBand = screen.getByLabelText("核心指标带");
    expect(within(kpiBand).getByText("仓库数")).toBeTruthy();
    expect(within(kpiBand).getByText("9")).toBeTruthy();
    expect(within(kpiBand).getByText("28,416")).toBeTruthy();
    expect(within(kpiBand).getByText("128")).toBeTruthy();
    expect(within(kpiBand).getByText("92")).toBeTruthy();
    expect(within(kpiBand).getByText("1")).toBeTruthy();
    expect(within(kpiBand).getByText("92.9%")).toBeTruthy();
    // 不混入主机指标
    expect(screen.queryByText(/CPU|内存|主机磁盘/)).toBeNull();
  });

  it("仓库状态面板展示各仓库连接状态分布", async () => {
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    expect(await screen.findByText("仓库状态")).toBeTruthy();
    // devmock 种子：9 个仓库（KPI 与列表一致）；4 可用 / 4 自动阻止 / 1 不可用
    expect(screen.getByText("npm-proxy")).toBeTruthy();
    expect(screen.getByText("maven-central")).toBeTruthy();
    expect(screen.getByText("docker-hub")).toBeTruthy();
    expect(screen.getByText("pypi-mirror")).toBeTruthy();
    expect(screen.getAllByText("自动阻止").length).toBeGreaterThanOrEqual(4);
    expect(screen.getAllByText("不可用").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("共 9 个仓库")).toBeTruthy();
  });

  it("切换到 7 天范围时同步更新选择器文本", async () => {
    const user = userEvent.setup();
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    // 打开时间范围选择器（触发器文本即当前范围）
    await user.click(await screen.findByRole("button", { name: "近 24 小时" }));
    // 在弹层内选择“近 7 天”
    await user.click(await screen.findByRole("button", { name: "近 7 天" }));

    // 弹层收起后，只剩触发器显示新范围（近 7 天）
    await waitFor(() => expect(screen.getAllByRole("button", { name: "近 7 天" })).toHaveLength(1));
  });

  it("仅以 60 秒间隔注册静默刷新", async () => {
    const setIntervalSpy = vi.spyOn(window, "setInterval");
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    await screen.findByText("请求与下载趋势");

    expect(setIntervalSpy).toHaveBeenCalledWith(expect.any(Function), 60_000);
  });

  it("从关注项跳转时使用审计页识别的 attentionId 参数", async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <>
        <DashboardPage />
        <LocationProbe />
      </>,
      { route: "/dashboard", authenticated: true },
    );

    const attentionButtons = await screen.findAllByRole("button", { name: "auth.login_rejected" });
    await user.click(attentionButtons[0]!);
    expect(screen.getByTestId("location-probe").textContent).toBe(
      "/audit-logs?attentionId=attention-security-login",
    );
  });

  it("需要处理面板不展示已全部确认的风险批次", async () => {
    server.use(
      http.get("*/api/v1/observability/audit/attentions", () =>
        HttpResponse.json({
          snapshot: "dashboard-attention-snapshot",
          totalCount: 2,
          items: [
            {
              attentionId: "attention-acknowledged",
              category: "management_change",
              severity: "critical",
              firstOccurredAt: "2026-09-06T10:00:00Z",
              latestOccurredAt: "2026-09-06T10:00:00Z",
              result: "success",
              action: "repo.delete",
              target: { kind: "repository", label: "已确认仓库" },
              summary: "已确认",
              successCount: 1,
              failureCount: 0,
              affectedCount: 1,
              state: "acknowledged",
              unacknowledgedRiskEventCount: 0,
            },
            {
              attentionId: "attention-active",
              category: "asset_change",
              severity: "high",
              firstOccurredAt: "2026-09-06T10:01:00Z",
              latestOccurredAt: "2026-09-06T10:01:00Z",
              result: "failure",
              action: "asset.delete",
              target: { kind: "artifact", label: "待确认制品" },
              summary: "待确认",
              successCount: 0,
              failureCount: 1,
              affectedCount: 1,
              state: "unacknowledged",
              unacknowledgedRiskEventCount: 1,
            },
          ],
        }),
      ),
    );
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    const heading = await screen.findByText("需要处理");
    const panel = heading.closest(".mantine-Card-root");
    expect(panel).not.toBeNull();
    expect(within(panel as HTMLElement).getByRole("button", { name: "asset.delete" })).toBeTruthy();
    expect(within(panel as HTMLElement).queryByRole("button", { name: "repo.delete" })).toBeNull();
  });
});
