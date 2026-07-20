// @jianartifact/ui —— 共享设计令牌与主题常量入口。
// 0.1.0 仅提供与框架无关的设计令牌；Mantine 组件与主题随 0.2.0 管理端骨架迁入。

/** 品牌主色（十六进制），管理端与验收站共用。 */
export const brandColor = "#3b5bdb" as const;

/** 设计令牌：间距、圆角与品牌色，供各前端应用消费。 */
export const tokens = {
  brandColor,
  radius: {
    sm: 4,
    md: 8,
    lg: 16,
  },
  spacing: {
    xs: 4,
    sm: 8,
    md: 16,
    lg: 24,
  },
} as const;

export type Tokens = typeof tokens;
