// 上传审计聚合（纯前端纯函数）：把扁平的「上传 / 发布」审计事件聚合成
// 「仓库 → groupId → artifactId → 版本 → 文件」的树，便于一眼看出「谁往哪个仓库
// 传了哪些坐标、多少个版本」。
//
// 关键约束：
// - 事件来自审计工作台当前查询结果集（不额外发请求），action 归类见 labels.isUploadAction；
// - 制品路径取自 target.label（后端 EntityKey = `repo/path`），仓库名取自 target.repository；
// - Maven 路径复用 coordinates.parseMavenGav 反解 GAV；非 Maven（npm / raw / 其他格式）
//   一律优雅降级到「其他」分支，按目录路径聚合，**绝不丢弃事件**。

import type { AuditEvent, RepoFormat } from "../../api/types";
import { isUploadAction, parseTime } from "./labels";
import { parseMavenGav } from "../../lib/coordinates";

/** 从制品路径尽力反解出的坐标；反解不出 GAV 时仅 filename / directory 有值。 */
export interface UploadCoordinates {
  /** Maven groupId（仅 Maven 路径）。 */
  groupId?: string;
  /** Maven artifactId（仅 Maven 路径）。 */
  artifactId?: string;
  /** Maven version（仅 Maven 路径）。 */
  version?: string;
  /** 文件名（路径最后一段）。 */
  filename: string;
  /** 目录路径（去掉文件名后的剩余部分），用于「其他」分支聚合。 */
  directory: string;
}

/** 制品事件的目标三元组：仓库名、仓库内路径、原始 label（`repo/path`）。 */
export interface UploadArtifactTarget {
  repo: string;
  path: string;
  label: string;
}

/** 树节点类型：仓库 / Maven 分组 / 制品 / 版本 / 其他 / 目录 / 文件。 */
export type UploadNodeKind =
  "repository" | "group" | "artifact" | "version" | "others" | "directory" | "file";

/** 聚合树节点（已按展示顺序排序）。 */
export interface UploadTreeNode {
  /** 层级路径构成的稳定键，供展开态与 React key 使用。 */
  key: string;
  kind: UploadNodeKind;
  label: string;
  /** 子树内事件数。 */
  count: number;
  /** 子树内最新 / 最早事件时间（毫秒时间戳；无样本为 0）。 */
  latestAt: number;
  earliestAt: number;
  children: UploadTreeNode[];
  /** 文件叶子对应的事件（仅 kind=file）。 */
  event?: AuditEvent;
}

/** 版本段至少含一个数字才认为像版本——避免把 npm 的 `-` 分隔段等误判为版本。 */
const VERSION_HINT = /\d/;

/**
 * 从制品路径反解坐标。
 *
 * - `format === "maven"`：按 Maven 布局反解（`group/artifactId/version/file`）。
 * - 其它已知格式（npm / raw / pypi / …）：非 Maven，直接走「其他」。
 * - `format` 未知：用启发式判断是否像 Maven 制品（见 looksLikeMavenGav），
 *   命中则给出 GAV，否则降级到「其他」。
 */
export function parseUploadCoordinates(path: string, format?: RepoFormat): UploadCoordinates {
  const segments = path.split("/").filter((s) => s.length > 0);
  const filename = segments.length > 0 ? segments[segments.length - 1]! : path;
  const directory = segments.slice(0, -1).join("/");
  const gav = resolveMavenGav(path, format);
  if (gav) return { ...gav, filename, directory };
  return { filename, directory };
}

function resolveMavenGav(
  path: string,
  format?: RepoFormat,
): { groupId: string; artifactId: string; version: string } | null {
  if (format === "maven") {
    // 已知是 Maven 仓库：信任自身布局规则（与后端 Gav::from_path 对齐）。
    return parseMavenGav(path);
  }
  if (format !== undefined) {
    // 已知是其它格式：不存在 Maven GAV，走「其他」分支。
    return null;
  }
  // 格式未知（审计事件不携带 format）：启发式判断，宁可判成「其他」也不误判。
  return looksLikeMavenGav(path);
}

/**
 * 启发式判断路径是否像 Maven 制品布局。
 *
 * 基础规则复用 parseMavenGav（≥4 段），再加两条约束以排除 npm / raw：
 * - 版本段必须含数字（npm 的 `@scope/pkg/-/pkg-1.0.0.tgz` 其「版本段」是 `-`，被排除）；
 * - 文件名要么是 `maven-metadata.xml`，要么以 `{artifactId}-` 开头（Maven 制品命名约定）。
 */
function looksLikeMavenGav(path: string): {
  groupId: string;
  artifactId: string;
  version: string;
} | null {
  const gav = parseMavenGav(path);
  if (!gav) return null;
  if (!VERSION_HINT.test(gav.version)) return null;
  const filename =
    path
      .split("/")
      .filter((s) => s.length > 0)
      .pop() ?? "";
  if (filename === "maven-metadata.xml") return gav;
  if (!filename.startsWith(`${gav.artifactId}-`)) return null;
  return gav;
}

/**
 * 取制品事件的目标三元组；非制品事件或缺少可用文本时返回 null。
 *
 * `target.label` 是后端 EntityKey（asset 操作为 `repo/path`），`target.repository`
 * 是仓库名。label 若已含 `repo/` 前缀则剥离，避免仓库层级重复。
 */
export function uploadArtifactTarget(event: AuditEvent): UploadArtifactTarget | null {
  const target = event.target as { kind?: string; label?: string; repository?: string } | undefined;
  if (target?.kind !== "artifact") return null;
  const label = target.label ?? target.repository ?? "";
  if (!label) return null;
  const repo = target.repository ?? "";
  if (repo && label.startsWith(`${repo}/`)) {
    return { repo, path: label.slice(repo.length + 1), label };
  }
  // 回退：label 首段当仓库名（后端始终带 repo/repository，这里仅兜底异常数据）。
  const slash = label.indexOf("/");
  if (slash > 0) return { repo: label.slice(0, slash), path: label.slice(slash + 1), label };
  return { repo, path: label, label };
}

/** 文件叶子跳转审计事件用的关键字（后端 EntityKey，`repo/path`）。 */
export function artifactSearchText(event: AuditEvent): string {
  const target = event.target as { label?: string; repository?: string } | undefined;
  return target?.label ?? target?.repository ?? "";
}

interface MutableNode {
  key: string;
  kind: UploadNodeKind;
  label: string;
  count: number;
  latestAt: number;
  earliestAt: number;
  children: Map<string, MutableNode>;
  event?: AuditEvent;
}

function upsert(
  map: Map<string, MutableNode>,
  key: string,
  kind: UploadNodeKind,
  label: string,
): MutableNode {
  const existing = map.get(key);
  if (existing) return existing;
  const created: MutableNode = {
    key,
    kind,
    label,
    count: 0,
    latestAt: 0,
    earliestAt: 0,
    children: new Map(),
  };
  map.set(key, created);
  return created;
}

/**
 * 把上传 / 发布事件聚合成树。
 *
 * 层级：仓库 → （Maven：groupId → artifactId → 版本）或（其他：目录）→ 文件。
 * 每层节点累计子树内的事件数与时间范围；非上传事件与非制品事件直接跳过。
 */
export function buildUploadTree(
  events: readonly AuditEvent[],
  options: { othersLabel: string },
): UploadTreeNode[] {
  const root = new Map<string, MutableNode>();

  for (const event of events) {
    if (!isUploadAction(event.action)) continue;
    const target = uploadArtifactTarget(event);
    if (!target) continue;
    const at = parseTime(event.occurredAt);
    const coords = parseUploadCoordinates(target.path);

    // 组装该事件在仓库节点下的层级链（不含仓库本身）。
    const chain: Array<{ kind: UploadNodeKind; label: string }> = [];
    if (coords.groupId && coords.artifactId && coords.version) {
      chain.push({ kind: "group", label: coords.groupId });
      chain.push({ kind: "artifact", label: coords.artifactId });
      chain.push({ kind: "version", label: coords.version });
    } else {
      chain.push({ kind: "others", label: options.othersLabel });
      if (coords.directory) chain.push({ kind: "directory", label: coords.directory });
    }
    chain.push({ kind: "file", label: coords.filename });

    // 沿链插入并累计计数 / 时间（仓库节点也计入）。
    const nodes: MutableNode[] = [];
    const repoKey = `repo:${target.repo}`;
    const repoNode = upsert(root, repoKey, "repository", target.repo);
    nodes.push(repoNode);
    let parentMap = repoNode.children;
    let prefix = repoKey;
    for (const step of chain) {
      const key = `${prefix}/${step.kind}:${step.label}`;
      const node = upsert(parentMap, key, step.kind, step.label);
      nodes.push(node);
      parentMap = node.children;
      prefix = key;
    }
    for (const node of nodes) {
      node.count += 1;
      if (at > node.latestAt) node.latestAt = at;
      if (node.earliestAt === 0 || at < node.earliestAt) node.earliestAt = at;
    }
    nodes[nodes.length - 1]!.event = event;
  }

  return finalize(root);
}

function finalize(map: Map<string, MutableNode>): UploadTreeNode[] {
  const nodes: UploadTreeNode[] = [...map.values()].map((node) => ({
    key: node.key,
    kind: node.kind,
    label: node.label,
    count: node.count,
    latestAt: node.latestAt,
    earliestAt: node.earliestAt,
    children: finalize(node.children),
    event: node.event,
  }));
  nodes.sort(compareNodes);
  return nodes;
}

function compareNodes(a: UploadTreeNode, b: UploadTreeNode): number {
  // 「其他」分支恒排最后，让可反解 GAV 的制品排前面。
  if (a.kind === "others" && b.kind !== "others") return 1;
  if (b.kind === "others" && a.kind !== "others") return -1;
  // 版本按最新上传时间倒序（最近发布的排前），比字符串排序更符合直觉。
  if (a.kind === "version" && b.kind === "version" && a.latestAt !== b.latestAt) {
    return b.latestAt - a.latestAt;
  }
  return a.label < b.label ? -1 : a.label > b.label ? 1 : 0;
}
