// 包级聚合的开发态实现（对应后端 FR-150 的 BuildPackageGroups / ParseCoordinates / MatchPrecision）。
//
// 为什么 mock 也要实现一遍：搜索结果页（FR-151）的主数据源就是 `group_by=package` 的响应，
// 若 mock 只给文件级 items，`pnpm dev` 下搜索页会永远空白，场景夹具（empty / loading /
// standby_read_only）也失去意义。这里按规格 §3.2–§3.5 做等价的最简实现，**只在开发态使用**，
// 不参与生产构建；与 Go 侧的口径保持一致即可，不追求逐个边界完全复刻。

/** 归一化后的 mock 搜索结果行（仓库 format 一并带上，坐标解析要按仓库格式门控）。 */
export interface MockSearchMatch {
  repository: string;
  format: string;
  path: string;
  size: number;
  hash: string;
  sha1?: string;
  updatedAt: string;
}

/** 精度分层（规格 §3.4）：数值越小越优。 */
export type MockPrecision = "exact" | "prerelease" | "substring" | "none";

export interface MockSearchPackageFile {
  name: string;
  path: string;
  repository: string;
  size: number;
  sha1: string;
  updatedAt: string;
  fileKind: string;
}

export interface MockSearchPackageVersion {
  version: string;
  precision: MockPrecision;
  fileCount: number;
  size: number;
  latestUpdatedAt: string;
  files: MockSearchPackageFile[];
}

export interface MockSearchPackage {
  coordinate: string;
  kind: "maven" | "npm" | "file";
  repositories: string[];
  versionCount: number;
  fileCount: number;
  bestPrecision: MockPrecision;
  latestUpdatedAt: string;
  topVersion: string;
  versions: MockSearchPackageVersion[];
  files?: MockSearchPackageFile[];
}

/** 精度优先级：与规格 §3.5 的比较器一致。 */
const PRECISION_RANK: Record<MockPrecision, number> = {
  exact: 0,
  prerelease: 1,
  substring: 2,
  none: 3,
};

/** 附录 A 的 fileKind 优先级（只用于组内文件排序）。 */
const FILE_KIND_PRIORITY: Record<string, number> = {
  jar: 0,
  war: 1,
  ear: 1,
  aar: 1,
  tgz: 1,
  sources: 2,
  javadoc: 3,
  tests: 4,
  pom: 5,
  packument: 5,
  metadata: 6,
  other: 7,
  checksum: 9,
};

const CHECKSUM_EXTS = new Set(["sha1", "md5", "sha256", "sha512", "asc"]);

/** 取扩展名（小写，不含点）。 */
function extensionOf(name: string): string {
  const index = name.lastIndexOf(".");
  return index === -1 ? "" : name.slice(index + 1).toLowerCase();
}

/** 按附录 A 的判定顺序归类（首个命中即停）。 */
export function classifyFileKind(path: string, format: string): string {
  const name = path.split("/").at(-1) ?? path;
  const ext = extensionOf(name);
  const lower = name.toLowerCase();
  if (ext === "jar") {
    if (lower.includes("-sources")) return "sources";
    if (lower.includes("-javadoc")) return "javadoc";
    if (lower.includes("-tests")) return "tests";
    return "jar";
  }
  if (ext === "war" || ext === "ear" || ext === "aar") return ext;
  if (ext === "tgz") return "tgz";
  if (ext === "pom") return "pom";
  // npm 包根的 packument：`<pkg>/<file>.json` 或 `@scope/<pkg>/<file>.json`
  if (ext === "json" && format === "npm" && path.split("/").filter(Boolean).length <= 2) {
    return "packument";
  }
  if (lower === "maven-metadata.xml" || ext === "module") return "metadata";
  if (CHECKSUM_EXTS.has(ext)) return "checksum";
  return "other";
}

/** 版本段解析结果：`coordinate` 为展示坐标，`version` 为空表示未版本化。 */
interface Coordinates {
  kind: "maven" | "npm" | "file";
  coordinate: string;
  version: string;
}

/** Maven：`group/artifact/version/file`（末段为文件名，倒数第二段须以数字起首）。 */
function parseMaven(path: string): Coordinates | null {
  const segments = path.split("/").filter(Boolean);
  if (segments.length < 4) {
    return null;
  }
  const version = segments[segments.length - 2] ?? "";
  if (!/^[0-9]/.test(version)) {
    return null;
  }
  const artifact = segments[segments.length - 3] ?? "";
  const group = segments.slice(0, segments.length - 3).join(".");
  if (!group || !artifact) {
    return null;
  }
  return { kind: "maven", coordinate: `${group}:${artifact}`, version };
}

/** npm：`pkg/...`、`@scope/pkg/...`、`pkg/-/pkg-<version>.tgz`。 */
function parseNpm(path: string): Coordinates | null {
  const segments = path.split("/").filter(Boolean);
  if (segments.length === 0) {
    return null;
  }
  const scoped = (segments[0] ?? "").startsWith("@");
  const nameSegments = scoped ? 2 : 1;
  if (segments.length < nameSegments) {
    return null;
  }
  const coordinate = segments.slice(0, nameSegments).join("/");
  const rest = segments.slice(nameSegments);
  if (rest.length === 0) {
    // 包根本身不是文件行，按未版本化处理（实际不会出现在搜索结果里）。
    return { kind: "npm", coordinate, version: "" };
  }
  if (!scoped && rest[0] === "-") {
    // tarball：`pkg/-/pkg-<version>.tgz`，剥离前缀与后缀。
    const name = rest.at(-1) ?? "";
    const bare = name.replace(/\.tgz$/i, "");
    const prefix = `${coordinate}-`;
    const version = bare.startsWith(prefix) ? bare.slice(prefix.length) : "";
    return { kind: "npm", coordinate, version };
  }
  const candidate = rest[0] ?? "";
  return { kind: "npm", coordinate, version: /^[0-9]/.test(candidate) ? candidate : "" };
}

/** 按仓库 format 门控解析坐标；不认识的格式一律降级为「无坐标」（按仓库归伪包）。 */
export function parseCoordinates(match: MockSearchMatch): Coordinates {
  if (match.format === "maven") {
    const parsed = parseMaven(match.path);
    if (parsed) return parsed;
  } else if (match.format === "npm") {
    const parsed = parseNpm(match.path);
    if (parsed) return parsed;
  }
  // 降级：coordinate 用仓库名，kind=file（规格 §3.2）。
  return { kind: "file", coordinate: match.repository, version: "" };
}

/** 单词对版本段的精度判定（规格 §3.4）。 */
function precisionOf(version: string, term: string): MockPrecision {
  const v = version.toLowerCase();
  const t = term.toLowerCase();
  if (!t || !v.includes(t)) return "none";
  if (v === t) return "exact";
  if (v.startsWith(t)) {
    const rest = v.slice(t.length);
    if (rest.startsWith("-") || rest.startsWith("+")) return "prerelease";
  }
  return "substring";
}

/** 文件的版本精度：多个关键词取最有利的一档。 */
function matchPrecision(version: string, terms: readonly string[]): MockPrecision {
  if (!version) return "none";
  let best: MockPrecision = "none";
  for (const term of terms) {
    const current = precisionOf(version, term);
    if (PRECISION_RANK[current] < PRECISION_RANK[best]) {
      best = current;
    }
  }
  return best;
}

/** 版本向量（规格 §3.5）：按 `.`/`-`/`+` 切分，遇非数字段即停。 */
function versionVector(version: string): number[] {
  const out: number[] = [];
  for (const segment of version.split(/[.\-+]/)) {
    if (!/^\d+$/.test(segment)) break;
    out.push(Number(segment));
  }
  return out;
}

/** 版本向量降序比较（缺失位视为 0）。 */
function compareVersionVectorDesc(a: string, b: string): number {
  const left = versionVector(a);
  const right = versionVector(b);
  const length = Math.max(left.length, right.length);
  for (let i = 0; i < length; i += 1) {
    const diff = (right[i] ?? 0) - (left[i] ?? 0);
    if (diff !== 0) return diff;
  }
  return 0;
}

function compareFilesKindThenPath(a: MockSearchPackageFile, b: MockSearchPackageFile): number {
  const diff =
    (FILE_KIND_PRIORITY[a.fileKind] ?? 8) - (FILE_KIND_PRIORITY[b.fileKind] ?? 8);
  return diff !== 0 ? diff : a.path.localeCompare(b.path);
}

function toFile(match: MockSearchMatch): MockSearchPackageFile {
  return {
    name: match.path.split("/").at(-1) ?? match.path,
    path: match.path,
    repository: match.repository,
    size: match.size,
    sha1: match.sha1 ?? "",
    updatedAt: match.updatedAt,
    fileKind: classifyFileKind(match.path, match.format),
  };
}

/**
 * 把命中的文件行聚合成包 / 版本 / 文件三级结构（规格 §3.3），并按 relevance 比较器排序（§3.5）。
 * `terms` 是表达式里的普通关键词，用于判定版本精度。
 */
export function buildSearchPackages(
  matches: readonly MockSearchMatch[],
  terms: readonly string[],
): MockSearchPackage[] {
  const grouped = new Map<string, MockSearchPackage>();

  for (const match of matches) {
    const coordinates = parseCoordinates(match);
    const file = toFile(match);
    const key = `${coordinates.kind}:${coordinates.coordinate}`;
    let pkg = grouped.get(key);
    if (!pkg) {
      pkg = {
        coordinate: coordinates.coordinate,
        kind: coordinates.kind,
        repositories: [],
        versionCount: 0,
        fileCount: 0,
        bestPrecision: "none",
        latestUpdatedAt: "",
        topVersion: "",
        versions: [],
        ...(coordinates.kind === "file" ? { files: [] } : {}),
      };
      grouped.set(key, pkg);
    }
    if (!pkg.repositories.includes(match.repository)) {
      pkg.repositories.push(match.repository);
    }
    pkg.fileCount += 1;
    if (match.updatedAt > pkg.latestUpdatedAt) {
      pkg.latestUpdatedAt = match.updatedAt;
    }
    if (coordinates.kind === "file") {
      (pkg.files ??= []).push(file);
      continue;
    }
    // 版本键大小写不敏感归并，展示取首次出现的原文（规格 §3.3）。
    const versionKey = coordinates.version.toLowerCase();
    let version = pkg.versions.find((item) => item.version.toLowerCase() === versionKey);
    if (!version) {
      version = {
        version: coordinates.version,
        precision: matchPrecision(coordinates.version, terms),
        fileCount: 0,
        size: 0,
        latestUpdatedAt: "",
        files: [],
      };
      pkg.versions.push(version);
    }
    version.files.push(file);
    version.fileCount += 1;
    version.size += file.size;
    if (file.updatedAt > version.latestUpdatedAt) {
      version.latestUpdatedAt = file.updatedAt;
    }
  }

  for (const pkg of grouped.values()) {
    pkg.versions.sort(
      (a, b) =>
        PRECISION_RANK[a.precision] - PRECISION_RANK[b.precision] ||
        compareVersionVectorDesc(a.version, b.version) ||
        a.version.localeCompare(b.version),
    );
    for (const version of pkg.versions) {
      version.files.sort(compareFilesKindThenPath);
    }
    pkg.files?.sort(compareFilesKindThenPath);
    pkg.versionCount = pkg.versions.length;
    pkg.bestPrecision = pkg.versions.reduce<MockPrecision>(
      (best, version) => (PRECISION_RANK[version.precision] < PRECISION_RANK[best] ? version.precision : best),
      "none",
    );
    pkg.topVersion = pkg.versions[0]?.version ?? "";
  }

  return [...grouped.values()].sort(comparePackages);
}

/** 包级 relevance 比较器（规格 §3.5）：精度 → 版本向量 → 更新时间 → 坐标。 */
function comparePackages(a: MockSearchPackage, b: MockSearchPackage): number {
  const rankDiff = PRECISION_RANK[a.bestPrecision] - PRECISION_RANK[b.bestPrecision];
  if (rankDiff !== 0) return rankDiff;
  if (PRECISION_RANK[a.bestPrecision] < 3) {
    const versionDiff = compareVersionVectorDesc(a.topVersion, b.topVersion);
    if (versionDiff !== 0) return versionDiff;
  }
  if (a.latestUpdatedAt !== b.latestUpdatedAt) {
    return a.latestUpdatedAt < b.latestUpdatedAt ? 1 : -1;
  }
  return a.coordinate.localeCompare(b.coordinate);
}

/** 从查询表达式里取普通关键词（去掉 `repo:` / `format:` / `ext:` / `-` 前缀的 token）。 */
export function searchTermsOf(query: string): string[] {
  const tokens = query.match(/(?:[^\s"]+|"[^"]*")+/g) ?? [];
  const terms: string[] = [];
  for (const raw of tokens) {
    const token = raw.replace(/^"|"$/g, "");
    if (/^(-?)(repo|format|ext):/.test(token) || token.startsWith("-")) continue;
    if (token) terms.push(token);
  }
  return terms;
}
