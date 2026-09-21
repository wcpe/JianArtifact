// 将扁平 asset 路径列表拼成目录树（前端拼树，不依赖后端树 API）。
import type { AssetSummary } from "../api/types";

export type AssetTreeNode = {
  name: string;
  /** 相对仓库根的完整路径（文件为 asset.path；目录为无尾斜杠的前缀） */
  path: string;
  kind: "dir" | "file";
  children?: AssetTreeNode[];
  asset?: AssetSummary;
  /** FR-142：累计完整下载次数（仅来自树 API 的文件节点持有；搜索拼树不带） */
  downloadCount?: number;
};

type MutableNode = {
  name: string;
  path: string;
  kind: "dir" | "file";
  children: Map<string, MutableNode>;
  asset?: AssetSummary;
};

/** 按 path 升序的扁平列表 → 目录优先的树。 */
export function buildAssetTree(assets: AssetSummary[]): AssetTreeNode[] {
  const root = new Map<string, MutableNode>();

  for (const asset of assets) {
    const parts = asset.path.split("/").filter(Boolean);
    if (parts.length === 0) {
      continue;
    }
    let level = root;
    let prefix = "";
    for (let i = 0; i < parts.length; i++) {
      const part = parts[i]!;
      prefix = prefix ? `${prefix}/${part}` : part;
      const isFile = i === parts.length - 1;
      let node = level.get(part);
      if (!node) {
        node = {
          name: part,
          path: prefix,
          kind: isFile ? "file" : "dir",
          children: new Map(),
        };
        level.set(part, node);
      }
      if (isFile) {
        node.kind = "file";
        node.asset = asset;
      } else {
        node.kind = "dir";
        level = node.children;
      }
    }
  }

  return mapToArray(root);
}

function mapToArray(m: Map<string, MutableNode>): AssetTreeNode[] {
  return [...m.values()]
    .sort((a, b) => {
      if (a.kind !== b.kind) {
        return a.kind === "dir" ? -1 : 1;
      }
      return a.name.localeCompare(b.name);
    })
    .map((n) => ({
      name: n.name,
      path: n.path,
      kind: n.kind,
      asset: n.asset,
      children: n.kind === "dir" ? mapToArray(n.children) : undefined,
    }));
}

/** 协议层下载 URL（同源相对路径）。 */
export function assetDownloadUrl(repo: string, format: string, path: string): string {
  const enc = path
    .split("/")
    .filter(Boolean)
    .map((s) => encodeURIComponent(s))
    .join("/");
  if (format === "npm") {
    return `/npm/${encodeURIComponent(repo)}/${enc}`;
  }
  return `/repository/${encodeURIComponent(repo)}/${enc}`;
}

/** 人类可读文件大小（实现迁移至 lib/format，此处保留导出以兼容既有引用）。 */
export { formatBytes } from "./format";
