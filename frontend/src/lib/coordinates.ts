// 多格式依赖坐标生成（FR-93，纯前端、不改后端）：
//
// 把 Maven 制品路径反解为 GAV，再用 JS 模板产出多种构建工具的依赖坐标片段
// （Apache Maven / Gradle Groovy / Gradle Kotlin DSL / Scala SBT / Apache Ivy /
//  Groovy Grape / Leiningen / PURL）。GAV 反解规则与后端 Gav::from_path 对齐，
// 仅对能反解出 GAV 的 Maven 主构件产出全套坐标；其余格式 / 无法反解者返回空。
//
// 另含 HTML View 外链（指向 FR-75 的 HTML 仓库索引视图）与制品下载 URL 的纯函数。

import type { RepoFormat } from '../api/types';

/** Maven 坐标（GAV）。 */
export interface MavenGav {
  groupId: string;
  artifactId: string;
  version: string;
}

/** 单条依赖坐标片段：下拉标签 + 代码语言（高亮提示）+ 内容。 */
export interface CoordinateSnippet {
  label: string;
  language: string;
  content: string;
}

/**
 * 从 Maven 制品路径反解 GAV（与后端 Gav::from_path 同规则）。
 *
 * 布局：`{group 以 / 分隔}/{artifactId}/{version}/{文件}`。去空段后取倒数第三段为
 * artifactId、倒数第二段为 version、其余前缀段以 `.` 连接为 groupId；段数 < 4
 * （无法构成合法 GAV，如目录级 metadata）返回 null。
 */
export function parseMavenGav(path: string): MavenGav | null {
  const segments = path.split('/').filter((s) => s.length > 0);
  // 至少 4 段：group(>=1) + artifactId + version + 文件
  if (segments.length < 4) return null;
  const fileIdx = segments.length - 1;
  const version = segments[fileIdx - 1];
  const artifactId = segments[fileIdx - 2];
  const groupSegments = segments.slice(0, fileIdx - 2);
  if (groupSegments.length === 0) return null;
  return {
    groupId: groupSegments.join('.'),
    artifactId,
    version,
  };
}

/**
 * 生成多格式依赖坐标片段。
 *
 * 仅对能反解出 GAV 的 Maven 主构件产出全套（8 种）坐标；其余格式或无法反解者返回空数组
 * （npm / docker / raw 等无统一 GAV，保留后端原生 usage 片段即可）。
 */
export function buildCoordinateSnippets(format: RepoFormat, path: string): CoordinateSnippet[] {
  if (format !== 'maven') return [];
  const gav = parseMavenGav(path);
  if (!gav) return [];
  const { groupId: g, artifactId: a, version: v } = gav;

  return [
    {
      label: 'Apache Maven',
      language: 'xml',
      content: `<dependency>\n  <groupId>${g}</groupId>\n  <artifactId>${a}</artifactId>\n  <version>${v}</version>\n</dependency>`,
    },
    {
      label: 'Gradle Groovy DSL',
      language: 'groovy',
      content: `implementation '${g}:${a}:${v}'`,
    },
    {
      label: 'Gradle Kotlin DSL',
      language: 'kotlin',
      content: `implementation("${g}:${a}:${v}")`,
    },
    {
      label: 'Scala SBT',
      language: 'scala',
      content: `libraryDependencies += "${g}" % "${a}" % "${v}"`,
    },
    {
      label: 'Apache Ivy',
      language: 'xml',
      content: `<dependency org="${g}" name="${a}" rev="${v}" />`,
    },
    {
      label: 'Groovy Grape',
      language: 'groovy',
      content: `@Grab(group='${g}', module='${a}', version='${v}')`,
    },
    {
      label: 'Leiningen',
      language: 'clojure',
      content: `[${g}/${a} "${v}"]`,
    },
    {
      label: 'PURL',
      language: 'text',
      content: `pkg:maven/${g}/${a}@${v}`,
    },
  ];
}

/** 对路径逐段编码并以 `/` 连接（保留分隔语义）。 */
function encodeSegments(value: string): string {
  return value
    .split('/')
    .filter((s) => s.length > 0)
    .map((seg) => encodeURIComponent(seg))
    .join('/');
}

/**
 * HTML View 外链 URL（FR-75 的 HTML 仓库索引视图）。
 *
 * 指向制品所在目录的索引：`/{repo}/{父目录}/`（尾斜杠 → 后端按 `Accept: text/html`
 * 渲染目录索引页）。根目录文件回退到仓库根索引 `/{repo}/`。
 */
export function htmlViewUrl(repoName: string, path: string): string {
  const repo = encodeURIComponent(repoName);
  const slash = path.lastIndexOf('/');
  if (slash < 0) {
    return `/${repo}/`;
  }
  const dir = encodeSegments(path.slice(0, slash));
  return `/${repo}/${dir}/`;
}

/** 制品原始下载 URL：`/{repo}/{path}`（无尾斜杠，单文件下载；逐段编码保留分隔斜杠）。 */
export function downloadUrl(repoName: string, path: string): string {
  return `/${encodeURIComponent(repoName)}/${encodeSegments(path)}`;
}
