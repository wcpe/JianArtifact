// 趋势图单元测试：覆盖统计条已移除（含拖选聚焦回归）、键盘聚焦可达性、单位格式化、
// 二次聚焦的索引换算、Y 轴自定义域与左右双轴的刻度格式化。
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";

import { TrendChart } from "../src/components/observability/TrendChart";
import { renderWithProviders } from "./harness";

const POINTS = [
  { label: "10:00", value: 10 },
  { label: "10:05", value: 20 },
  { label: "10:10", value: 30 },
  { label: "10:15", value: 40 },
];

/** jsdom 无布局：mock 出可控宽度，让 clientX 能线性换算成数据点索引。 */
function mockRect(element: HTMLElement, width = 100) {
  vi.spyOn(element, "getBoundingClientRect").mockReturnValue({
    left: 0,
    width,
    height: 200,
    top: 0,
    right: width,
    bottom: 200,
    x: 0,
    y: 0,
    toJSON: () => ({}),
  } as DOMRect);
}

describe("趋势图", () => {
  it("统计条已移除：不再渲染区间剖析的标题与各项口径", () => {
    renderWithProviders(
      <TrendChart
        title="请求趋势"
        summary="请求量随时间的变化"
        primary={POINTS}
        primaryLabel="请求"
      />,
    );

    // 区间剖析条整体删除：标题与「均值 / 总量 / 峰值 / 谷值 / 区间变动」都不应出现在 DOM
    expect(screen.queryByText("区间剖析")).toBeNull();
    expect(screen.queryByText("均值")).toBeNull();
    expect(screen.queryByText("总量")).toBeNull();
    expect(screen.queryByText("峰值")).toBeNull();
    expect(screen.queryByText("谷值")).toBeNull();
    expect(screen.queryByText("区间变动")).toBeNull();
    // 原先由统计条算出的累计值 100（10 + 20 + 30 + 40）不再出现
    expect(screen.queryByText("100")).toBeNull();
  });

  it("百分比单位仍以 % 呈现读数，且不再渲染统计条", () => {
    renderWithProviders(
      <TrendChart
        title="CPU 趋势"
        summary="CPU 使用率随时间的变化"
        primary={POINTS}
        primaryLabel="CPU 使用率"
        unit="percent"
      />,
    );

    // 悬停读数（保留能力）仍按 percent 格式化：末点 40 → 40.0%
    fireEvent.mouseMove(screen.getByRole("img", { name: "CPU 趋势：CPU 使用率随时间的变化" }), {
      clientX: 100,
    });
    expect(screen.getByTestId("trend-hover-summary").textContent).toContain("CPU 使用率：40.0%");
    // 统计条已移除：百分比不再展示「均值」（原先 25.0%），标题也不在
    expect(screen.queryByText("均值")).toBeNull();
    expect(screen.queryByText("25.0%")).toBeNull();
    expect(screen.queryByText("区间剖析")).toBeNull();
  });

  it("字节单位读数走人类可读体积，且不再渲染统计条", () => {
    renderWithProviders(
      <TrendChart
        title="容量趋势"
        summary="逻辑体积随时间的变化"
        primary={[
          { label: "10:00", value: 1024 },
          { label: "10:05", value: 2048 },
        ]}
        primaryLabel="逻辑体积"
        unit="bytes"
      />,
    );

    // 悬停读数（保留能力）仍按 bytes 换算：末点 2048 → 2.0 KB
    fireEvent.mouseMove(screen.getByRole("img", { name: "容量趋势：逻辑体积随时间的变化" }), {
      clientX: 100,
    });
    expect(screen.getByTestId("trend-hover-summary").textContent).toContain("逻辑体积：2.0 KB");
    // 统计条已移除：不再有「均值 / 总量」
    expect(screen.queryByText("均值")).toBeNull();
    expect(screen.queryByText("总量")).toBeNull();
  });

  it("键盘方向键移动光标、Shift 扩选、Enter 提交聚焦、Esc 还原", () => {
    renderWithProviders(
      <TrendChart title="键盘趋势" summary="键盘可聚焦" primary={POINTS} primaryLabel="请求" />,
    );
    const chart = screen.getByRole("img", { name: "键盘趋势：键盘可聚焦" });

    fireEvent.keyDown(chart, { key: "ArrowRight" }); // 光标 → 索引 1
    expect(screen.getByTestId("trend-hover-summary").textContent).toContain("10:05");

    fireEvent.keyDown(chart, { key: "ArrowRight", shiftKey: true }); // 锚点 1，光标 → 2
    fireEvent.keyDown(chart, { key: "Enter" });

    expect(screen.getByText("已聚焦 10:05 – 10:10")).toBeTruthy();

    fireEvent.keyDown(chart, { key: "Escape" });
    expect(screen.queryByText(/已聚焦/)).toBeNull();
  });

  it("点击图例隐藏系列并可再次点击恢复，状态由 aria-pressed 与视觉类表达", () => {
    renderWithProviders(
      <TrendChart
        title="图例开关趋势"
        summary="图例可切换系列显隐"
        primary={POINTS}
        secondary={[
          { label: "10:00", value: 4 },
          { label: "10:05", value: 8 },
          { label: "10:10", value: 12 },
          { label: "10:15", value: 16 },
        ]}
        primaryLabel="请求"
        secondaryLabel="下载"
      />,
    );
    const chart = screen.getByRole("img", { name: "图例开关趋势：图例可切换系列显隐" });
    const downloadLegend = screen.getByRole("button", { name: "下载" });

    // 默认显示：aria-pressed=true、无隐藏视觉类
    expect(downloadLegend.getAttribute("aria-pressed")).toBe("true");
    expect(downloadLegend.className).not.toContain("trend-legend-hidden");

    // 悬停读数默认包含两个系列
    fireEvent.mouseMove(chart, { clientX: 100 });
    expect(screen.getByTestId("trend-hover-summary").textContent).toBe(
      "10:15 · 请求：40 · 下载：16",
    );

    // 点击隐藏：aria-pressed 翻转 + 隐藏视觉类出现；读数不再包含隐藏系列
    fireEvent.click(downloadLegend);
    expect(downloadLegend.getAttribute("aria-pressed")).toBe("false");
    expect(downloadLegend.className).toContain("trend-legend-hidden");
    fireEvent.mouseMove(chart, { clientX: 100 });
    expect(screen.getByTestId("trend-hover-summary").textContent).toBe("10:15 · 请求：40");

    // 再次点击恢复：状态与读数一并还原
    fireEvent.click(downloadLegend);
    expect(downloadLegend.getAttribute("aria-pressed")).toBe("true");
    expect(downloadLegend.className).not.toContain("trend-legend-hidden");
    fireEvent.mouseMove(chart, { clientX: 100 });
    expect(screen.getByTestId("trend-hover-summary").textContent).toBe(
      "10:15 · 请求：40 · 下载：16",
    );
  });

  it("百分比辅助序列不进入 hover 读数与图例", () => {
    renderWithProviders(
      <TrendChart
        title="占比读数"
        summary="占比不进读数"
        primary={POINTS}
        primaryLabel="已用内存"
        unit="bytes"
        yDomain={[0, 40]}
        rightPercentAxis
      />,
    );
    const chart = screen.getByRole("img", { name: "占比读数：占比不进读数" });
    fireEvent.mouseMove(chart, { clientX: 100 });
    // 读数只含主系列：透明的百分比辅助序列对用户完全不可见。
    expect(screen.getByTestId("trend-hover-summary").textContent).toBe("10:15 · 已用内存：40 B");
    // 图例同样只有主系列一项（辅助序列不进图例）。
    expect(screen.getAllByRole("button").map((node) => node.textContent)).toEqual(["已用内存"]);
  });

  it("聚焦后再次拖选按当前视图换算区间，不被全量长度带偏", () => {
    renderWithProviders(
      <TrendChart title="二级聚焦" summary="聚焦后可再选" primary={POINTS} primaryLabel="请求" />,
    );
    const chart = screen.getByRole("img", { name: "二级聚焦：聚焦后可再选" });
    mockRect(chart);

    // 首次拖选锁定 10:05 – 10:10（索引 1–2）
    fireEvent.mouseDown(chart, { clientX: 30 });
    fireEvent.mouseMove(chart, { clientX: 65 });
    fireEvent.mouseUp(chart, { clientX: 65 });
    expect(screen.getByText("已聚焦 10:05 – 10:10")).toBeTruthy();

    // 视图此时只剩 2 个点，拖满整段仍是同一区间（若按全量 4 点换算会扩到 10:00 – 10:15）
    fireEvent.mouseDown(chart, { clientX: 0 });
    fireEvent.mouseMove(chart, { clientX: 100 });
    fireEvent.mouseUp(chart, { clientX: 100 });
    expect(screen.getByText("已聚焦 10:05 – 10:10")).toBeTruthy();
  });

  it("移除统计条后横向拖选聚焦与双击还原仍可用", () => {
    renderWithProviders(
      <TrendChart
        title="拖选回归"
        summary="删条后拖选仍可聚焦"
        primary={POINTS}
        primaryLabel="请求"
      />,
    );
    const chart = screen.getByRole("img", { name: "拖选回归：删条后拖选仍可聚焦" });
    mockRect(chart);

    // 前置：统计条已不存在（本用例只验证底层聚焦能力未被删条波及）
    expect(screen.queryByText("区间剖析")).toBeNull();
    expect(screen.queryByText("均值")).toBeNull();

    // 横向拖选锁定 10:05 – 10:10（索引 1–2）：聚焦态由胶囊 + 还原入口表达
    fireEvent.mouseDown(chart, { clientX: 30 });
    fireEvent.mouseMove(chart, { clientX: 65 });
    fireEvent.mouseUp(chart, { clientX: 65 });
    expect(screen.getByText("已聚焦 10:05 – 10:10")).toBeTruthy();
    // 聚焦胶囊带一键还原入口，点击即退出聚焦
    fireEvent.click(screen.getByRole("button", { name: "退出聚焦" }));
    expect(screen.queryByText(/已聚焦/)).toBeNull();

    // 再次拖选后双击图面同样还原
    fireEvent.mouseDown(chart, { clientX: 30 });
    fireEvent.mouseMove(chart, { clientX: 65 });
    fireEvent.mouseUp(chart, { clientX: 65 });
    expect(screen.getByText("已聚焦 10:05 – 10:10")).toBeTruthy();
    fireEvent.doubleClick(chart);
    expect(screen.queryByText(/已聚焦/)).toBeNull();
  });
});

/**
 * 取各条 Y 轴的刻度文本。
 * recharts 3 把刻度标签渲染在 `recharts-yAxis-tick-labels` 容器下（X 轴是 `recharts-xAxis-tick-labels`，
 * 天然被排除）；存在右轴时按 DOM 顺序返回 `[左, 右]`，左轴在前。
 */
function yAxisTickTexts(container: HTMLElement): string[][] {
  return Array.from(container.querySelectorAll(".recharts-yAxis-tick-labels")).map((group) =>
    Array.from(group.querySelectorAll(".recharts-cartesian-axis-tick-label")).map(
      (node) => node.textContent ?? "",
    ),
  );
}

/**
 * 等图表完成测量并渲染出 Y 轴刻度。
 * ResponsiveContainer 要拿到 ResizeObserver 上报的 contentRect 才会测量容器，且尺寸提交被
 * `debounce={120}` 节流，故刻度不会在首帧出现，必须等待（拿不到就抛错让 waitFor 重试）。
 */
async function waitForYAxisTicks(container: HTMLElement): Promise<string[][]> {
  let ticks: string[][] = [];
  await waitFor(() => {
    ticks = yAxisTickTexts(container);
    if (ticks.length === 0 || (ticks[0]?.length ?? 0) === 0) throw new Error("Y 轴刻度尚未渲染");
  });
  return ticks;
}

/**
 * 固定尺寸的 ResizeObserver 替身，仅供本组用例测量图表容器。
 *
 * 全局装置（test/setup.ts）里那份 stub 的回调只传了单条 entry，而 recharts 读的是 `entries[0]`，
 * 于是 `entry == null` 直接 return、容器尺寸恒为 0×0、SVG 根本不渲染（既有缺陷，不属本任务范围）。
 * 这里就地补一个回调参数形状正确的替身，用完还原，不影响本文件其它用例。
 */
class FixedSizeResizeObserver implements ResizeObserver {
  constructor(private readonly callback: ResizeObserverCallback) {}

  observe(): void {
    const rect = {
      x: 0,
      y: 0,
      top: 0,
      left: 0,
      bottom: 300,
      right: 800,
      width: 800,
      height: 300,
      toJSON: () => ({}),
    } as DOMRectReadOnly;
    const entry = {
      target: null,
      contentRect: rect,
      borderBoxSize: [{ inlineSize: 800, blockSize: 300 }],
      contentBoxSize: [{ inlineSize: 800, blockSize: 300 }],
      devicePixelContentBoxSize: [{ inlineSize: 800, blockSize: 300 }],
    } as unknown as ResizeObserverEntry;
    setTimeout(() => this.callback([entry], this), 0);
  }

  unobserve(): void {}

  disconnect(): void {}
}

describe("趋势图 Y 轴自定义域", () => {
  const originalResizeObserver = globalThis.ResizeObserver;

  beforeAll(() => {
    globalThis.ResizeObserver = FixedSizeResizeObserver as unknown as typeof ResizeObserver;
  });

  afterAll(() => {
    globalThis.ResizeObserver = originalResizeObserver;
  });

  it("不传 yDomain 时保持 recharts 默认轴域（自 0 起），渲染结果与支持该能力前一致", async () => {
    const { container } = renderWithProviders(
      <TrendChart title="默认轴域" summary="未指定轴域" primary={POINTS} primaryLabel="请求" />,
    );

    const [left] = await waitForYAxisTicks(container);
    expect(left).toEqual(["0", "10", "20", "30", "40"]);
  });

  it("固定上界 yDomain=[0, total] 时左轴按该上界铺满刻度", async () => {
    const { container } = renderWithProviders(
      <TrendChart
        title="固定上界"
        summary="轴顶为总量"
        primary={POINTS}
        primaryLabel="已用内存"
        unit="bytes"
        yDomain={[0, 100 * 1024 * 1024]}
      />,
    );

    const [left] = await waitForYAxisTicks(container);
    expect(left?.[0]).toBe("0 B");
    // 上界即域顶：刻度按 unit="bytes" 格式化为 100.0 MB
    expect(left?.[left.length - 1]).toBe("100.0 MB");
  });

  it('自动放大 yDomain=["auto", "auto"] 时左轴不再从 0 起', async () => {
    const { container } = renderWithProviders(
      <TrendChart
        title="自动轴域"
        summary="由数据决定轴域"
        primary={POINTS}
        primaryLabel="请求"
        yDomain={["auto", "auto"]}
      />,
    );

    const [left] = await waitForYAxisTicks(container);
    expect(left).not.toContain("0");
    const first = Number(left?.[0]);
    const last = Number(left?.[left.length - 1]);
    expect(first).toBeGreaterThan(0);
    expect(last).toBeGreaterThanOrEqual(40);
  });

  it("函数对 yDomain 时按函数求值后的上下界渲染刻度", async () => {
    const { container } = renderWithProviders(
      <TrendChart
        title="函数轴域"
        summary="带内边距的轴域"
        primary={POINTS}
        primaryLabel="请求"
        yDomain={[(min: number) => min * 0.98, (max: number) => max * 1.02]}
      />,
    );

    const [left] = await waitForYAxisTicks(container);
    // 数据 10–40 → 下界 9.8、上界 40.8
    expect(Number(left?.[0])).toBeCloseTo(9.8, 5);
    expect(Number(left?.[left.length - 1])).toBeCloseTo(40.8, 5);
  });

  it("传 yDomain 只作用于左轴，右轴（第三系列）既有域逻辑不变", async () => {
    const { container } = renderWithProviders(
      <TrendChart
        title="双轴"
        summary="仅左轴可配域"
        primary={POINTS}
        primaryLabel="请求"
        tertiary={POINTS.map((point) => ({ ...point, value: point.value / 2 }))}
        tertiaryLabel="失败"
        yDomain={[0, 100]}
      />,
    );

    const [left, right] = await waitForYAxisTicks(container);
    // 左轴跟随 yDomain
    expect(left?.[left.length - 1]).toBe("100");
    // 右轴仍是自身兜底域 [0, ceil(dataMax)]：第三系列最大 20
    expect(right?.[0]).toBe("0");
    expect(right?.[right.length - 1]).toBe("20");
  });

  it("rightPercentAxis 且 yDomain=[0, max] 时渲染右侧百分比轴（覆盖 domain 与 formatter）", async () => {
    const { container } = renderWithProviders(
      <TrendChart
        title="占比轴"
        summary="右轴读占比"
        primary={POINTS}
        primaryLabel="已用内存"
        unit="bytes"
        yDomain={[0, 40]}
        rightPercentAxis
      />,
    );

    const axes = await waitForYAxisTicks(container);
    // 出现两条 Y 轴：左轴（字节口径）+ 右轴（百分比口径）。
    expect(axes.length).toBe(2);
    const left = axes[0]!;
    const right = axes[1]!;
    // 左轴仍是字节口径，未被百分比轴影响。
    expect(left[left.length - 1]).toBe("40 B");
    // 右轴每个刻度都带 %（formatter），两端恰为 0% / 100%（domain=[0,100]）。
    expect(right.every((tick) => tick.endsWith("%"))).toBe(true);
    expect(right[0]).toBe("0%");
    expect(right[right.length - 1]).toBe("100%");
  });

  it('rightPercentAxis 遇 yDomain="auto" 或未传 yDomain 时不渲染百分比轴', async () => {
    const auto = renderWithProviders(
      <TrendChart
        title="自适应轴域不配占比"
        summary="无具体上界"
        primary={POINTS}
        primaryLabel="请求"
        yDomain={["auto", "auto"]}
        rightPercentAxis
      />,
    );
    // 上界不确定 ⇒ 不猜占比，只有左轴一条。
    expect((await waitForYAxisTicks(auto.container)).length).toBe(1);

    const none = renderWithProviders(
      <TrendChart
        title="缺省轴域不配占比"
        summary="未传 yDomain"
        primary={POINTS}
        primaryLabel="请求"
        rightPercentAxis
      />,
    );
    expect((await waitForYAxisTicks(none.container)).length).toBe(1);
  });

  it("rightPercentAxis 与 tertiary 同传时以 tertiary 右轴优先，不叠加百分比轴", async () => {
    const { container } = renderWithProviders(
      <TrendChart
        title="占比与第三系列"
        summary="tertiary 优先"
        primary={POINTS}
        primaryLabel="请求"
        tertiary={POINTS.map((point) => ({ ...point, value: point.value / 2 }))}
        tertiaryLabel="失败"
        yDomain={[0, 40]}
        rightPercentAxis
      />,
    );

    const axes = await waitForYAxisTicks(container);
    expect(axes.length).toBe(2);
    const right = axes[1]!;
    // 右轴仍是 tertiary 的数值轴（第三系列最大 20），不是百分比轴。
    expect(right[right.length - 1]).toBe("20");
    expect(right.some((tick) => tick.endsWith("%"))).toBe(false);
  });

  it("右轴刻度按 unit 换算（不再显示原始字节数）", async () => {
    const GB = 1024 ** 3;
    const { container } = renderWithProviders(
      <TrendChart
        title="右轴换算"
        summary="右轴刻度也应换算"
        primary={POINTS}
        primaryLabel="可用"
        tertiary={[
          { label: "10:00", value: 100 * GB },
          { label: "10:05", value: 100 * GB },
        ]}
        tertiaryLabel="总量"
        unit="bytes"
      />,
    );

    const [, right] = await waitForYAxisTicks(container);
    // 右轴每个刻度都应是人类可读体积（如 "93.1 GB"），而非 107374182400 这类原始字节数。
    const byteTick = /^-?\d+(\.\d+)? (B|KB|MB|GB|TB)$/;
    expect(right!.length).toBeGreaterThan(0);
    expect(right!.every((tick) => byteTick.test(tick))).toBe(true);
    expect(right!.some((tick) => tick.endsWith("GB"))).toBe(true);
  });
});
