// 信息密度基线（FR-92）：集中约定全站的间距 / 卡片瘦身 / 栅格 gap 等密度 token，
// 供 shell 外壳与各页面引用，避免魔法值散落、便于后续 FR 逐页对齐同一密度。
//
// 取值取 Mantine 内置间距档（xs/sm/md/lg/xl）的字符串名，直接传给组件的
// padding / gap / spacing 等属性；本 FR 只在 shell + 仪表盘落地示范，
// 其余页面的密度细化交后续 FR 跟进（FR-93/94/96/99）。

import type { MantineSpacing } from '@mantine/core';

/** 全站密度基线 token。 */
export const density = {
  /** 折叠图标导航条宽度：窄态仅容图标，宽态容图标+文字。 */
  navbarWidth: {
    collapsed: 64,
    expanded: 240,
  },
  /** 内容区主 padding：较默认 md 收紧为 sm，提升信息密度。 */
  mainPadding: 'sm' as MantineSpacing,
  /**
   * 页眉高度（FR-92 alt 外壳）：单一真源，供 AppShell.Header 高度与页内 sticky 元素的
   * 顶部偏移共用。alt 布局下页眉 `position: fixed` 覆盖视口顶部，页内 sticky 锚点须以此为
   * `top` 偏移、避免被固定页眉遮住（修 FR-92 后设置页锚点导航 sticky 失效）。单位 px。
   */
  headerHeight: 56,
  /**
   * 内容区最大宽度（FR-92）：内容居中并限定最大宽度，使卡片 / 新内容出现时
   * 不再把整体布局撑变形（用户反馈「出来个东西就变形」）。单位 px。
   */
  contentMaxWidth: 1280,
  /** 卡片内边距：由 lg 收紧为 md，卡片瘦身。 */
  cardPadding: 'md' as MantineSpacing,
  /** 栅格 / 堆叠间距：默认收紧为 sm，避免一味纵向铺开。 */
  gridSpacing: 'sm' as MantineSpacing,
  /** 紧凑徽章 / 内联元素间距。 */
  inlineGap: 'xs' as MantineSpacing,
} as const;
