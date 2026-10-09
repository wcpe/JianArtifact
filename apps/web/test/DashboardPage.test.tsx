// 业务仪表盘（真实读模型，v0.8.0）：数据经 devmock 的 observability/dashboard 接口提供，
// 验证真实 KPI 渲染、上游自动阻止去重面板与时间范围切换。
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpResponse, http } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useLocation } from "react-router-dom";

import { DashboardPage } from "../src/pages/DashboardPage";
import { buildBucketAxis } from "../src/components/observability/DownloadGroupedTrend";
import { TrendChart } from "../src/components/observability/TrendChart";
import { renderWithProviders } from "./harness";
import { server } from "@jianartifact/devmock/node";

// 拦截仪表盘的 TrendChart：既**断言传给图表的 props**（容量增量口径、纵轴域），
// 又仍调用真实组件渲染（保留本文件既有悬停/拖选等交互用例）。
// mock 工厂会被 hoist 到 import 之前，故调用记录用 vi.hoisted 提前声明；不用 vi.fn，
// 以免 afterEach 的 restoreAllMocks 清掉包装实现。每个用例前清空记录。
const trendChartSpy = vi.hoisted(() => ({ calls: [] as Array<Record<string, unknown>> }));
vi.mock("../src/components/observability/TrendChart", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("../src/components/observability/TrendChart")>();
  return {
    ...actual,
    TrendChart: (props: Parameters<typeof actual.TrendChart>[0]) => {
      trendChartSpy.calls.push(props as unknown as Record<string, unknown>);
      return actual.TrendChart(props);
    },
  };
});

beforeEach(() => {
  trendChartSpy.calls.length = 0;
});

afterEach(() => vi.restoreAllMocks());

/** dashboard 响应最小骨架 + 指定 capacityTrend，用于容量增量口径断言。 */
function mockDashboardCapacity(capacityTrend: Array<Record<string, unknown>>): void {
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
          repositoryCount: 1,
          assetCount: 2,
          logicalBytes: 8_600_000_000,
          requestCount: 4,
          downloadCount: 5,
          failureCount: 0,
          cacheHitRate: null,
        },
        requestTrend: [],
        capacityTrend,
        alerts: [],
      }),
    ),
  );
}

/** 容量趋势桶点（capacityTrend 元素的最小合法形态，logicalBytes 为逻辑字节数）。 */
function capacityPoint(from: string, logicalBytes: number): Record<string, unknown> {
  return {
    from,
    to: from,
    repositoryCount: 1,
    assetCount: 1,
    logicalBytes,
  };
}

/** 从拦截记录里取容量图（唯一以 unit="bytes" 渲染的 TrendChart）的 props。 */
function capacityChartProps(): Record<string, unknown> | undefined {
  return trendChartSpy.calls.find((call) => call.unit === "bytes");
}

function LocationProbe() {
  const location = useLocation();
  return <span data-testid="location-probe">{`${location.pathname}${location.search}`}</span>;
}

/**
 * 分组下载趋势（端点 A /observability/downloads/trend）最小合法响应。
 * devmock 未注册该端点（schema.gen 只有类型），按既有模式在测试内 server.use 覆盖：
 * 2 小时窗口 hour 桶 → 2 个桶点；按 groupBy 分别返回族 / IP 两套 totals 与稀疏时序。
 */
function mockDownloadTrend(): void {
  const span = { from: "2026-08-30T00:00:00.000Z", to: "2026-08-30T02:00:00.000Z" };
  const midBucket = "2026-08-30T01:00:00.000Z";
  server.use(
    http.get("*/api/v1/observability/downloads/trend", ({ request }) => {
      const groupBy = new URL(request.url).searchParams.get("groupBy") === "ip" ? "ip" : "family";
      const totals =
        groupBy === "ip"
          ? [
              { group: "10.0.2.1", count: 30 },
              { group: "10.0.3.5", count: 12 },
            ]
          : [
              { group: "Chrome", count: 30 },
              { group: "curl", count: 12 },
            ];
      // 稀疏「桶 × 组」时序：两桶各给一个点，组件负责把缺失桶补 0。
      const points = totals.flatMap((item) => [
        { from: span.from, to: midBucket, group: item.group, count: item.count },
        { from: midBucket, to: span.to, group: item.group, count: Math.floor(item.count / 2) },
      ]);
      return HttpResponse.json({
        ...span,
        effectiveBucket: "hour",
        groupBy,
        points,
        totals,
      });
    }),
  );
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

    // KPI 取服务端值（10/11/12），不从趋势数据重新汇总。
    // 趋势卡右上「最新值大数字」也取趋势末点（999），与 KPI 是两套口径，故 KPI 断言限定在核心指标带内。
    // 区间剖析条移除后，999 在页面上只应剩这一处——此前统计条会再算出均值/峰值/总量等重复的 999。
    const kpiBand = await screen.findByLabelText("核心指标带");
    expect(within(kpiBand).getByText("10")).toBeTruthy();
    expect(within(kpiBand).getByText("11")).toBeTruthy();
    expect(within(kpiBand).getByText("12")).toBeTruthy();
    expect(within(kpiBand).getByText("25.0%")).toBeTruthy();
    expect(within(kpiBand).queryByText("999")).toBeNull();
    expect(screen.getAllByText("999")).toHaveLength(1);
  });

  it("容量趋势绘制相对区间起点的增量：首点为 0、末点为区间增长量", async () => {
    const base = 8_610_000_000;
    mockDashboardCapacity([
      capacityPoint("2026-08-30T00:00:00.000Z", base),
      capacityPoint("2026-08-30T06:00:00.000Z", base + 20 * 1024 * 1024),
      capacityPoint("2026-08-30T12:00:00.000Z", base + 33 * 1024 * 1024),
    ]);
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    // 标题/摘要改为「增长」语义，role=img 的 aria-label 由二者拼成。
    await screen.findByRole("img", {
      name: "容量增长（相对区间起点）：以区间起点为 0，展示期间增长量",
    });

    const capacity = capacityChartProps();
    const primary = capacity?.primary as Array<{ value: number }>;
    // 增量口径：首点恒为 0，末点＝区间增长量（与 8.6 GB 基数无关）。
    expect(primary.map((point) => point.value)).toEqual([0, 20 * 1024 * 1024, 33 * 1024 * 1024]);
    expect(capacity?.primaryLabel).toBe("增长量");
    // 增量非负 → 纵轴自 0 起，增长才有可比性。
    expect(capacity?.yDomain).toEqual([0, "auto"]);
    // 右上读数＝当前增量（末点），带符号细粒度而非 "8.0 GB" 总量。
    expect(await screen.findByText("+33.0 MB")).toBeTruthy();
  });

  it("区间内出现负增量时纵轴退化为 auto，读数带负号", async () => {
    const base = 8_610_000_000;
    mockDashboardCapacity([
      capacityPoint("2026-08-30T00:00:00.000Z", base),
      capacityPoint("2026-08-30T12:00:00.000Z", base - 16 * 1024 * 1024),
    ]);
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    await screen.findByRole("img", {
      name: "容量增长（相对区间起点）：以区间起点为 0，展示期间增长量",
    });

    const capacity = capacityChartProps();
    // 容量下降：固定下界 0 会截断负值，故退化为 auto/auto 贴合数据两端。
    expect(capacity?.yDomain).toEqual(["auto", "auto"]);
    expect(await screen.findByText("-16.0 MB")).toBeTruthy();
  });

  it("容量趋势为空数组时不崩溃且不渲染读数", async () => {
    mockDashboardCapacity([]);
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    // 空序列走 TrendChart 的空态分支（不渲染 role=img），页面显示"暂无样本数据"
    // （请求趋势同为空序列，故这里可能有多个空态占位）。
    expect((await screen.findAllByText("暂无样本数据")).length).toBeGreaterThanOrEqual(1);

    const capacity = capacityChartProps();
    expect(capacity?.primary).toEqual([]);
    expect(capacity?.yDomain).toEqual([0, "auto"]);
    // 无数据 → headerRight 为 null，不出现任何增量读数。
    expect(capacity?.headerRight).toBeNull();
  });

  it("容量趋势单点时增量为 0：首点即基准，显示 +0 B", async () => {
    mockDashboardCapacity([capacityPoint("2026-08-30T00:00:00.000Z", 8_610_000_000)]);
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    await screen.findByRole("img", {
      name: "容量增长（相对区间起点）：以区间起点为 0，展示期间增长量",
    });

    const capacity = capacityChartProps();
    const primary = capacity?.primary as Array<{ value: number }>;
    expect(primary.map((point) => point.value)).toEqual([0]);
    // 单点：增量为 0，显示 "+0 B" 而非空白。
    expect(await screen.findByText("+0 B")).toBeTruthy();
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

  it("拖选聚焦子时段：胶囊出现、双击还原（已移除的剖析条不再渲染）", () => {
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
    // 区间剖析条已移除：聚焦段的统计口径（原先均值 15 / 峰值 20）与标题都不再渲染
    expect(screen.queryByText("区间剖析")).toBeNull();
    expect(screen.queryByText("均值")).toBeNull();
    expect(screen.queryByText("15")).toBeNull();

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
    // KPI 值限定在核心指标带内断言；趋势卡区间剖析条已移除，页面不再出现其标题文案。
    const kpiBand = screen.getByLabelText("核心指标带");
    expect(within(kpiBand).getByText("仓库数")).toBeTruthy();
    expect(within(kpiBand).getByText("9")).toBeTruthy();
    expect(within(kpiBand).getByText("28,416")).toBeTruthy();
    expect(within(kpiBand).getByText("128")).toBeTruthy();
    expect(within(kpiBand).getByText("92")).toBeTruthy();
    expect(within(kpiBand).getByText("1")).toBeTruthy();
    expect(within(kpiBand).getByText("92.9%")).toBeTruthy();
    // 区间剖析条已整体移除（不再有「区间剖析」标题，故其数字也无从出现）
    expect(screen.queryByText("区间剖析")).toBeNull();
    // 不混入主机指标
    expect(screen.queryByText(/CPU|内存|主机磁盘/)).toBeNull();
  });

  it("仓库状态面板展示各仓库连接状态分布", async () => {
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    expect(await screen.findByText("仓库状态")).toBeTruthy();
    // devmock 种子：9 个仓库（KPI 与列表一致）；4 可用 / 3 自动阻止 / 1 半开 / 1 不可用
    expect(screen.getByText("npm-proxy")).toBeTruthy();
    expect(screen.getByText("maven-central")).toBeTruthy();
    expect(screen.getByText("docker-hub")).toBeTruthy();
    expect(screen.getByText("pypi-mirror")).toBeTruthy();
    expect(screen.getAllByText("自动阻止").length).toBeGreaterThanOrEqual(3);
    // FR-43：半开（窗口已到、正在试探上游）与自动阻止同属阻止态，但必须显示为不同状态，
    // 否则「正在试探」会被误读成「窗口内封锁」。
    expect(screen.getAllByText("试探中").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("不可用").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("共 9 个仓库")).toBeTruthy();
    // 明细行展示制品数与体积（devmock 种子 maven-releases：1284 制品 / 8589934592 字节 → 8.0 GB）。
    const mavenRow = screen.getByRole("button", { name: "maven-releases" });
    expect(within(mavenRow).getByText("制品数 1,284 · 体积 8.0 GB")).toBeTruthy();
  });

  it("仓库明细字段缺失时以 — 占位，不渲染 0 假象", async () => {
    server.use(
      http.get("*/api/v1/repositories", () =>
        HttpResponse.json({
          total: 2,
          items: [
            {
              id: 101,
              name: "stats-full",
              format: "npm",
              type: "hosted",
              visibility: "public",
              createdAt: "2026-01-01T00:00:00Z",
              artifactCount: 42,
              totalSize: 1024,
            },
            {
              id: 102,
              name: "stats-missing",
              format: "raw",
              type: "hosted",
              visibility: "private",
              createdAt: "2026-01-01T00:00:00Z",
              // 故意缺 artifactCount / totalSize：面板应显示 — 而非 0
            },
          ],
        }),
      ),
    );
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    const list = await screen.findByTestId("repo-status-list");
    const fullRow = within(list).getByRole("button", { name: "stats-full" });
    expect(within(fullRow).getByText("制品数 42 · 体积 1.0 KB")).toBeTruthy();
    const missingRow = within(list).getByRole("button", { name: "stats-missing" });
    expect(within(missingRow).getByText("制品数 — · 体积 —")).toBeTruthy();
  });

  it("仓库状态明细限高滚动，超出可视行数时给出查看全部出口", async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <>
        <DashboardPage />
        <LocationProbe />
      </>,
      { route: "/dashboard", authenticated: true },
    );

    // 限高：仓库再多（档位 ×16 可达上百个）也不会把左侧主区无限拉长。
    const list = await screen.findByTestId("repo-status-list");
    const maxHeight = Number.parseInt(list.style.maxHeight, 10);
    expect(Number.isInteger(maxHeight)).toBe(true);
    expect(maxHeight).toBeLessThanOrEqual(400);
    // 明细仍是全量（区域内滚动即可看全），不是截断成前 N 个。
    expect(within(list).getAllByRole("button")).toHaveLength(9);

    // 页眉出口：去仓库列表看全量（种子 9 个 > 一屏可视行数）。
    await user.click(screen.getByRole("button", { name: "查看全部仓库" }));
    expect(screen.getByTestId("location-probe").textContent).toBe("/repositories");
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

    // 面板经 actionLabel 渲染译文（修复「裸 action 键」后不再是原始 action 串）。
    const attentionButtons = await screen.findAllByRole("button", { name: "管理登录被拒绝" });
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
    expect(within(panel as HTMLElement).getByRole("button", { name: "删除制品" })).toBeTruthy();
    expect(within(panel as HTMLElement).queryByRole("button", { name: "repo.delete" })).toBeNull();
  });

  it("分组下载趋势卡渲染折线图并以 aria-label 暴露标题", async () => {
    mockDownloadTrend();
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    const card = await screen.findByTestId("download-grouped-trend");
    // 骨架 / 错误态只有文本标题，role="img" 出现才代表成功态折线图已挂载。
    expect(await within(card).findByRole("img", { name: "分组下载趋势" })).toBeTruthy();
    // 底部双饼同样以 role="img" + aria-label 暴露可读标题。
    expect(within(card).getByRole("img", { name: "IP 占比" })).toBeTruthy();
    expect(within(card).getByRole("img", { name: "客户端族占比" })).toBeTruthy();
  });

  it("分组切换按钮的 aria-pressed 随点击在 family / ip 间翻转", async () => {
    const user = userEvent.setup();
    mockDownloadTrend();
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    const card = await screen.findByTestId("download-grouped-trend");
    await within(card).findByRole("img", { name: "分组下载趋势" });

    // 初始态：默认按客户端族分组。
    expect(
      within(card).getByRole("button", { name: "客户端族" }).getAttribute("aria-pressed"),
    ).toBe("true");
    expect(within(card).getByRole("button", { name: "IP" }).getAttribute("aria-pressed")).toBe(
      "false",
    );

    await user.click(within(card).getByRole("button", { name: "IP" }));
    // 两个 groupBy 的数据挂载时已并发取回，切换只翻转本地态即可换图。
    expect(within(card).getByRole("button", { name: "IP" }).getAttribute("aria-pressed")).toBe(
      "true",
    );
    expect(
      within(card).getByRole("button", { name: "客户端族" }).getAttribute("aria-pressed"),
    ).toBe("false");
    // 图例同步切到 IP 维度的组（确认数据真的换了，而不只是按钮态翻转）。
    expect(within(card).getByRole("button", { name: "10.0.2.1" })).toBeTruthy();
  });

  it("点击图例隐藏系列：aria-pressed 由 true 变 false 并加 trend-legend-hidden 类", async () => {
    const user = userEvent.setup();
    mockDownloadTrend();
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    const card = await screen.findByTestId("download-grouped-trend");
    await within(card).findByRole("img", { name: "分组下载趋势" });

    const legend = within(card).getByRole("button", { name: "Chrome" });
    expect(legend.getAttribute("aria-pressed")).toBe("true");
    expect(legend.classList.contains("trend-legend-hidden")).toBe(false);

    await user.click(legend);

    const hiddenLegend = within(card).getByRole("button", { name: "Chrome" });
    expect(hiddenLegend.getAttribute("aria-pressed")).toBe("false");
    expect(hiddenLegend.classList.contains("trend-legend-hidden")).toBe(true);
  });

  it("点击占比饼图例跳审计页，携带审计识别的 clientIp / q 参数", async () => {
    const user = userEvent.setup();
    mockDownloadTrend();
    renderWithProviders(
      <>
        <DashboardPage />
        <LocationProbe />
      </>,
      { route: "/dashboard", authenticated: true },
    );

    const card = await screen.findByTestId("download-grouped-trend");
    await within(card).findByRole("img", { name: "分组下载趋势" });
    const probe = () => screen.getByTestId("location-probe").textContent ?? "";
    const params = () => new URLSearchParams(probe().split("?")[1] ?? "");

    // IP 饼图例 → /audit-logs?range=custom&...&clientIp=<组值>（注意是小写 p 的 clientIp）。
    await user.click(within(card).getByRole("button", { name: "IP 占比 10.0.2.1" }));
    await waitFor(() => expect(probe()).toContain("/audit-logs?"));
    expect(params().get("range")).toBe("custom");
    expect(params().get("clientIp")).toBe("10.0.2.1");
    expect(params().get("from")).toBeTruthy();
    expect(params().get("to")).toBeTruthy();
    expect(params().has("attentionId")).toBe(false);

    // 客户端族饼图例 → 带关键字 q（审计侧模糊匹配），同一用例内再次导航覆盖上一次参数。
    await user.click(within(card).getByRole("button", { name: "客户端族占比 Chrome" }));
    await waitFor(() => expect(params().get("q")).toBe("Chrome"));
    expect(params().get("range")).toBe("custom");
    expect(params().has("clientIp")).toBe(false);
    expect(params().has("attentionId")).toBe(false);
  });

  it("旧下载累计趋势独立图已移除，分组下载趋势卡全页唯一", async () => {
    mockDownloadTrend();
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });

    const card = await screen.findByTestId("download-grouped-trend");
    await within(card).findByRole("img", { name: "分组下载趋势" });

    // 新组件全页唯一（旧独立图已删除，不应出现第二张同类卡）。
    expect(screen.getAllByTestId("download-grouped-trend")).toHaveLength(1);
    // 旧独立图的可辨识文案（i18n 残留键 trendDownloads / downloadTopIps / downloadFamilies，
    // src 已无引用）不再出现：标题为独立文本节点，故用精确匹配避免误中「分组下载趋势」。
    expect(screen.queryByText("下载趋势", { exact: true })).toBeNull();
    expect(screen.queryByText("来源 IP Top 10 · 独立来源口径")).toBeNull();
    expect(screen.queryByText("客户端分布（UA 归类）")).toBeNull();
    // 旧图的 aria-label 形如「下载趋势：<summary>」；新图以「分组下载趋势」开头，不会误中。
    expect(screen.queryByRole("img", { name: /^下载趋势：/ })).toBeNull();
  });

  it("分组小时桶与服务端整点键对齐，to 边界采用右开窗口", () => {
    const buckets = buildBucketAxis("2026-09-23T10:37:30Z", "2026-09-23T12:00:00Z", "hour");

    expect(
      buckets.map((bucket) => ({
        key: new Date(bucket.keyMs).toISOString(),
        from: bucket.from,
        to: bucket.to,
      })),
    ).toEqual([
      {
        key: "2026-09-23T10:00:00.000Z",
        from: "2026-09-23T10:37:30.000Z",
        to: "2026-09-23T11:00:00.000Z",
      },
      {
        key: "2026-09-23T11:00:00.000Z",
        from: "2026-09-23T11:00:00.000Z",
        to: "2026-09-23T12:00:00.000Z",
      },
    ]);
  });

  it("分钟桶跳过早于 from 的源桶且不生成 to 所在分钟", () => {
    const buckets = buildBucketAxis("2026-09-23T10:37:30Z", "2026-09-23T10:40:00Z", "minute");

    expect(buckets.map((bucket) => new Date(bucket.keyMs).toISOString())).toEqual([
      "2026-09-23T10:38:00.000Z",
      "2026-09-23T10:39:00.000Z",
    ]);
    expect(buckets[0]?.from).toBe("2026-09-23T10:38:00.000Z");
    expect(buckets.at(-1)?.to).toBe("2026-09-23T10:40:00.000Z");
  });
});
