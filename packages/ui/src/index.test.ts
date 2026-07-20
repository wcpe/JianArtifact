import { describe, expect, it } from "vitest";

import { brandColor, tokens } from "./index";

describe("设计令牌", () => {
  it("品牌色为合法十六进制", () => {
    expect(brandColor).toMatch(/^#[0-9a-f]{6}$/i);
  });

  it("间距令牌单调递增", () => {
    const { xs, sm, md, lg } = tokens.spacing;
    expect(xs).toBeLessThan(sm);
    expect(sm).toBeLessThan(md);
    expect(md).toBeLessThan(lg);
  });
});
