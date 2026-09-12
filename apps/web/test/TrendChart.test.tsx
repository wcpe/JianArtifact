// 趋势图单元测试：覆盖区间剖析口径、键盘聚焦可达性、单位格式化与二次聚焦的索引换算。
import { fireEvent, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

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
  it("累计型指标在剖析条给出总量口径", () => {
    renderWithProviders(
      <TrendChart
        title="请求趋势"
        summary="请求量随时间的变化"
        primary={POINTS}
        primaryLabel="请求"
      />,
    );

    // 10 + 20 + 30 + 40 = 100
    expect(screen.getByText("总量")).toBeTruthy();
    expect(screen.getByText("100")).toBeTruthy();
  });

  it("百分比单位在轴与剖析读数上带 % 后缀，且不展示无意义的总量", () => {
    renderWithProviders(
      <TrendChart
        title="CPU 趋势"
        summary="CPU 使用率随时间的变化"
        primary={POINTS}
        primaryLabel="CPU 使用率"
        unit="percent"
      />,
    );

    // 均值 (10+20+30+40)/4 = 25 → 25.0%
    expect(screen.getByText("25.0%")).toBeTruthy();
    // 百分比是瞬时量，求和无意义
    expect(screen.queryByText("总量")).toBeNull();
  });

  it("字节单位不展示总量，而是走人类可读体积", () => {
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

    expect(screen.queryByText("总量")).toBeNull();
    // 均值 1536 B → 1.5 KB
    expect(screen.getByText("1.5 KB")).toBeTruthy();
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
});
