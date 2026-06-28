// 按路由动态设置浏览器标签页标题（FR-113）：document.title = 「<当前页名> - JianArtifact」。
// 路由→中文页名映射集中一处，避免散落魔法值；未命中路由回落到品牌名本身。

import { useEffect } from 'react';

/** 品牌名：标题后缀与未命中路由时的回落标题。 */
const BRAND = 'JianArtifact';

/**
 * 路由路径首段 → 中文页名映射（FR-113）。
 * 仅取 pathname 首段匹配（如 /repositories/libs 归「仓库」），覆盖各主要路由。
 */
const PATH_TITLES: Record<string, string> = {
  '': '仪表盘',
  repositories: '仓库',
  repository: '仓库详情',
  artifact: '制品详情',
  search: '搜索',
  licenses: '开源许可',
  login: '登录',
  tokens: '访问令牌',
  upload: '上传',
  users: '用户与组',
  groups: '用户与组',
  protection: '防护配置',
  monitor: '监控',
  analytics: '使用分析',
  audit: '审计日志',
  'system-logs': '系统日志',
  'protection-monitor': '防护监控',
  migration: 'Nexus 迁移',
  settings: '设置',
  system: '系统',
};

/** 由 pathname 解析中文页名：取首个非空路径段查表，未命中返回 null。 */
export function resolvePageTitle(pathname: string): string | null {
  const segment = pathname.split('/').filter(Boolean)[0] ?? '';
  return PATH_TITLES[segment] ?? null;
}

/**
 * 监听 pathname 变化、动态设置 document.title。
 * 命中映射时为「<页名> - JianArtifact」，未命中时仅显品牌名。
 */
export function useDocumentTitle(pathname: string): void {
  useEffect(() => {
    const pageTitle = resolvePageTitle(pathname);
    document.title = pageTitle ? `${pageTitle} - ${BRAND}` : BRAND;
  }, [pathname]);
}
