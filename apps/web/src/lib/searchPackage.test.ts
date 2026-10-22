// searchPackage.ts 单测：精度徽标映射 / 命中词高亮切分 / fileKind chip 与 `+N` 折叠 /
// 非 relevance 的前端重排 / 主从联动的默认展开与选中解析。
// 这些是里唯一"有分支"的纯逻辑，页面只做渲染，因此重点压在这里。
import { describe, expect, it } from "vitest";

import type { SearchPackage, SearchPackageFile } from "../api/endpoints";
import {
  compileHighlightPattern,
  defaultOrderFor,
  fileKindChips,
  fileKey,
  firstFileOfPackage,
  initialViewState,
  packageKey,
  precisionMeta,
  resolveSelection,
  sortPackages,
  splitChecksumFiles,
  splitHighlight,
  splitHighlightWithPattern,
  toggleInSet,
  versionKey,
} from "./searchPackage";

function file(overrides: Partial<SearchPackageFile>): SearchPackageFile {
  return {
    name: "foo-1.0.0.jar",
    path: "com/example/foo/1.0.0/foo-1.0.0.jar",
    repository: "maven-releases",
    size: 100,
    sha1: "abc",
    updatedAt: "2026-10-18 09:12:33",
    fileKind: "jar",
    ...overrides,
  };
}

function pkg(overrides: Partial<SearchPackage>): SearchPackage {
  return {
    coordinate: "com.example:foo",
    kind: "maven",
    repositories: ["maven-releases"],
    versionCount: 1,
    fileCount: 1,
    bestPrecision: "exact",
    latestUpdatedAt: "2026-10-18 09:12:33",
    topVersion: "1.0.0",
    versions: [
      {
        version: "1.0.0",
        precision: "exact",
        fileCount: 1,
        size: 100,
        latestUpdatedAt: "2026-10-18 09:12:33",
        files: [file({})],
      },
    ],
    ...overrides,
  };
}

describe("precisionMeta 精度徽标映射", () => {
  it("三档有信号的精度各自映射到独立文案键与颜色", () => {
    expect(precisionMeta("exact")).toEqual({ labelKey: "search.precisionExact", color: "teal" });
    expect(precisionMeta("prerelease")).toEqual({
      labelKey: "search.precisionPrerelease",
      color: "yellow",
    });
    // 子串不是错误，用中性灰而不是警示色。
    expect(precisionMeta("substring")).toEqual({
      labelKey: "search.precisionSubstring",
      color: "gray",
    });
  });

  it("none 与未知取值都不显示徽标（labelKey 为 null）", () => {
    expect(precisionMeta("none").labelKey).toBeNull();
    expect(precisionMeta("whatever-the-backend-adds").labelKey).toBeNull();
    expect(precisionMeta(null).labelKey).toBeNull();
    expect(precisionMeta(undefined).labelKey).toBeNull();
  });
});

describe("splitHighlight 命中词高亮切分", () => {
  it("无关键词或无命中时原样返回单个片段", () => {
    expect(splitHighlight("foo-1.17.12.jar", [])).toEqual([
      { text: "foo-1.17.12.jar", hit: false },
    ]);
    expect(splitHighlight("foo-1.17.12.jar", ["1.18"])).toEqual([
      { text: "foo-1.17.12.jar", hit: false },
    ]);
  });

  it("大小写不敏感匹配，但保留原文大小写", () => {
    expect(splitHighlight("Foo-1.17.12.JAR", ["foo"])).toEqual([
      { text: "Foo", hit: true },
      { text: "-1.17.12.JAR", hit: false },
    ]);
  });

  it("多个关键词各自高亮，重叠段合并为一段（不嵌套 mark）", () => {
    const segments = splitHighlight("com.example:foo", ["example", "foo"]);
    expect(segments).toEqual([
      { text: "com.", hit: false },
      { text: "example", hit: true },
      { text: ":", hit: false },
      { text: "foo", hit: true },
    ]);

    // 长词优先：同时搜 1.17 与 1.17.12 时整段一起高亮。
    const overlapping = splitHighlight("foo-1.17.12.jar", ["1.17", "1.17.12"]);
    expect(overlapping.filter((segment) => segment.hit)).toEqual([{ text: "1.17.12", hit: true }]);
  });

  it("正则元字符按字面量匹配（点号不是通配）", () => {
    expect(splitHighlight("a1b17c12", ["1.17.12"])).toEqual([{ text: "a1b17c12", hit: false }]);
  });

  it("命中在开头/结尾时不会产生空片段", () => {
    expect(splitHighlight("1.17.12", ["1.17.12"])).toEqual([{ text: "1.17.12", hit: true }]);
  });
});

describe("预编译高亮正则 compileHighlightPattern", () => {
  it("无有效关键词时返回 null（调用方据此跳过匹配）", () => {
    expect(compileHighlightPattern([])).toBeNull();
    expect(compileHighlightPattern(["  ", ""])).toBeNull();
  });

  it("同一批关键词只编译一次，可被多行共享且结果与 splitHighlight 一致", () => {
    const pattern = compileHighlightPattern(["1.17", "1.17.12"]);
    expect(pattern).not.toBeNull();

    // 逐行复用同一个实例：matchAll 不会改动它的 lastIndex，因此结果是稳定的。
    for (let i = 0; i < 3; i += 1) {
      expect(splitHighlightWithPattern("foo-1.17.12.jar", pattern)).toEqual(
        splitHighlight("foo-1.17.12.jar", ["1.17", "1.17.12"]),
      );
    }
  });

  it("pattern 为 null 时原样返回单个片段", () => {
    expect(splitHighlightWithPattern("foo.jar", null)).toEqual([{ text: "foo.jar", hit: false }]);
  });
});

describe("fileKindChips 类型 chip 与 +N 折叠", () => {
  it("按附录 A 优先级取前 4 个，其余折叠为 overflow 计数", () => {
    const chips = fileKindChips([
      file({ fileKind: "javadoc" }),
      file({ fileKind: "jar" }),
      file({ fileKind: "tests" }),
      file({ fileKind: "pom" }),
      file({ fileKind: "sources" }),
    ]);
    // jar(0) → sources(2) → javadoc(3) → tests(4)，pom 折进 +1。
    expect(chips.kinds).toEqual(["jar", "sources", "javadoc", "tests"]);
    expect(chips.overflow).toBe(1);
  });

  it("校验和不占 chip 位（它已有独立的「校验和 (N)」折叠行）", () => {
    const chips = fileKindChips([
      file({ fileKind: "jar" }),
      file({ fileKind: "checksum" }),
      file({ fileKind: "checksum" }),
    ]);
    expect(chips.kinds).toEqual(["jar"]);
    expect(chips.overflow).toBe(0);
  });

  it("未知类型排在 other 之后但仍在校验和之前", () => {
    const chips = fileKindChips([
      file({ fileKind: "checksum" }),
      file({ fileKind: "weird-kind" }),
      file({ fileKind: "other" }),
    ]);
    expect(chips.kinds).toEqual(["other", "weird-kind"]);
  });

  it("同类型只算一个 chip", () => {
    const chips = fileKindChips([file({ fileKind: "jar" }), file({ fileKind: "jar" })]);
    expect(chips).toEqual({ kinds: ["jar"], overflow: 0 });
  });
});

describe("splitChecksumFiles 校验和分组", () => {
  it("按 fileKind 分出正文文件与校验和文件，保持原顺序", () => {
    const jar = file({ fileKind: "jar", name: "a.jar" });
    const sha1 = file({ fileKind: "checksum", name: "a.jar.sha1" });
    const pom = file({ fileKind: "pom", name: "a.pom" });
    const result = splitChecksumFiles([jar, sha1, pom]);
    expect(result.primary).toEqual([jar, pom]);
    expect(result.checksums).toEqual([sha1]);
  });
});

describe("packageKey / versionKey / fileKey 稳定性", () => {
  it("包键含 kind，同坐标不同协议不会混为一谈", () => {
    expect(packageKey({ kind: "maven", coordinate: "a:b" })).toBe("maven:a:b");
    expect(packageKey({ kind: "file", coordinate: "raw-repo" })).toBe("file:raw-repo");
    expect(versionKey("maven:a:b", "1.0.0")).toBe("maven:a:b::1.0.0");
    expect(fileKey({ repository: "r", path: "a/b.jar" })).toBe("r/a/b.jar");
  });

  it("toggleInSet 返回新集合，不原地修改", () => {
    const source = new Set(["a"]);
    const added = toggleInSet(source, "b");
    expect([...added]).toEqual(["a", "b"]);
    expect([...source]).toEqual(["a"]);
    expect([...toggleInSet(added, "a")]).toEqual(["b"]);
  });
});

describe("sortPackages 非 relevance 的前端重排", () => {
  // 三个包，首文件的名字/大小/时间各不相同，便于逐键断言。
  const packages: SearchPackage[] = [
    pkg({
      coordinate: "com.example:charlie",
      versions: [
        {
          version: "2.0.0",
          precision: "none",
          fileCount: 1,
          size: 300,
          latestUpdatedAt: "2026-10-20 00:00:00",
          files: [
            file({
              name: "charlie.jar",
              path: "c/charlie.jar",
              size: 300,
              updatedAt: "2026-10-20 00:00:00",
            }),
          ],
        },
      ],
    }),
    pkg({
      coordinate: "com.example:alpha",
      versions: [
        {
          version: "1.0.0",
          precision: "exact",
          fileCount: 1,
          size: 100,
          latestUpdatedAt: "2026-10-18 00:00:00",
          files: [
            file({
              name: "alpha.jar",
              path: "a/alpha.jar",
              size: 100,
              updatedAt: "2026-10-18 00:00:00",
            }),
          ],
        },
      ],
    }),
    pkg({
      coordinate: "com.example:bravo",
      versions: [
        {
          version: "3.0.0",
          precision: "none",
          fileCount: 1,
          size: 200,
          latestUpdatedAt: "2026-10-19 00:00:00",
          files: [
            file({
              name: "bravo.jar",
              path: "b/bravo.jar",
              size: 200,
              updatedAt: "2026-10-19 00:00:00",
            }),
          ],
        },
      ],
    }),
  ];

  it("relevance 保持后端顺序，但不交出只读入参的原引用", () => {
    const sorted = sortPackages(packages, "relevance", "relevance");
    // 顺序原样：相关度序由后端给定，前端不重排。
    expect(sorted).toEqual(packages);
    // 但必须是另一份数组：入参是 readonly，直接把原引用交出去会让调用方的任何就地
    // mutate 改到 asyncCache 里的缓存对象。这里锁住「浅拷贝后返回」的约定。
    expect(sorted).not.toBe(packages);
    sorted.push(sorted[0]!);
    expect(packages).toHaveLength(3);
  });

  it("按名称升序 / 降序", () => {
    expect(sortPackages(packages, "name", "asc").map((p) => p.coordinate)).toEqual([
      "com.example:alpha",
      "com.example:bravo",
      "com.example:charlie",
    ]);
    expect(sortPackages(packages, "name", "desc").map((p) => p.coordinate)).toEqual([
      "com.example:charlie",
      "com.example:bravo",
      "com.example:alpha",
    ]);
  });

  it("按大小与修改时间（新的/大的在前）", () => {
    expect(sortPackages(packages, "size", "desc").map((p) => p.coordinate)).toEqual([
      "com.example:charlie",
      "com.example:bravo",
      "com.example:alpha",
    ]);
    expect(sortPackages(packages, "updated", "desc").map((p) => p.coordinate)).toEqual([
      "com.example:charlie",
      "com.example:bravo",
      "com.example:alpha",
    ]);
  });

  it("版本组内部也按所选键重排，且不修改入参", () => {
    const multi = pkg({
      coordinate: "com.example:multi",
      versions: [
        {
          version: "1.0.0",
          precision: "exact",
          fileCount: 2,
          size: 30,
          latestUpdatedAt: "2026-10-18 00:00:00",
          files: [
            file({ name: "z.jar", path: "z.jar", size: 10 }),
            file({ name: "a.jar", path: "a.jar", size: 20 }),
          ],
        },
        {
          version: "2.0.0",
          precision: "exact",
          fileCount: 1,
          size: 5,
          latestUpdatedAt: "2026-10-19 00:00:00",
          files: [file({ name: "m.jar", path: "m.jar", size: 5 })],
        },
      ],
    });

    const sorted = sortPackages([multi], "name", "asc");
    // 代表文件 a.jar < m.jar，故 1.0.0 组在前。
    expect(sorted[0]!.versions.map((v) => v.version)).toEqual(["1.0.0", "2.0.0"]);
    expect(sorted[0]!.versions[0]!.files.map((f) => f.name)).toEqual(["a.jar", "z.jar"]);
    // 入参未被原地排序。
    expect(multi.versions[0]!.files.map((f) => f.name)).toEqual(["z.jar", "a.jar"]);
  });

  it("无坐标伪包按包级 files 参与重排", () => {
    const pseudoA = pkg({
      coordinate: "raw-b",
      kind: "file",
      versionCount: 0,
      versions: [],
      files: [file({ name: "b.txt", path: "docs/b.txt", fileKind: "other" })],
    });
    const pseudoB = pkg({
      coordinate: "raw-a",
      kind: "file",
      versionCount: 0,
      versions: [],
      files: [file({ name: "a.txt", path: "docs/a.txt", fileKind: "other" })],
    });
    expect(sortPackages([pseudoA, pseudoB], "name", "asc").map((p) => p.coordinate)).toEqual([
      "raw-a",
      "raw-b",
    ]);
  });
});

describe("defaultOrderFor 排序键的默认方向", () => {
  it("相关度没有方向；名称/仓库/路径升序；大小/时间倒序", () => {
    expect(defaultOrderFor("relevance")).toBe("relevance");
    expect(defaultOrderFor("name")).toBe("asc");
    expect(defaultOrderFor("repo")).toBe("asc");
    expect(defaultOrderFor("path")).toBe("asc");
    expect(defaultOrderFor("size")).toBe("desc");
    expect(defaultOrderFor("updated")).toBe("desc");
  });
});

describe("initialViewState 默认展开与选中", () => {
  it("展开首个包、展开并选中其首版本、选中该版本首个文件", () => {
    const first = pkg({
      coordinate: "com.example:foo",
      versions: [
        {
          version: "1.17.12",
          precision: "exact",
          fileCount: 2,
          size: 200,
          latestUpdatedAt: "2026-10-18 09:12:33",
          files: [
            file({ name: "foo-1.17.12.jar", path: "p/foo-1.17.12.jar" }),
            file({ name: "foo-1.17.12.pom", path: "p/foo-1.17.12.pom", fileKind: "pom" }),
          ],
        },
      ],
    });
    const second = pkg({ coordinate: "com.example:bar" });

    const view = initialViewState([first, second]);
    expect([...view.expandedPackages]).toEqual(["maven:com.example:foo"]);
    expect([...view.expandedVersions]).toEqual(["maven:com.example:foo::1.17.12"]);
    expect([...view.expandedChecksums]).toEqual([]);
    expect(view.selection).toEqual({
      packageKey: "maven:com.example:foo",
      version: "1.17.12",
      repository: "maven-releases",
      path: "p/foo-1.17.12.jar",
    });
  });

  it("无坐标伪包跳过版本层，选中包级首个文件", () => {
    const pseudo = pkg({
      coordinate: "raw-repo",
      kind: "file",
      versionCount: 0,
      versions: [],
      files: [file({ name: "docs/a.txt", path: "docs/a.txt", fileKind: "other" })],
    });
    const view = initialViewState([pseudo]);
    expect([...view.expandedPackages]).toEqual(["file:raw-repo"]);
    expect([...view.expandedVersions]).toEqual([]);
    expect(view.selection?.version).toBeNull();
    expect(view.selection?.path).toBe("docs/a.txt");
  });

  it("空结果给出空状态（不选中任何东西）", () => {
    expect(initialViewState([])).toEqual({
      expandedPackages: new Set(),
      expandedVersions: new Set(),
      expandedChecksums: new Set(),
      selection: null,
    });
  });
});

describe("resolveSelection / firstFileOfPackage 选中项解析", () => {
  it("按包键 + 版本 + 仓库路径定位到文件，并给出同版本文件", () => {
    const p = pkg({
      versions: [
        {
          version: "1.0.0",
          precision: "exact",
          fileCount: 2,
          size: 200,
          latestUpdatedAt: "2026-10-18 09:12:33",
          files: [
            file({ name: "a.jar", path: "a.jar" }),
            file({ name: "b.pom", path: "b.pom", fileKind: "pom" }),
          ],
        },
      ],
    });
    const resolved = resolveSelection([p], {
      packageKey: "maven:com.example:foo",
      version: "1.0.0",
      repository: "maven-releases",
      path: "b.pom",
    });
    expect(resolved?.file.name).toBe("b.pom");
    expect(resolved?.siblings.map((f) => f.name)).toEqual(["a.jar", "b.pom"]);
    expect(firstFileOfPackage(p)?.name).toBe("a.jar");
  });

  it("选中项已不在数据里（换关键词后）返回 null", () => {
    expect(
      resolveSelection([pkg({})], {
        packageKey: "maven:com.example:foo",
        version: "9.9.9",
        repository: "maven-releases",
        path: "x",
      }),
    ).toBeNull();
    expect(resolveSelection([pkg({})], null)).toBeNull();
  });
});
