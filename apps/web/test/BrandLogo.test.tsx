import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { BrandLogo } from "../src/components/BrandLogo";

describe("BrandLogo", () => {
  it("使用由 Vite 管理的品牌图标资源", () => {
    render(<BrandLogo />);

    expect(screen.getByAltText("JianArtifact").getAttribute("src")).toBe("/src/assets/favicon.svg");
  });
});
