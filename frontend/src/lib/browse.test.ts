// 目录浏览折叠纯函数测试（FR-76）：与后端语义对齐——一层下钻、子目录去重、面包屑拆分。

import { describe, it, expect } from 'vitest';
import { buildDirectoryListing, breadcrumbSegments } from './browse';
import type { ArtifactDto } from '../api/types';

/** 构造一条最小制品。 */
function art(path: string, size = 7): ArtifactDto {
  return {
    path,
    size,
    sha256: 'abc',
    content_type: null,
    cached: false,
    created_at: '2026-01-01T00:00:00Z',
  };
}

describe('buildDirectoryListing', () => {
  it('一层折叠：区分子目录与文件并对子目录去重', () => {
    const arts = [art('dir/a.txt'), art('dir/sub/b.txt'), art('dir/sub/c.txt'), art('dir/z.bin')];
    const entries = buildDirectoryListing(arts, 'dir/');
    // 目录在前（sub），文件在后（a.txt、z.bin）
    expect(entries.map((e) => [e.name, e.type])).toEqual([
      ['sub', 'folder'],
      ['a.txt', 'file'],
      ['z.bin', 'file'],
    ]);
    // 文件携带完整路径与元数据
    const a = entries.find((e) => e.name === 'a.txt')!;
    expect(a.path).toBe('dir/a.txt');
    expect(a.size).toBe(7);
  });

  it('根目录（空前缀）取第一段', () => {
    const entries = buildDirectoryListing([art('dir/a.txt'), art('top.txt')], '');
    expect(entries.map((e) => [e.name, e.type])).toEqual([
      ['dir', 'folder'],
      ['top.txt', 'file'],
    ]);
  });

  it('不扁平铺开深层文件', () => {
    const entries = buildDirectoryListing([art('dir/sub/b.txt')], 'dir/');
    expect(entries).toHaveLength(1);
    expect(entries[0]).toMatchObject({ name: 'sub', type: 'folder' });
  });

  it('不串入兄弟前缀', () => {
    const entries = buildDirectoryListing([art('docs/r.txt'), art('docsx/n.txt')], 'docs/');
    expect(entries.map((e) => e.name)).toEqual(['r.txt']);
  });
});

describe('breadcrumbSegments', () => {
  it('根前缀无分段', () => {
    expect(breadcrumbSegments('')).toEqual([]);
  });

  it('逐级累积前缀', () => {
    expect(breadcrumbSegments('a/b/')).toEqual([
      { name: 'a', prefix: 'a/' },
      { name: 'b', prefix: 'a/b/' },
    ]);
  });
});
