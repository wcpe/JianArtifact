// 跨仓库搜索结果折叠（FR-94）：把后端返回的扁平命中列表折叠为「仓库分组 → 路径层级文件夹树」。
//
// 第一层为仓库分组（携带 repo_id / repo_name / format，用于按格式渲染专属 icon）；
// 第二层起为该仓库命中按路径折叠出的**嵌套文件夹/文件树**（与仓库详情树 FR-93 一致的层级观感）。
// 命中是全量已知，故一次性建完整嵌套树（非懒加载）。折叠语义与后端 collapse_directory_entries
// 对齐：按路径段逐级下钻，目录去重、目录在前文件在后、各自按名升序。
// 纯函数、无副作用，便于穷举单测；结果集本身已由后端按读权限过滤，前端不放宽。

import type { RepoFormat, SearchHit } from '../api/types';

/** 搜索树节点：目录（含子节点）或文件（携带原始命中）。 */
export type SearchTreeNode =
  | {
      type: 'folder';
      /** 本层内的目录名（不含前缀、不含尾斜杠）。 */
      name: string;
      /** 该目录的仓库内前缀路径（以 `/` 结尾，如 `com/example/`）。 */
      path: string;
      /** 子节点（目录在前、文件在后，各按名升序）。 */
      children: SearchTreeNode[];
    }
  | {
      type: 'file';
      /** 文件名（不含前缀）。 */
      name: string;
      /** 文件的仓库内完整路径。 */
      path: string;
      /** 原始搜索命中（供点击进入详情）。 */
      hit: SearchHit;
    };

/** 仓库分组：一个仓库下命中折叠出的路径层级树。 */
export interface SearchRepoGroup {
  /** 仓库 id（分组键，亦用于详情跳转）。 */
  repoId: string;
  /** 仓库名（展示用）。 */
  repoName: string;
  /** 仓库格式（用于按格式渲染专属 icon）。 */
  format: RepoFormat;
  /** 该仓库命中折叠出的根层节点（目录在前、文件在后，各按名升序）。 */
  tree: SearchTreeNode[];
}

/** 分组累积态：保留分组元数据 + 暂存该仓库命中，待全部归集后再建树。 */
interface GroupAccumulator {
  repoId: string;
  repoName: string;
  format: RepoFormat;
  hits: SearchHit[];
}

/**
 * 把某仓库的命中列表折叠为给定前缀下的一层节点，目录递归建子树。
 *
 * @param hits 该仓库的命中（路径为仓库内完整路径）。
 * @param prefix 当前层前缀：空串表示仓库根，否则形如 `dir/`（以 `/` 结尾）。
 * @returns 一层节点，目录在前、文件在后，各自按名升序。
 */
function buildNodes(hits: SearchHit[], prefix: string): SearchTreeNode[] {
  // 子目录名 → 落在该子目录下的命中（用于递归）；文件名 → 该文件命中
  const folderHits = new Map<string, SearchHit[]>();
  const files = new Map<string, SearchHit>();

  for (const hit of hits) {
    // 仅看落在前缀下的命中（防御性：不以前缀开头的跳过）
    if (!hit.path.startsWith(prefix)) continue;
    const relative = hit.path.slice(prefix.length);
    if (relative.length === 0) continue;
    const slash = relative.indexOf('/');
    if (slash >= 0) {
      // 第一段后仍有 `/` → 子目录（按目录名归集命中，递归时再下钻）
      const first = relative.slice(0, slash);
      if (first.length === 0) continue; // 防御空段（病态路径）
      const bucket = folderHits.get(first);
      if (bucket) bucket.push(hit);
      else folderHits.set(first, [hit]);
    } else {
      // 当前层文件（同名以最后一条为准，与后端去重一致）
      files.set(relative, hit);
    }
  }

  const folderNodes: SearchTreeNode[] = [...folderHits.keys()]
    .sort((a, b) => a.localeCompare(b))
    .map((name) => {
      const childPrefix = `${prefix}${name}/`;
      return {
        type: 'folder' as const,
        name,
        path: childPrefix,
        children: buildNodes(folderHits.get(name)!, childPrefix),
      };
    });

  const fileNodes: SearchTreeNode[] = [...files.keys()]
    .sort((a, b) => a.localeCompare(b))
    .map((name) => {
      const hit = files.get(name)!;
      return { type: 'file' as const, name, path: hit.path, hit };
    });

  return [...folderNodes, ...fileNodes];
}

/**
 * 把扁平命中列表折叠为「仓库分组 → 路径层级树」。
 *
 * @param hits 后端返回的搜索命中（已按读权限过滤）。
 * @returns 仓库分组数组，分组按仓库名升序，组内为按路径折叠的嵌套文件夹/文件树。
 */
export function buildSearchTree(hits: SearchHit[]): SearchRepoGroup[] {
  // 按 repo_id 聚合命中，保留首次出现的 repo_name / format 作为分组元数据
  const groups = new Map<string, GroupAccumulator>();

  for (const hit of hits) {
    let group = groups.get(hit.repo_id);
    if (!group) {
      group = { repoId: hit.repo_id, repoName: hit.repo_name, format: hit.format, hits: [] };
      groups.set(hit.repo_id, group);
    }
    group.hits.push(hit);
  }

  return [...groups.values()]
    .sort((a, b) => a.repoName.localeCompare(b.repoName))
    .map((g) => ({
      repoId: g.repoId,
      repoName: g.repoName,
      format: g.format,
      tree: buildNodes(g.hits, ''),
    }));
}
