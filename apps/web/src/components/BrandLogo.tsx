// 品牌 logo 矢量图与品牌色常量：供控制台外壳（AppLayout）与初始化页（SetupPage）共用，
// 避免同一 SVG / 魔法色值散落多处。viewBox 24×24，纯内联 SVG（无外部资源）。

/** 品牌蓝（logo 主色）。 */
export const BRAND_BLUE = "#228be6";
/** 品牌浅蓝（立方体 / 包裹线稿描边）。 */
export const BRAND_BLUE_LIGHT = "#a5d8ff";

/**
 * 品牌 logo：蓝底圆角方块 + 浅蓝立方体 / 包裹线稿，寓意「制品 / 打包」。
 * 尺寸由调用方通过 size 控制。
 */
export function BrandLogo({ size = 28 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      xmlns="http://www.w3.org/2000/svg"
      aria-hidden="true"
      focusable="false"
    >
      <rect x="1.5" y="1.5" width="21" height="21" rx="5" fill={BRAND_BLUE} />
      <path
        d="M12 5.5 L17.5 8.5 L12 11.5 L6.5 8.5 Z"
        stroke={BRAND_BLUE_LIGHT}
        strokeWidth="1.4"
        strokeLinejoin="round"
        fill="none"
      />
      <path
        d="M6.5 8.5 L6.5 15 L12 18 L17.5 15 L17.5 8.5"
        stroke={BRAND_BLUE_LIGHT}
        strokeWidth="1.4"
        strokeLinejoin="round"
        fill="none"
      />
      <path d="M12 11.5 L12 18" stroke={BRAND_BLUE_LIGHT} strokeWidth="1.4" />
    </svg>
  );
}
