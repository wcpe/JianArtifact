// 固定视口页面外壳：列表页 / 工作台类页面的统一容器。
//
// 目的：让"页头 + 工具行固定，主体在视口内滚动"成为唯一范式，页面不再各自手算高度。
//
// 高度口径（全站唯一真源）：
//   calc(100dvh - var(--app-shell-header-offset) - 2 × var(--app-shell-padding))
// 与 Mantine AppShell.Main 自身的 padding-top（header-offset + padding）严格同源，
// 因此内容区可用高度恰好等于视口减去页眉与上下内边距。
//
// 本次收敛修掉的三类历史坑：
// 1) vh / dvh 混用——移动端地址栏展开时 100vh > 可视高度，容器溢出、底部内容被截；
//    统一用 dvh（动态视口高度）。
// 2) header-height / header-offset 混用——两者当前同值，但 header.offset=false 或
//    移动端 header 收起时只有 offset 会归零，混用即高度错位；统一用 offset。
// 3) minHeight 有无不一——无兜底时长工具行一换行就把主体压到 0 高；统一给下限。
import { Box } from "@mantine/core";
import type { CSSProperties, ReactNode } from "react";

import { density } from "../theme/density";

/** 内容区最大宽度：与 density 同源，各页不得再自造 maw。 */
export const CONTENT_MAX_WIDTH = density.contentMaxWidth;

/** 固定视口高度表达式：与 AppShell.Main 的内边距同源，页面无需各自重算。 */
export const PAGE_VIEWPORT_HEIGHT =
  "calc(100dvh - var(--app-shell-header-offset, 56px) - 2 * var(--app-shell-padding, 12px))";

/** 主体最小高度：低于此值说明布局已被压扁，宁可让外壳滚动也不截断内容。 */
export const PAGE_MIN_HEIGHT = 480;

export interface PageShellProps {
  children: ReactNode;
  /** 纵向间距；列表页通常为 0（自行控制分区间距），工作台类页面用 12 / 20。 */
  gap?: number;
  /** 主体最小高度，默认 480px。 */
  minHeight?: number;
  /** 测试定位用；保持与既有 testid 一致，避免改布局连带改测试。 */
  testId?: string;
  style?: CSSProperties;
}

/**
 * 固定视口外壳：撑满内容区可用高度，内部纵向排布、整体不滚动，
 * 由子分区各自 `overflow: auto` 承担滚动。
 */
export function PageShell({
  children,
  gap = 0,
  minHeight = PAGE_MIN_HEIGHT,
  testId,
  style,
}: PageShellProps) {
  return (
    <Box
      data-testid={testId}
      style={{
        height: PAGE_VIEWPORT_HEIGHT,
        // 下限 480px，但**不得高于可视高度**：矮视口（开着 DevTools / 小窗 / 分屏）下
        // 可用高度不足 480 时，min-height 会把外壳撑出视口，出现「外壳滚 + 内部区滚」
        // 的双重滚动；用 CSS min() 取「480 下限」与「可视高度」的较小者——
        // 视口足够时仍是 480 的原有兜底（长工具行换行不把主体压扁），矮视口则不再溢出。
        minHeight: `min(${minHeight}px, ${PAGE_VIEWPORT_HEIGHT})`,
        display: "flex",
        flexDirection: "column",
        gap,
        overflow: "hidden",
        ...style,
      }}
    >
      {children}
    </Box>
  );
}
