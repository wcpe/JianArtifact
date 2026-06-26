// 搜索结果折叠纯函数测试（FR-94）：按仓库分组、分组按仓库名升序、组内按路径升序、空输入。

import { describe, it, expect } from 'vitest';
import { buildSearchTree } from './searchTree';
import type { RepoFormat, SearchHit } from '../api/types';

/** 构造一条最小搜索命中。 */
function hit(repoId: string, repoName: string, format: RepoFormat, path: string): SearchHit {
  return {
    repo_id: repoId,
    repo_name: repoName,
    format,
    path,
    sha256: 'abc',
    size: 7,
    created_at: '2026-01-01T00:00:00Z',
  };
}

describe('buildSearchTree', () => {
  it('空输入返回空数组', () => {
    expect(buildSearchTree([])).toEqual([]);
  });

  it('单仓库多命中：聚为一组并按路径升序', () => {
    const groups = buildSearchTree([
      hit('r1', 'maven-hosted', 'maven', 'com/z/z.jar'),
      hit('r1', 'maven-hosted', 'maven', 'com/a/a.jar'),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].repoId).toBe('r1');
    expect(groups[0].format).toBe('maven');
    expect(groups[0].hits.map((h) => h.path)).toEqual(['com/a/a.jar', 'com/z/z.jar']);
  });

  it('多仓库：分组按仓库名升序，各保留自身格式', () => {
    const groups = buildSearchTree([
      hit('r2', 'npm-proxy', 'npm', 'left-pad/index.js'),
      hit('r1', 'maven-hosted', 'maven', 'com/a/a.jar'),
    ]);
    expect(groups.map((g) => g.repoName)).toEqual(['maven-hosted', 'npm-proxy']);
    expect(groups.map((g) => g.format)).toEqual(['maven', 'npm']);
  });

  it('同一仓库的命中合并到同一分组（不因顺序拆散）', () => {
    const groups = buildSearchTree([
      hit('r1', 'raw-hosted', 'raw', 'b.bin'),
      hit('r2', 'docker-hosted', 'docker', 'app/latest'),
      hit('r1', 'raw-hosted', 'raw', 'a.bin'),
    ]);
    expect(groups).toHaveLength(2);
    const raw = groups.find((g) => g.repoId === 'r1')!;
    expect(raw.hits.map((h) => h.path)).toEqual(['a.bin', 'b.bin']);
  });
});
