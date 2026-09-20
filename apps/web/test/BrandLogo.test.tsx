import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { BrandLogo } from "../src/components/BrandLogo";

describe("BrandLogo", () => {
  it("使用由 Vite 管理的品牌图标资源", () => {
    render(<BrandLogo />);

    // vite 7 的 vitest 环境会把小资源内联成 data URI（vite 5 时返回 /src/assets/ 原路径）。
    // 两种形态都证明走了 vite 的 import 打包管线；若有人改回 public/ 直接引用（/favicon.svg），
    // 两个分支都不会命中，用例失败。
    const src = screen.getByAltText("JianArtifact").getAttribute("src") ?? "";
    const inlined = src.startsWith("data:image/svg+xml");
    const managed = src.startsWith("/src/assets/");
    expect(inlined || managed).toBe(true);
  });
});
