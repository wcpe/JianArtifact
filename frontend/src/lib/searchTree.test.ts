// 搜索结果树纯函数测试（FR-94）：仓库分组 + 组内按路径折叠为嵌套文件夹/文件树。
// 穷举：空输入、单仓库多命中嵌套、多仓库分组升序、同仓库命中合并、目录在前文件在后各升序、
// 深层嵌套层级正确、文件叶子携带原始命中、同名兄弟目录去重合并。

import { describe, it, expect } from 'vitest';
import { buildSearchTree, type SearchTreeNode } from './searchTree';
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

/** 取节点 [名称, 类型] 对，便于断言一层结构。 */
function shape(nodes: SearchTreeNode[]): [string, string][] {
  return nodes.map((n) => [n.name, n.type]);
}

describe('buildSearchTree', () => {
  it('空输入返回空数组', () => {
    expect(buildSearchTree([])).toEqual([]);
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
    // 组内根层两文件按名升序
    expect(shape(raw.tree)).toEqual([
      ['a.bin', 'file'],
      ['b.bin', 'file'],
    ]);
  });

  it('单仓库多命中：组内折叠为嵌套文件夹/文件树（目录在前、文件在后、各升序）', () => {
    const groups = buildSearchTree([
      hit('r1', 'maven-hosted', 'maven', 'com/z/z.jar'),
      hit('r1', 'maven-hosted', 'maven', 'com/a/a.jar'),
      hit('r1', 'maven-hosted', 'maven', 'top.txt'),
    ]);
    expect(groups).toHaveLength(1);
    const root = groups[0].tree;
    // 根层：com（目录）在前，top.txt（文件）在后
    expect(shape(root)).toEqual([
      ['com', 'folder'],
      ['top.txt', 'file'],
    ]);

    const com = root[0];
    if (com.type !== 'folder') throw new Error('com 应为目录');
    // com/ 下两个子目录按名升序
    expect(shape(com.children)).toEqual([
      ['a', 'folder'],
      ['z', 'folder'],
    ]);
    // com/ 前缀路径
    expect(com.path).toBe('com/');
  });

  it('文件叶子携带原始命中（供点击跳转详情）', () => {
    const h = hit('r1', 'maven-hosted', 'maven', 'com/a/a.jar');
    const groups = buildSearchTree([h]);
    const com = groups[0].tree[0];
    if (com.type !== 'folder') throw new Error('com 应为目录');
    const a = com.children[0];
    if (a.type !== 'folder') throw new Error('a 应为目录');
    const file = a.children[0];
    if (file.type !== 'file') throw new Error('叶子应为文件');
    expect(file.name).toBe('a.jar');
    expect(file.path).toBe('com/a/a.jar');
    expect(file.hit).toBe(h); // 同一引用
  });

  it('深层嵌套：逐级成树而非扁平铺开', () => {
    const groups = buildSearchTree([hit('r1', 'm', 'maven', 'a/b/c/d.jar')]);
    let level = groups[0].tree;
    expect(shape(level)).toEqual([['a', 'folder']]);
    expect((level[0] as Extract<SearchTreeNode, { type: 'folder' }>).path).toBe('a/');
    level = (level[0] as Extract<SearchTreeNode, { type: 'folder' }>).children;
    expect(shape(level)).toEqual([['b', 'folder']]);
    expect((level[0] as Extract<SearchTreeNode, { type: 'folder' }>).path).toBe('a/b/');
    level = (level[0] as Extract<SearchTreeNode, { type: 'folder' }>).children;
    expect(shape(level)).toEqual([['c', 'folder']]);
    expect((level[0] as Extract<SearchTreeNode, { type: 'folder' }>).path).toBe('a/b/c/');
    level = (level[0] as Extract<SearchTreeNode, { type: 'folder' }>).children;
    expect(shape(level)).toEqual([['d.jar', 'file']]);
  });

  it('同名兄弟目录合并去重（同前缀下多命中归一棵子树）', () => {
    const groups = buildSearchTree([
      hit('r1', 'm', 'maven', 'com/example/a.jar'),
      hit('r1', 'm', 'maven', 'com/example/b.jar'),
    ]);
    const com = groups[0].tree[0];
    if (com.type !== 'folder') throw new Error('com 应为目录');
    expect(shape(com.children)).toEqual([['example', 'folder']]);
    const example = com.children[0];
    if (example.type !== 'folder') throw new Error('example 应为目录');
    // example/ 下两文件按名升序
    expect(shape(example.children)).toEqual([
      ['a.jar', 'file'],
      ['b.jar', 'file'],
    ]);
  });

  it('文件与同名目录可在同层共存（互不吞没）', () => {
    const groups = buildSearchTree([
      hit('r1', 'm', 'raw', 'app'),
      hit('r1', 'm', 'raw', 'app/inner.txt'),
    ]);
    const root = groups[0].tree;
    // 同层应同时出现目录 app 与文件 app：目录在前
    expect(shape(root)).toEqual([
      ['app', 'folder'],
      ['app', 'file'],
    ]);
  });
});
