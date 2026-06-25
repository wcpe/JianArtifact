// 目录浏览折叠（FR-76）：把仓库内制品的扁平路径列表折叠为「当前目录一层」的条目。
//
// 与后端 collapse_directory_entries 语义对齐：给定一个目录前缀（空串为仓库根，否则以 `/`
// 结尾），只取每条路径相对前缀的第一段——第一段后仍有 `/` 的归为子目录（去重），否则为文件。
// 纯函数、无副作用，便于单测；前端据此在客户端构造可逐级展开的目录树。

import type { ArtifactDto } from '../api/types';

/** 目录项：子目录或文件。文件携带其完整路径与元数据，子目录仅有名称。 */
export interface BrowseEntry {
  /** 本层内的条目名（不含前缀，不含尾斜杠）。 */
  name: string;
  /** 类型。 */
  type: 'folder' | 'file';
  /** 文件的仓库内完整路径（子目录为 undefined）。 */
  path?: string;
  /** 文件字节大小（子目录为 undefined）。 */
  size?: number;
  /** 是否为代理缓存制品（子目录为 undefined）。 */
  cached?: boolean;
  /** 文件创建时间（子目录为 undefined）。 */
  createdAt?: string;
}

/**
 * 把制品扁平列表折叠为给定前缀下的一层目录项。
 *
 * @param artifacts 仓库内全部制品（路径为仓库内完整路径）。
 * @param prefix 目录前缀：空串表示仓库根，否则形如 `dir/`（以 `/` 结尾）。
 * @returns 一层条目，目录在前、文件在后，各自按名称升序。
 */
export function buildDirectoryListing(artifacts: ArtifactDto[], prefix: string): BrowseEntry[] {
  const folders = new Set<string>();
  const files: BrowseEntry[] = [];

  for (const art of artifacts) {
    // 只看落在前缀下的制品
    if (!art.path.startsWith(prefix)) continue;
    const relative = art.path.slice(prefix.length);
    if (relative.length === 0) continue;
    const slash = relative.indexOf('/');
    if (slash >= 0) {
      // 第一段后仍有 `/` → 子目录（取第一段，去重）
      const first = relative.slice(0, slash);
      if (first.length > 0) folders.add(first);
    } else {
      // 当前层文件
      files.push({
        name: relative,
        type: 'file',
        path: art.path,
        size: art.size,
        cached: art.cached,
        createdAt: art.created_at,
      });
    }
  }

  const folderEntries: BrowseEntry[] = [...folders]
    .sort((a, b) => a.localeCompare(b))
    .map((name) => ({ name, type: 'folder' as const }));
  files.sort((a, b) => a.name.localeCompare(b.name));

  return [...folderEntries, ...files];
}

/**
 * 把目录前缀拆为面包屑分段（用于导航）。
 *
 * @param prefix 目录前缀（空串为根，否则以 `/` 结尾）。
 * @returns 各级 [显示名, 该级前缀] 对；根不含分段。
 */
export function breadcrumbSegments(prefix: string): { name: string; prefix: string }[] {
  if (prefix.length === 0) return [];
  const parts = prefix.replace(/\/$/, '').split('/');
  const segments: { name: string; prefix: string }[] = [];
  let acc = '';
  for (const part of parts) {
    acc += `${part}/`;
    segments.push({ name: part, prefix: acc });
  }
  return segments;
}
