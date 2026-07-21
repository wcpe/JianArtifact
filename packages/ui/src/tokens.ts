// 设计令牌：与框架无关的品牌色、间距与圆角常量。
// 独立成模块，避免主题 / 组件与聚合入口（index.ts）互相引用形成循环。

/** 品牌主色（十六进制），管理端与验收站共用；对齐旧项目 logo 品牌蓝。 */
export const brandColor = "#228be6" as const;

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
