// 信息密度基线：集中约定全站的间距 / 卡片瘦身 / 栅格 gap 等密度 token，
// 供外壳与各页面引用，避免魔法值散落。取值沿用旧项目控制台密度基线。
import type { MantineSpacing } from "@mantine/core";

/** 全站密度基线 token。 */
export const density = {
  /** 折叠图标导航条宽度：窄态仅容图标，宽态容图标+文字。 */
  navbarWidth: {
    collapsed: 64,
    expanded: 240,
  },
  /** 内容区主 padding：较默认 md 收紧为 sm，提升信息密度。 */
  mainPadding: "sm" as MantineSpacing,
  /** 页眉高度（px）：单一真源，供 AppShell.Header 高度与页内 sticky 元素共用。 */
  headerHeight: 56,
  /** 内容区最大宽度（px）：内容居中并限定最大宽度，避免新内容出现时把整体布局撑变形。 */
  contentMaxWidth: 1280,
  /** 卡片内边距：由 lg 收紧为 md，卡片瘦身。 */
  cardPadding: "md" as MantineSpacing,
  /** 栅格 / 堆叠间距：默认收紧为 sm，避免一味纵向铺开。 */
  gridSpacing: "sm" as MantineSpacing,
  /** 紧凑徽章 / 内联元素间距。 */
  inlineGap: "xs" as MantineSpacing,
} as const;
