import faviconUrl from "../assets/favicon.svg";

// 品牌图标由 Vite 打包管理，供控制台外壳与初始化页共用。
export function BrandLogo({ size = 28 }: { size?: number }) {
  return (
    <img
      src={faviconUrl}
      width={size}
      height={size}
      alt="JianArtifact"
      aria-hidden="true"
      draggable={false}
      style={{ display: "block" }}
    />
  );
}
