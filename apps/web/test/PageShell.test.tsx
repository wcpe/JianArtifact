// PageShell 矮视口回归守卫。
//
// 背景：PageShell 的高度口径是 calc(100dvh − 页眉 − 内边距)，但 min-height 曾写死 480px——
// 视口可用高度低于 480 时（开着 DevTools / 小窗 / 分屏），min-height 反而把外壳撑出视口，
// 出现「整页滚 + 内容区内部滚」的双重滚动（线上实测 1281×420 视口整页溢出 140px）。
// 修复：min-height 取「480 下限」与「可视高度」的较小者（CSS min()），两侧意图都保留。
import { describe, expect, it } from "vitest";

import { PageShell, PAGE_VIEWPORT_HEIGHT } from "../src/app/PageShell";
import { renderWithProviders } from "./harness";

describe("PageShell", () => {
  it("min-height 不高于可视高度（矮视口不撑出外壳）", () => {
    const { container } = renderWithProviders(<PageShell testId="shell">内容</PageShell>);
    const shell = container.querySelector('[data-testid="shell"]') as HTMLElement;

    expect(shell.style.height).toBe(PAGE_VIEWPORT_HEIGHT);
    expect(shell.style.minHeight).toBe(`min(480px, ${PAGE_VIEWPORT_HEIGHT})`);
  });

  it("显式 minHeight 同样受可视高度上限约束", () => {
    const { container } = renderWithProviders(
      <PageShell testId="shell" minHeight={320}>
        内容
      </PageShell>,
    );
    const shell = container.querySelector('[data-testid="shell"]') as HTMLElement;

    expect(shell.style.minHeight).toBe(`min(320px, ${PAGE_VIEWPORT_HEIGHT})`);
  });
});
