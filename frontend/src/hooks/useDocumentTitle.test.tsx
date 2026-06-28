// 动态标题 hook 测试（FR-113）：路由→中文页名映射 + 监听 pathname 设置 document.title。

import { describe, it, expect } from 'vitest';
import { renderHook } from '@testing-library/react';
import { resolvePageTitle, useDocumentTitle } from './useDocumentTitle';

describe('resolvePageTitle 路由→中文页名映射', () => {
  it('根路径解析为「仪表盘」', () => {
    expect(resolvePageTitle('/')).toBe('仪表盘');
  });

  it('各主要路由首段映射到对应中文页名', () => {
    expect(resolvePageTitle('/repositories')).toBe('仓库');
    expect(resolvePageTitle('/search')).toBe('搜索');
    expect(resolvePageTitle('/settings')).toBe('设置');
    expect(resolvePageTitle('/system')).toBe('系统');
    expect(resolvePageTitle('/system-logs')).toBe('系统日志');
    expect(resolvePageTitle('/migration')).toBe('Nexus 迁移');
  });

  it('按首段匹配：子路径归属父页名（/repositories/libs → 仓库）', () => {
    expect(resolvePageTitle('/repositories/libs')).toBe('仓库');
  });

  it('未命中的路由返回 null', () => {
    expect(resolvePageTitle('/unknown-route')).toBeNull();
  });
});

describe('useDocumentTitle 设置 document.title', () => {
  it('命中路由：标题为「<页名> - JianArtifact」', () => {
    renderHook(() => useDocumentTitle('/settings'));
    expect(document.title).toBe('设置 - JianArtifact');
  });

  it('根路径：标题为「仪表盘 - JianArtifact」', () => {
    renderHook(() => useDocumentTitle('/'));
    expect(document.title).toBe('仪表盘 - JianArtifact');
  });

  it('pathname 变化时标题随之更新', () => {
    const { rerender } = renderHook(({ p }) => useDocumentTitle(p), {
      initialProps: { p: '/search' },
    });
    expect(document.title).toBe('搜索 - JianArtifact');

    rerender({ p: '/audit' });
    expect(document.title).toBe('审计日志 - JianArtifact');
  });

  it('未命中路由：标题回落为品牌名 JianArtifact', () => {
    renderHook(() => useDocumentTitle('/unknown-route'));
    expect(document.title).toBe('JianArtifact');
  });
});
