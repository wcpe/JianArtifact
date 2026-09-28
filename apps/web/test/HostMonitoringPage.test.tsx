import { HttpResponse, http } from "msw";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { server } from "@jianartifact/devmock/node";
import { HostMonitoringPage } from "../src/pages/HostMonitoringPage";
import i18n from "../src/i18n";
import { formatBytes } from "../src/lib/format";
import { renderWithProviders } from "./harness";

// jsdom 下 recharts 容器恒为 0×0（setup.ts 的 ResizeObserver 替身所限），图表 SVG 从不真正
// 渲染，无法用刻度 / DOM 断言 Y 轴。故拦截 TrendChart，改为记录它收到的 props
//（yDomain / secondary / 各 series label），用它验证图表口径——这是本文件验证图表配置的标准手段。
type TrendChartCall = {
  title: string;
  summary: string;
  primary: unknown;
  secondary?: unknown;
  tertiary?: unknown;
  primaryLabel: string;
  secondaryLabel?: string;
  tertiaryLabel?: string;
  yDomain?: unknown;
  rightPercentAxis?: boolean;
  headerRight?: ReactNode;
};

// 跨用例收集的 props 记录：vi.hoisted 保证它在模块导入（触发 vi.mock 工厂）之前完成初始化。
const trendChartCalls = vi.hoisted(() => [] as TrendChartCall[]);

vi.mock("../src/components/observability/TrendChart", () => ({
  TrendChart: (props: TrendChartCall) => {
    trendChartCalls.push(props);
    // 渲染与真实组件等价的可见文本（标题 / 摘要 / 图例按钮 / 图头附加内容），让既有断言继续可用。
    return (
      <div data-testid={`trend-${props.title}`}>
        <span>{props.title}</span>
        <span>{props.summary}</span>
        {props.headerRight}
        <button type="button">{props.primaryLabel}</button>
        {props.secondaryLabel ? <button type="button">{props.secondaryLabel}</button> : null}
        {props.tertiaryLabel ? <button type="button">{props.tertiaryLabel}</button> : null}
      </div>
    );
  },
}));

describe("当前主机监控", () => {
  beforeEach(() => {
    trendChartCalls.length = 0;
  });

  it("开发态读取真实 DevMock 主机接口，而非固定预览", async () => {
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });
    expect(screen.getByText("CPU 使用趋势")).toBeTruthy();
    expect(screen.getByText("网络流量趋势")).toBeTruthy();
    expect(screen.queryByText("开发预览数据")).toBeNull();
    expect(screen.queryByText(/同步令牌|JIAN_/)).toBeNull();
  });

  it("内存与磁盘展示总量/已用/可用三值与比率，运行时长出现在进程指标表", async () => {
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });
    // 内存三值标签（mock：总量 16.0 GB 固定，已用/可用随采样点变化）；
    // 「可用内存」仍出现在三值格里（趋势图已不再画可用线，故仅此一处）。
    expect(screen.getByText("内存总量")).toBeTruthy();
    expect(screen.getByText("内存已用")).toBeTruthy();
    expect(screen.getAllByText("可用内存").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("内存使用率")).toBeTruthy();
    expect(screen.getAllByText("16.0 GB").length).toBeGreaterThanOrEqual(1);
    // 磁盘三值标签（mock：总量 1000.0 GB 固定）；标签同时出现在三值格与图例，用 getAllByText
    expect(screen.getAllByText("磁盘总量").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("磁盘已用").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("磁盘占用率")).toBeTruthy();
    expect(screen.getAllByText(/GB|TB/).length).toBeGreaterThanOrEqual(3);
    // 进程指标表：表标题、三列与运行时长行（mock 运行时长 > 1 小时 → 形如 01:00:33）；
    // 首行标签「进程 CPU」由 hostMonitoring.processCpu 键提供，不应再是裸键。
    expect(screen.getByText("进程指标")).toBeTruthy();
    expect(screen.getByText("指标")).toBeTruthy();
    expect(screen.getByText("当前值")).toBeTruthy();
    expect(screen.getByText("说明")).toBeTruthy();
    expect(screen.getByText("进程 CPU")).toBeTruthy();
    expect(screen.queryByText("hostMonitoring.processCpu")).toBeNull();
    expect(screen.getByText("运行时长")).toBeTruthy();
    expect(screen.getByText(/挂钟口径，仅统计当前进程/)).toBeTruthy();
    expect(screen.getAllByText(/\d{2}:\d{2}:\d{2}/).length).toBeGreaterThanOrEqual(1);
    // 节点状态栏第三个 Divider 标签「指标明细」由 hostMonitoring.metricDetails 键提供
    expect(screen.getByText("指标明细")).toBeTruthy();
    // 内存图只留「已用内存」（面积）+「进程 RSS」（同左轴的次系列）两条图例；可用线已移除
    expect(screen.getByRole("button", { name: "已用内存" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "进程 RSS" })).toBeTruthy();
    expect(screen.queryByText("进程内存趋势")).toBeNull();
  });

  it("内存与磁盘纵轴均以各自容量为顶，磁盘退回单一左轴（已用面积 + 空白即可用）", async () => {
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });

    const memory = trendChartCalls.find((call) => call.title === "内存趋势");
    expect(memory).toBeTruthy();
    // 纵轴 = [0, 最新样本的系统内存总量]：devmock 固定 16 GiB → 16.0 GB
    const domain = memory!.yDomain as [number, number];
    expect(domain[0]).toBe(0);
    expect(formatBytes(domain[1])).toBe("16.0 GB");
    // 已移除 secondary=可用内存线：空闲由「已用之上到轴顶」的空白表示；
    // 进程 RSS 改为 secondary（与内存共用左轴，故贴近图底），不再走右轴 → tertiary 必为空。
    expect(memory!.secondary).toBeTruthy();
    expect(memory!.tertiary).toBeUndefined();
    expect(memory!.tertiaryLabel).toBeUndefined();
    expect(memory!.primaryLabel).toBe("已用内存");
    expect(memory!.secondaryLabel).toBe("进程 RSS");
    expect(memory!.summary).toContain("空闲");
    // 内存图启用右轴百分比：已用 ÷ 内存总量，左轴读字节、右轴读占比。
    expect(memory!.rightPercentAxis).toBe(true);

    const disk = trendChartCalls.find((call) => call.title === "磁盘趋势");
    expect(disk).toBeTruthy();
    // 磁盘图与内存图同构：纵轴 = [0, 数据目录卷总量]（devmock 固定 1 TB → 1000.0 GB），
    // primary=已用面积，可用由「已用之上到轴顶」的空白表达；不再有 secondary（可用线）与
    // tertiary（总量右轴参照线）——旧口径会让已用与总量各自贴顶、被误读成「已用≈总量」。
    const diskDomain = disk!.yDomain as [number, number];
    expect(diskDomain[0]).toBe(0);
    expect(formatBytes(diskDomain[1])).toBe("1000.0 GB");
    expect(disk!.primaryLabel).toBe("已用磁盘");
    expect(disk!.secondary).toBeUndefined();
    expect(disk!.tertiary).toBeUndefined();
    expect(disk!.secondaryLabel).toBeUndefined();
    expect(disk!.tertiaryLabel).toBeUndefined();
    expect(disk!.summary).toContain("空白");
    // 磁盘图同样启用右轴百分比（已用 ÷ 数据目录卷总量）。
    expect(disk!.rightPercentAxis).toBe(true);
  });

  it("进程指标卡与内存图同排，底部只剩磁盘与网络两格平分", async () => {
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });

    // 内存图与进程指标卡各自的 Grid.Col 是相邻兄弟（同一行内的两列）：
    // 顺序为 内存卡(lg:8) → 进程指标卡(lg:4)，两者父节点同为该 Grid 的 inner 容器。
    // 注意：Mantine 每个 Grid.Col 会在同一父级插入一个 <style>（列变量），故按 Grid-col 过滤后再比顺序。
    const memoryCol = screen.getByTestId("trend-内存趋势").closest("[class*='Grid-col']");
    const processCol = screen.getByText("进程指标").closest("[class*='Grid-col']");
    expect(memoryCol).toBeTruthy();
    expect(processCol).toBeTruthy();
    expect(processCol!.parentElement).toBe(memoryCol!.parentElement);
    const cols = Array.from(memoryCol!.parentElement!.children).filter((el) =>
      el.className.toString().includes("Grid-col"),
    );
    const memoryIndex = cols.indexOf(memoryCol!);
    expect(memoryIndex).toBeGreaterThanOrEqual(0);
    expect(cols[memoryIndex + 1]).toBe(processCol);

    // 底部只剩磁盘 / 网络两张图，同一网格直接子节点恰为 2（各占一格、平分）。
    const diskCell = screen.getByTestId("trend-磁盘趋势").parentElement;
    const networkCell = screen.getByTestId("trend-网络流量趋势").parentElement;
    const bottomGrid = diskCell!.parentElement;
    expect(bottomGrid).toBe(networkCell!.parentElement);
    expect(bottomGrid!.children.length).toBe(2);
    // 进程指标卡已移出底部网格（不再有第三格）。
    expect(bottomGrid!.textContent).not.toContain("进程指标");
  });

  it("最新样本缺 memoryTotalBytes 时内存纵轴回退自适应，而非塌缩到 0", async () => {
    server.use(
      http.get("*/api/v1/observability/host", () => {
        const base = {
          from: "2024-05-01T00:00:00Z",
          to: "2024-05-01T01:00:00Z",
          hostState: { state: "ok" },
          networkState: { state: "ok" },
          processState: { state: "ok" },
          readinessState: { state: "ok" },
          // 故意不给 memoryTotalBytes：验证 [0, total] 的兜底分支
          memoryUsedBytes: 4294967296,
          diskAvailableBytes: 107374182400,
          networkReceiveBytesPerSecond: 1024,
          networkTransmitBytesPerSecond: 512,
          processRssBytes: 67108864,
          processCpuPercent: 2.5,
          goroutineCount: 40,
        };
        return HttpResponse.json({
          hostState: "healthy",
          latestSampleAt: base.to,
          effectiveBucket: "minute",
          latest: base,
          samples: [base],
        });
      }),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });
    const memory = trendChartCalls.find((call) => call.title === "内存趋势");
    expect(memory!.yDomain).toEqual(["auto", "auto"]);
  });

  it("最新样本缺 diskTotalBytes 时磁盘纵轴回退自适应，且面积取已用而非可用", async () => {
    server.use(
      http.get("*/api/v1/observability/host", () => {
        const base = {
          from: "2024-05-01T00:00:00Z",
          to: "2024-05-01T01:00:00Z",
          hostState: { state: "ok" },
          networkState: { state: "ok" },
          processState: { state: "ok" },
          readinessState: { state: "ok" },
          memoryTotalBytes: 8589934592,
          memoryUsedBytes: 4294967296,
          // 故意不给 diskTotalBytes：验证 [0, total] 的兜底分支
          diskUsedBytes: 536870912000, // 500 GB（断言面积取此值）
          diskAvailableBytes: 107374182400, // 100 GB（若面积误取可用，下面的断言会失败）
          networkReceiveBytesPerSecond: 1024,
          networkTransmitBytesPerSecond: 512,
          processRssBytes: 67108864,
          processCpuPercent: 2.5,
          goroutineCount: 40,
        };
        return HttpResponse.json({
          hostState: "healthy",
          latestSampleAt: base.to,
          effectiveBucket: "minute",
          latest: base,
          samples: [base],
        });
      }),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });
    const disk = trendChartCalls.find((call) => call.title === "磁盘趋势");
    expect(disk!.yDomain).toEqual(["auto", "auto"]);
    // primary 的面积数据必须来自「已用」：用两个不同的磁盘值绑定，避免只靠 label 无法区分
    // 已用与可用。devmock 缺 diskTotalBytes 时面积为唯一的已用值。
    expect(disk!.primary).toEqual([{ label: expect.any(String), value: 536870912000 }]);
  });

  it("历史样本缺新容量字段时降级为 —，不伪造 0", async () => {
    server.use(
      http.get("*/api/v1/observability/host", () => {
        const base = {
          from: "2024-05-01T00:00:00Z",
          to: "2024-05-01T01:00:00Z",
          hostState: { state: "ok" },
          networkState: { state: "ok" },
          processState: { state: "ok" },
          readinessState: { state: "ok" },
          memoryTotalBytes: 8589934592,
          memoryAvailableBytes: 4294967296,
          diskAvailableBytes: 107374182400,
          networkReceiveBytesPerSecond: 1024,
          networkTransmitBytesPerSecond: 512,
          processRssBytes: 67108864,
          processCpuPercent: 2.5,
          goroutineCount: 40,
        };
        return HttpResponse.json({
          hostState: "healthy",
          latestSampleAt: base.to,
          effectiveBucket: "minute",
          latest: base,
          samples: [base],
        });
      }),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });
    // 老数据无 memoryUsedBytes：内存已用按 total − available 兜底（8 − 4 = 4.0 GB）
    // 「4.0 GB」也会出现在趋势图剖析条等处，故用 getAllByText。
    expect(screen.getAllByText("4.0 GB").length).toBeGreaterThanOrEqual(1);
    // 老数据无 diskTotalBytes / diskUsedBytes / processUptimeSeconds：显示占位 "—"
    const dashes = screen.getAllByText("—");
    expect(dashes.length).toBeGreaterThanOrEqual(3);
  });

  it("真实接口无样本时明确呈现空态", async () => {
    server.use(
      http.get("*/api/v1/observability/host", () =>
        HttpResponse.json({ hostState: "unknown", effectiveBucket: "minute", samples: [] }),
      ),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    expect(await screen.findByText("尚无主机样本")).toBeTruthy();
  });

  it("真实接口失败时展示可重试错误，而不伪装为预览数据", async () => {
    server.use(
      http.get("*/api/v1/observability/host", () =>
        HttpResponse.json(
          { error: { code: "host_unavailable", message: "采样服务暂不可用" } },
          { status: 503 },
        ),
      ),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    expect(await screen.findByText("无法读取主机监控")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
  });

  it("采样范围档位跟随语言，而非硬编码中文", async () => {
    await i18n.changeLanguage("en");
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    // 该页全部文案（含状态行）都跟随语言，故等待英文版状态文本即代表已切到英文。
    expect(
      await screen.findByText("Sampling OK, host metrics available", undefined, { timeout: 5000 }),
    ).toBeTruthy();
    // 5 个档位由 hostMonitoring.range* 键提供，切英文后应显示英文且不再残留中文。
    expect(screen.getByRole("button", { name: "Last 24 hours" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Last 7 days" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "近 24 小时" })).toBeNull();
    // 新补的 processCpu / metricDetails 键在英文侧同样本地化，不留裸键。
    expect(screen.getByText("Process CPU")).toBeTruthy();
    expect(screen.getByText("Metric Details")).toBeTruthy();
    expect(screen.queryByText("hostMonitoring.processCpu")).toBeNull();
    expect(screen.queryByText("hostMonitoring.metricDetails")).toBeNull();
  });

  // 网络卡固定基线：累计总量取最新样本 total，网卡列表提供两个网卡供选择器断言。
  // 所有其它字段都给全，避免进程指标表等处出现额外的 "—" 干扰断言。
  const networkBase = {
    from: "2024-05-01T00:00:00Z",
    to: "2024-05-01T01:00:00Z",
    hostState: { state: "ok" },
    networkState: { state: "ok" },
    processState: { state: "ok" },
    readinessState: { state: "ok" },
    memoryTotalBytes: 8589934592,
    memoryAvailableBytes: 4294967296,
    memoryUsedBytes: 4294967296,
    diskTotalBytes: 1073741824000,
    diskAvailableBytes: 107374182400,
    diskUsedBytes: 966367641600,
    networkReceiveBytesPerSecond: 10240,
    networkTransmitBytesPerSecond: 5120,
    networkReceiveBytesTotal: 5_100_000_000,
    networkTransmitBytesTotal: 1_920_000_000,
    processRssBytes: 67108864,
    processCpuPercent: 2.5,
    processUptimeSeconds: 3600,
    goroutineCount: 40,
    openFileDescriptors: 30,
  };
  const networkInterfaces = [
    { name: "eth0", receiveBytesTotal: 4_200_000_000, transmitBytesTotal: 1_600_000_000 },
    { name: "wlan0", receiveBytesTotal: 900_000_000, transmitBytesTotal: 320_000_000 },
  ];

  it("网络卡展示总发送/总接收，选择器列出「全部网卡」与响应里的网卡名", async () => {
    server.use(
      http.get("*/api/v1/observability/host", () =>
        HttpResponse.json({
          hostState: "healthy",
          latestSampleAt: networkBase.to,
          effectiveBucket: "minute",
          latest: networkBase,
          samples: [networkBase],
          networkInterfaces,
        }),
      ),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });
    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });

    // 总量两项按最新样本 total 格式化（自网卡启动以来的累计值）。
    expect(screen.getByText("总接收")).toBeTruthy();
    expect(screen.getByText("总发送")).toBeTruthy();
    expect(screen.getByText(formatBytes(networkBase.networkReceiveBytesTotal))).toBeTruthy();
    expect(screen.getByText(formatBytes(networkBase.networkTransmitBytesTotal))).toBeTruthy();

    // 速率图两条线（接收 / 发送）保持不变。
    expect(screen.getByRole("button", { name: "接收" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "发送" })).toBeTruthy();

    // 选择器：默认「全部网卡」，展开后含响应里的两个网卡名。
    const user = userEvent.setup();
    await user.click(screen.getByRole("combobox", { name: /网卡/ }));
    expect(await screen.findByRole("option", { name: "全部网卡" })).toBeTruthy();
    expect(await screen.findByRole("option", { name: "eth0" })).toBeTruthy();
    expect(await screen.findByRole("option", { name: "wlan0" })).toBeTruthy();
  });

  it("切换网卡后重新请求带上 interface 查询参数，未选择时不带", async () => {
    const requestedInterfaces: (string | null)[] = [];
    server.use(
      http.get("*/api/v1/observability/host", ({ request }) => {
        requestedInterfaces.push(new URL(request.url).searchParams.get("interface"));
        return HttpResponse.json({
          hostState: "healthy",
          latestSampleAt: networkBase.to,
          effectiveBucket: "minute",
          latest: networkBase,
          samples: [networkBase],
          networkInterfaces,
        });
      }),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });
    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });

    // 首载走「全部网卡」聚合：不带 interface 参数。
    expect(requestedInterfaces.length).toBeGreaterThanOrEqual(1);
    expect(requestedInterfaces.at(-1)).toBeNull();

    const user = userEvent.setup();
    await user.click(screen.getByRole("combobox", { name: /网卡/ }));
    await user.click(await screen.findByRole("option", { name: "eth0" }));

    // 选定网卡后重新取数并带上 interface=eth0。
    await waitFor(() => expect(requestedInterfaces).toContain("eth0"));
  });

  it("最新样本缺网络累计总量时显示 — 而非 0", async () => {
    const noTotals = {
      ...networkBase,
      // 故意不给累计总量（历史样本 / 采集失败）：应显示占位，绝不伪造 0。
      networkReceiveBytesTotal: undefined,
      networkTransmitBytesTotal: undefined,
    };
    server.use(
      http.get("*/api/v1/observability/host", () =>
        HttpResponse.json({
          hostState: "healthy",
          latestSampleAt: networkBase.to,
          effectiveBucket: "minute",
          latest: noTotals,
          samples: [noTotals],
          networkInterfaces,
        }),
      ),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });
    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });

    // 页面其余字段都给全，故 "—" 恰好是网络卡两项总量。
    expect(screen.getAllByText("—").length).toBe(2);
    expect(screen.queryByText("0 B")).toBeNull();
  });

  it("networkInterfaces 为空时不渲染网卡选择器", async () => {
    server.use(
      http.get("*/api/v1/observability/host", () =>
        HttpResponse.json({
          hostState: "healthy",
          latestSampleAt: networkBase.to,
          effectiveBucket: "minute",
          latest: networkBase,
          samples: [networkBase],
          networkInterfaces: [],
        }),
      ),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });
    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });

    // 无网卡列表（历史数据 / 非 Linux 平台）时整块选择器不渲染；总量仍展示。
    expect(screen.queryByRole("combobox", { name: /网卡/ })).toBeNull();
    expect(screen.queryByText("全部网卡")).toBeNull();
    expect(screen.getByText("总接收")).toBeTruthy();
    expect(screen.getByText("总发送")).toBeTruthy();
  });

  it("切换网卡时叠加加载遮罩，且旧数据仍保留在 DOM", async () => {
    // 第二次（切换网卡）请求挂起：把 refreshing 态钉住，便于断言「响应未回」的那一帧。
    let releaseSwitch: () => void = () => {};
    const switchGate = new Promise<void>((resolve) => {
      releaseSwitch = resolve;
    });
    server.use(
      http.get("*/api/v1/observability/host", async ({ request }) => {
        const iface = new URL(request.url).searchParams.get("interface");
        if (iface === "eth0") await switchGate;
        return HttpResponse.json({
          hostState: "healthy",
          latestSampleAt: networkBase.to,
          effectiveBucket: "minute",
          latest: networkBase,
          samples: [networkBase],
          networkInterfaces,
        });
      }),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });
    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });

    // 初始加载完成：没有遮罩。
    expect(screen.queryByTestId("host-monitoring-refreshing")).toBeNull();

    const user = userEvent.setup();
    await user.click(screen.getByRole("combobox", { name: /网卡/ }));
    await user.click(await screen.findByRole("option", { name: "eth0" }));

    // 响应未回：出现加载遮罩；同时旧数据（总接收 / 总发送）仍在 DOM —— keepPreviousData 未被破坏，
    // 只是补上了此前缺失的「正在换数据」反馈。
    await waitFor(() => expect(screen.getByTestId("host-monitoring-refreshing")).toBeTruthy());
    expect(screen.getByText("总接收")).toBeTruthy();
    expect(screen.getByText("总发送")).toBeTruthy();
    expect(screen.getByText(formatBytes(networkBase.networkReceiveBytesTotal))).toBeTruthy();
    expect(screen.getByText(formatBytes(networkBase.networkTransmitBytesTotal))).toBeTruthy();

    // 放行并等待遮罩消失，避免用例结束后仍有挂起的状态更新。
    releaseSwitch();
    await waitFor(() => expect(screen.queryByTestId("host-monitoring-refreshing")).toBeNull());
  });

  it("内存与磁盘趋势图启用右轴百分比，并在图头显示当前使用率", async () => {
    server.use(
      http.get("*/api/v1/observability/host", () =>
        HttpResponse.json({
          hostState: "healthy",
          latestSampleAt: networkBase.to,
          effectiveBucket: "minute",
          latest: networkBase,
          samples: [networkBase],
          networkInterfaces,
        }),
      ),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });
    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });

    // networkBase：内存 已用 4 GiB / 总量 8 GiB → 50.0%；磁盘 已用 900 GB / 总量 1000 GB → 90.0%。
    // 图头右附加内容与卡内三值共用同一口径（resolveMemoryUsed / diskUsagePercent）。
    expect(screen.getByText("内存使用率 50.0%")).toBeTruthy();
    expect(screen.getByText("磁盘占用率 90.0%")).toBeTruthy();

    const memory = trendChartCalls.find((call) => call.title === "内存趋势");
    const disk = trendChartCalls.find((call) => call.title === "磁盘趋势");
    expect(memory!.rightPercentAxis).toBe(true);
    expect(disk!.rightPercentAxis).toBe(true);
  });
});
