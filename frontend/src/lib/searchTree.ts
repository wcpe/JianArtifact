// 跨仓库搜索结果折叠（FR-94）：把后端返回的扁平命中列表折叠为「按仓库分组」的两层树。
//
// 第一层为仓库分组（携带 repo_id / repo_name / format，用于按格式渲染专属 icon）；
// 第二层为该仓库下的命中制品（保留原始 SearchHit，供点击进入详情）。
// 纯函数、无副作用，便于穷举单测；结果集本身已由后端按读权限过滤，前端不放宽。

import type { RepoFormat, SearchHit } from '../api/types';

/** 仓库分组：一个仓库下的全部搜索命中。 */
export interface SearchRepoGroup {
  /** 仓库 id（分组键，亦用于详情跳转）。 */
  repoId: string;
  /** 仓库名（展示用）。 */
  repoName: string;
  /** 仓库格式（用于按格式渲染专属 icon）。 */
  format: RepoFormat;
  /** 该仓库下的命中项（按路径升序）。 */
  hits: SearchHit[];
}

/**
 * 把扁平命中列表折叠为按仓库分组的树。
 *
 * @param hits 后端返回的搜索命中（已按读权限过滤）。
 * @returns 仓库分组数组，分组按仓库名升序，组内命中按路径升序。
 */
export function buildSearchTree(hits: SearchHit[]): SearchRepoGroup[] {
  // 按 repo_id 聚合，保留首次出现的 repo_name / format 作为分组元数据
  const groups = new Map<string, SearchRepoGroup>();

  for (const hit of hits) {
    let group = groups.get(hit.repo_id);
    if (!group) {
      group = { repoId: hit.repo_id, repoName: hit.repo_name, format: hit.format, hits: [] };
      groups.set(hit.repo_id, group);
    }
    group.hits.push(hit);
  }

  const result = [...groups.values()];
  result.sort((a, b) => a.repoName.localeCompare(b.repoName));
  for (const group of result) {
    group.hits.sort((a, b) => a.path.localeCompare(b.path));
  }
  return result;
}
