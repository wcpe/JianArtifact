// 控制台布局侧栏导航高亮测试（fix-B）：
// 防止前缀匹配导致的高亮串台 —— /protection 是 /protection-monitor 的前缀，
// 进入「防护监控」页时「防护配置」不得被误判为 active；任一时刻只高亮当前页对应项。

import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { MemoryRouter } from 'react-router-dom';
import { AppLayout } from './AppLayout';
import { AuthContext, type AuthContextValue } from '../auth/AuthContext';

/** 构造管理员认证上下文（导航全部可见）。 */
function 管理员上下文(): AuthContextValue {
  return {
    user: { id: 'u-admin', username: 'admin', role: 'admin' },
    loading: false,
    isAdmin: true,
    signIn: async () => {},
    signOut: async () => {},
  };
}

/** 在指定初始路由下渲染布局外壳。 */
function renderAt(initialPath: string) {
  return render(
    <MantineProvider>
      <AuthContext.Provider value={管理员上下文()}>
        <MemoryRouter initialEntries={[initialPath]}>
          <AppLayout />
        </MemoryRouter>
      </AuthContext.Provider>
    </MantineProvider>,
  );
}

/** 取导航项的 NavLink 根元素（带 data-active 属性）。 */
function navLinkByLabel(label: string): HTMLElement {
  const labelEl = screen.getByText(label);
  // Mantine NavLink 的 data-active 落在最外层 <a> 上，向上找到带 href 的锚点
  const anchor = labelEl.closest('a');
  if (!anchor) {
    throw new Error(`未找到导航项「${label}」对应的锚点元素`);
  }
  return anchor;
}

describe('AppLayout 侧栏导航高亮', () => {
  it('位于 /protection-monitor 时仅「防护监控」高亮，「防护配置」不串台', () => {
    renderAt('/protection-monitor');

    expect(navLinkByLabel('防护监控').getAttribute('data-active')).toBe('true');
    expect(navLinkByLabel('防护配置').getAttribute('data-active')).toBeNull();
  });

  it('位于 /protection 时仅「防护配置」高亮', () => {
    renderAt('/protection');

    expect(navLinkByLabel('防护配置').getAttribute('data-active')).toBe('true');
    expect(navLinkByLabel('防护监控').getAttribute('data-active')).toBeNull();
  });

  it('位于子路径 /repositories/libs 时「仓库管理」仍高亮（按段匹配）', () => {
    renderAt('/repositories/libs');

    expect(navLinkByLabel('仓库管理').getAttribute('data-active')).toBe('true');
  });

  it('位于根路径 / 时仅「仪表盘」高亮，不波及其他项', () => {
    renderAt('/');

    expect(navLinkByLabel('仪表盘').getAttribute('data-active')).toBe('true');
    expect(navLinkByLabel('仓库管理').getAttribute('data-active')).toBeNull();
  });
});
