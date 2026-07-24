import { describe, expect, it } from "vitest";

import { buildAssetTree, assetDownloadUrl, formatBytes } from "./assetTree";

describe("buildAssetTree", () => {
  it("拼目录优先树且文件挂叶子", () => {
    const tree = buildAssetTree([
      {
        path: "a/b.txt",
        size: 1,
        hash: "h1",
        updatedAt: "t",
      },
      {
        path: "a/c/d.bin",
        size: 2,
        hash: "h2",
        updatedAt: "t",
      },
      {
        path: "z.dat",
        size: 3,
        hash: "h3",
        updatedAt: "t",
      },
    ]);
    expect(tree.map((n) => n.name)).toEqual(["a", "z.dat"]);
    expect(tree[0]?.kind).toBe("dir");
    expect(tree[1]?.kind).toBe("file");
    const a = tree[0]!;
    expect(a.children?.map((c) => c.name)).toEqual(["c", "b.txt"]);
    const c = a.children?.find((x) => x.name === "c");
    expect(c?.children?.[0]?.path).toBe("a/c/d.bin");
    expect(c?.children?.[0]?.asset?.hash).toBe("h2");
  });
});

describe("assetDownloadUrl", () => {
  it("raw 与 npm 路径不同", () => {
    expect(assetDownloadUrl("r", "raw", "x/y")).toBe("/repository/r/x/y");
    expect(assetDownloadUrl("r", "npm", "x/y")).toBe("/npm/r/x/y");
  });
});

describe("formatBytes", () => {
  it("格式化字节", () => {
    expect(formatBytes(500)).toBe("500 B");
    expect(formatBytes(2048)).toBe("2.0 KB");
  });
});
