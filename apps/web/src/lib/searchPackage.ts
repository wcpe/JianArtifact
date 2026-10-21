// 搜索结果「包级聚合」视图的纯函数集合。
//
// 页面只负责取数与渲染，这里承担四件可单测的事：
//   1. 精度徽标映射（exact / prerelease / substring / none，规格 §3.4）；
//   2. 命中词高亮切分（仅包坐标 / 版本号 / 文件名三处使用，路径全文不高亮）；
//   3. 文件类型 chip 选取与 `+N` 折叠（附录 A 的优先级）；
//   4. 非 relevance 排序下的前端重排（不为此再发请求，规格 §3.9）。
// 另附主从联动所需的默认展开 / 选中解析。
import type {
  SearchPackage,
  SearchPackageFile,
  SearchPackageVersion,
  SearchPrecision,
} from "../api/endpoints";

/** 排序键：与后端 `sort` 白名单同值，另加前端默认的 `relevance`（服务端聚合序）。 */
export type SearchSortKey = "relevance" | "name" | "repo" | "path" | "size" | "updated";

/** 排序方向；`relevance` 只与 `relevance` 键搭配（规格 §3.8 的持久化校验口径）。 */
export type SearchSortOrder = "asc" | "desc" | "relevance";

/** 精度徽标的渲染信息：文案走 i18n 键，颜色走 Mantine `Badge variant="light"`。 */
export interface PrecisionMeta {
  /** i18n 键；`none` 表示"无版本信号"，不显示徽标，故为 null。 */
  labelKey: string | null;
  /** Mantine 颜色名。 */
  color: string;
}

/** 精度 → 文案键 / 颜色。`none` 不显示徽标（规格 §3.4：无信号不加噪）。 */
export const PRECISION_META: Record<SearchPrecision, PrecisionMeta> = {
  exact: { labelKey: "search.precisionExact", color: "teal" },
  prerelease: { labelKey: "search.precisionPrerelease", color: "yellow" },
  substring: { labelKey: "search.precisionSubstring", color: "gray" },
  none: { labelKey: null, color: "gray" },
};

/** 取精度徽标信息；未知取值（后端将来新增层级）按 `none` 处理，保证不渲染错徽标。 */
export function precisionMeta(precision: string | null | undefined): PrecisionMeta {
  if (precision && precision in PRECISION_META) {
    return PRECISION_META[precision as SearchPrecision];
  }
  return PRECISION_META.none;
}

/** 高亮切分的一段：`hit=false` 的原样输出，`hit=true` 的包成 `<mark>`。 */
export interface HighlightSegment {
  text: string;
  hit: boolean;
}

/** 正则元字符转义，保证关键词按字面量匹配（`1.17.12` 里的点不能当通配）。 */
function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/**
 * 按关键词预编译高亮正则。
 *
 * 高亮是按行切分的：一个大包展开后可能有数千个文件行，若每行都现编译一次
 * `new RegExp(...)`，就会平白构造数千个等价正则。把编译结果提到「关键词集合」这一层，
 * 同一批关键词的全部分片共享一个正则；关键词不变时调用方还能用 memo 复用同一个实例。
 *
 * 返回 null 表示无需高亮（无关键词 / 全是空白）。正则带 `g` 标志，供 `matchAll` 使用；
 * `matchAll` 内部会克隆正则，不会改动这里实例的 `lastIndex`，因此可安全跨行复用。
 */
export function compileHighlightPattern(keywords: readonly string[]): RegExp | null {
  const keys = [
    ...new Set(keywords.map((keyword) => keyword.trim()).filter((keyword) => keyword.length > 0)),
  ];
  if (keys.length === 0) {
    return null;
  }
  // 长词优先，让 `1.17.12` 整体命中而不是被短词 `1.17` 先切碎。
  keys.sort((a, b) => b.length - a.length);
  return new RegExp(keys.map(escapeRegExp).join("|"), "giu");
}

/**
 * 用预编译正则把文本切成「前置 / 命中 / 后置」片段（大小写不敏感，保留原文大小写）。
 *
 * 多个关键词各自高亮；重叠命中（如同时搜 `1.17` 与 `1.17.12`）合并为一段，
 * 避免同一段文字被嵌套渲染成两层 `<mark>`。无命中时返回整段单个片段，
 * 调用方无需为"有没有高亮"写两套渲染分支。
 */
export function splitHighlightWithPattern(
  text: string,
  pattern: RegExp | null,
): HighlightSegment[] {
  if (!text || pattern === null) {
    return [{ text, hit: false }];
  }
  const ranges: [number, number][] = [];
  for (const match of text.matchAll(pattern)) {
    const start = match.index ?? 0;
    ranges.push([start, start + match[0].length]);
  }
  if (ranges.length === 0) {
    return [{ text, hit: false }];
  }
  ranges.sort((a, b) => a[0] - b[0]);
  const merged: [number, number][] = [];
  for (const range of ranges) {
    const last = merged[merged.length - 1];
    if (last && range[0] <= last[1]) {
      last[1] = Math.max(last[1], range[1]);
    } else {
      merged.push([range[0], range[1]]);
    }
  }
  const segments: HighlightSegment[] = [];
  let cursor = 0;
  for (const [start, end] of merged) {
    if (start > cursor) {
      segments.push({ text: text.slice(cursor, start), hit: false });
    }
    segments.push({ text: text.slice(start, end), hit: true });
    cursor = end;
  }
  if (cursor < text.length) {
    segments.push({ text: text.slice(cursor), hit: false });
  }
  return segments;
}

/**
 * 按关键词切分文本为「前置 / 命中 / 后置」片段。
 *
 * 单次调用入口：内部现编译正则。适合只切一两段文本的场景；同一批关键词要切很多行时，
 * 改用 `compileHighlightPattern` + `splitHighlightWithPattern`，避免逐行重复编译。
 */
export function splitHighlight(text: string, keywords: readonly string[]): HighlightSegment[] {
  return splitHighlightWithPattern(text, compileHighlightPattern(keywords));
}

/**
 * 文件类型优先级（规格附录 A 的 `fileKindPriority`）。
 * 未收录的取值排在 `other` 之后、`checksum` 之前，保证陌生类型可见但不挤掉主构件。
 */
export const FILE_KIND_PRIORITY: Record<string, number> = {
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

/** 未收录的 fileKind 取值：主构件之后、校验和之前。 */
const UNKNOWN_KIND_PRIORITY = 8;

/** 版本行默认展示的 chip 个数（规格 §3.9：前 4 个，其余折叠为 `+N`）。 */
export const FILE_KIND_CHIP_LIMIT = 4;

/** 校验和 / 签名文件（附录 A 的 `checksum`）——树内单独折成一行，不占 chip 位。 */
export function isChecksumKind(fileKind: string): boolean {
  return fileKind === "checksum";
}

/** chip 选取结果：`kinds` 按优先级升序，`overflow` 是被折叠掉的不同类型个数。 */
export interface FileKindChips {
  kinds: string[];
  overflow: number;
}

/**
 * 按附录 A 优先级挑出前 `limit` 个文件类型 chip，其余折叠为 `+N`。
 *
 * 校验和类型不参与 chip 选取：它在树里已有独立的「校验和 (N)」折叠行，
 * 占一个 chip 位只会把 `+N` 变成无人可寻的空指示。
 */
export function fileKindChips(
  files: readonly { fileKind: string }[],
  limit: number = FILE_KIND_CHIP_LIMIT,
): FileKindChips {
  const kinds = [...new Set(files.map((file) => file.fileKind))].filter(
    (kind) => !isChecksumKind(kind),
  );
  kinds.sort((a, b) => {
    const pa = FILE_KIND_PRIORITY[a] ?? UNKNOWN_KIND_PRIORITY;
    const pb = FILE_KIND_PRIORITY[b] ?? UNKNOWN_KIND_PRIORITY;
    return pa - pb || a.localeCompare(b);
  });
  return { kinds: kinds.slice(0, limit), overflow: Math.max(0, kinds.length - limit) };
}

/** 把版本（或无坐标伪包）的文件拆成「正文文件」与「校验和文件」两组。 */
export function splitChecksumFiles(files: readonly SearchPackageFile[]): {
  primary: SearchPackageFile[];
  checksums: SearchPackageFile[];
} {
  const primary: SearchPackageFile[] = [];
  const checksums: SearchPackageFile[] = [];
  for (const file of files) {
    (isChecksumKind(file.fileKind) ? checksums : primary).push(file);
  }
  return { primary, checksums };
}

/** 包节点的稳定键：`kind + coordinate`（同坐标不同协议不会混为一谈）。 */
export function packageKey(pkg: Pick<SearchPackage, "kind" | "coordinate">): string {
  return `${pkg.kind}:${pkg.coordinate}`;
}

/** 版本节点的稳定键（包键 + 版本号）。 */
export function versionKey(pkgKey: string, version: string): string {
  return `${pkgKey}::${version}`;
}

/** 文件行的稳定键：同一路径可能存在于多个仓库，必须带仓库消歧。 */
export function fileKey(file: Pick<SearchPackageFile, "repository" | "path">): string {
  return `${file.repository}/${file.path}`;
}

/** 当前选中项：包键 + 版本（伪包为 null）+ 文件键（仓库 + 路径）。 */
export interface SearchSelection {
  packageKey: string;
  version: string | null;
  repository: string;
  path: string;
}

/** 主从视图的展开 / 选中状态。 */
export interface SearchViewState {
  expandedPackages: ReadonlySet<string>;
  expandedVersions: ReadonlySet<string>;
  /** 已展开「校验和 (N)」的版本键集合。 */
  expandedChecksums: ReadonlySet<string>;
  selection: SearchSelection | null;
}

/** 不可变地翻转集合中的一个键。 */
export function toggleInSet(source: ReadonlySet<string>, key: string): Set<string> {
  const next = new Set(source);
  if (next.has(key)) {
    next.delete(key);
  } else {
    next.add(key);
  }
  return next;
}

/** 取包的首个文件：有版本的包取首版本首文件，无坐标伪包直接取包级首文件。 */
export function firstFileOfPackage(pkg: SearchPackage): SearchPackageFile | null {
  return pkg.versions[0]?.files[0] ?? pkg.files?.[0] ?? null;
}

/** 由包 + 版本 + 文件构造选中项。 */
export function createSelection(
  pkg: SearchPackage,
  version: string | null,
  file: SearchPackageFile,
): SearchSelection {
  return {
    packageKey: packageKey(pkg),
    version,
    repository: file.repository,
    path: file.path,
  };
}

/** 取某版本（伪包传 null）的首个文件。 */
export function firstFileOfVersion(
  pkg: SearchPackage,
  version: string | null,
): SearchPackageFile | null {
  if (version === null) {
    return pkg.files?.[0] ?? null;
  }
  return pkg.versions.find((item) => item.version === version)?.files[0] ?? null;
}

/**
 * 默认视图状态（规格 §3.9）：展开首个包、展开并选中其首个版本、右栏显示该版本首个文件。
 * 包为空时返回全空状态。
 */
export function initialViewState(packages: readonly SearchPackage[]): SearchViewState {
  const first = packages[0];
  if (!first) {
    return {
      expandedPackages: new Set<string>(),
      expandedVersions: new Set<string>(),
      expandedChecksums: new Set<string>(),
      selection: null,
    };
  }
  const pkgKey = packageKey(first);
  const version = first.versions[0]?.version ?? null;
  const file = firstFileOfVersion(first, version);
  return {
    expandedPackages: new Set([pkgKey]),
    expandedVersions: version ? new Set([versionKey(pkgKey, version)]) : new Set<string>(),
    expandedChecksums: new Set<string>(),
    selection: file ? createSelection(first, version, file) : null,
  };
}

/** 选中项解析结果：包、版本（伪包为 null）、文件。 */
export interface ResolvedSelection {
  pkg: SearchPackage;
  version: SearchPackageVersion | null;
  file: SearchPackageFile;
  /** 「同版本文件」列表的数据源（伪包用包级 files）。 */
  siblings: SearchPackageFile[];
}

/** 在聚合结果里定位选中项；数据已变化（文件消失）时返回 null。 */
export function resolveSelection(
  packages: readonly SearchPackage[],
  selection: SearchSelection | null,
): ResolvedSelection | null {
  if (!selection) {
    return null;
  }
  const pkg = packages.find((item) => packageKey(item) === selection.packageKey);
  if (!pkg) {
    return null;
  }
  const version =
    selection.version === null
      ? null
      : (pkg.versions.find((item) => item.version === selection.version) ?? null);
  if (selection.version !== null && !version) {
    return null;
  }
  const siblings = version ? version.files : (pkg.files ?? []);
  const file = siblings.find(
    (item) => item.repository === selection.repository && item.path === selection.path,
  );
  if (!file) {
    return null;
  }
  return { pkg, version, file, siblings };
}

/** 非 relevance 的排序键（`sortPackages` 实际会重排的分支）。 */
type OrderingKey = Exclude<SearchSortKey, "relevance">;

/** 路径比较：带数字感知，`2/` 排在 `10/` 之前而不是按字典序倒过来。 */
function comparePath(a: string, b: string): number {
  return a.localeCompare(b, undefined, { numeric: true });
}

/** 单个文件的排序比较（升序口径；方向由调用方乘 -1）。 */
function compareFiles(a: SearchPackageFile, b: SearchPackageFile, by: OrderingKey): number {
  switch (by) {
    case "name":
      return comparePath(a.name, b.name) || comparePath(a.path, b.path);
    case "repo":
      return comparePath(a.repository, b.repository) || comparePath(a.path, b.path);
    case "size":
      return a.size - b.size || comparePath(a.path, b.path);
    case "updated":
      // 后端时间统一是 `YYYY-MM-DD HH:MM:SS`（UTC），字典序即时间序。
      return a.updatedAt.localeCompare(b.updatedAt) || comparePath(a.path, b.path);
    case "path":
    default:
      return comparePath(a.path, b.path);
  }
}

/** 空值安全的文件比较：缺文件的分组一律沉底，避免排序键缺失时整体乱序。 */
function compareFilesSafe(
  a: SearchPackageFile | undefined,
  b: SearchPackageFile | undefined,
  by: OrderingKey,
  direction: number,
): number {
  if (!a && !b) {
    return 0;
  }
  if (!a) {
    return 1;
  }
  if (!b) {
    return -1;
  }
  return direction * compareFiles(a, b, by);
}

/** 版本组排序用的代表文件（组内首个文件，即重排后优先级最高的那个）。 */
function representativeOfVersion(version: SearchPackageVersion): SearchPackageFile | undefined {
  return version.files[0];
}

/** 包排序用的代表文件（首版本首文件；伪包取包级首文件）。 */
function representativeOfPackage(pkg: SearchPackage): SearchPackageFile | undefined {
  return pkg.versions[0]?.files[0] ?? pkg.files?.[0];
}

/**
 * 按当前排序偏好重排聚合结果（规格 §3.9：左栏仍按包分组，只改包与版本内部的相对顺序）。
 *
 * 语义是「文件级排序折叠进分组」：文件按所选键排序，版本以组内首个文件、包以首版本首个文件
 * 作为代表参与同级排序，于是整个左栏的阅读顺序与文件级排序一致。`relevance` 原样返回后端顺序
 * （后端已按相关度比较器排好，见规格 §3.5）。
 */
export function sortPackages(
  packages: readonly SearchPackage[],
  by: SearchSortKey,
  order: SearchSortOrder,
): SearchPackage[] {
  if (by === "relevance") {
    return packages as SearchPackage[];
  }
  const key: OrderingKey = by;
  const direction = order === "desc" ? -1 : 1;
  return packages
    .map((pkg) => {
      const versions = pkg.versions
        .map((version) => ({
          ...version,
          files: [...version.files].sort((a, b) => direction * compareFiles(a, b, key)),
        }))
        .sort(
          (a, b) =>
            compareFilesSafe(
              representativeOfVersion(a),
              representativeOfVersion(b),
              key,
              direction,
            ) || comparePath(a.version, b.version),
        );
      const files = pkg.files
        ? [...pkg.files].sort((a, b) => direction * compareFiles(a, b, key))
        : undefined;
      return files ? { ...pkg, versions, files } : { ...pkg, versions };
    })
    .sort(
      (a, b) =>
        compareFilesSafe(representativeOfPackage(a), representativeOfPackage(b), key, direction) ||
        comparePath(a.coordinate, b.coordinate),
    );
}

/** 每类排序键的默认方向（新的 / 大的在前，名称与路径正序）。 */
export function defaultOrderFor(by: SearchSortKey): SearchSortOrder {
  switch (by) {
    case "relevance":
      return "relevance";
    case "size":
    case "updated":
      return "desc";
    default:
      return "asc";
  }
}
