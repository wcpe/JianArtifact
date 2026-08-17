// 品牌 logo：引用公共图标文件 public/favicon.svg（与浏览器 favicon 同一图形）。
// 不做内联 SVG，单一文件来源；供控制台外壳（AppLayout）与初始化页（SetupPage）共用。
export function BrandLogo({ size = 28 }: { size?: number }) {
  return (
    <img
      src="/favicon.svg"
      width={size}
      height={size}
      alt="JianArtifact"
      aria-hidden="true"
      draggable={false}
      style={{ display: "block" }}
    />
  );
}
