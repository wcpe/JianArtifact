// 控制台布局外壳测试（FR-92 折叠图标导航条 + 角色门控 + fix-B 高亮）：
// 1) 默认窄（图标条，仅 aria-label，无可见文字）/ 点击切换可展开为图标+文字、再切回；
// 2) 角色门控：非 Admin 看不到管理入口，Admin 看得到；
// 3) active 高亮按路径段精确匹配（保持 fix-B）——/protection 不被 /protection-monitor 串台。

import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { AppLayout } from './AppLayout';
import { AuthContext, type AuthContextValue } from '../auth/AuthContext';

/** 探针：把当前路由的 pathname + search 暴露到 DOM，供断言页眉搜索跳转。 */
function LocationProbe() {
  const location = useLocation();
  return <div data-testid="location-probe">{location.pathname + location.search}</div>;
}

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

/** 构造普通用户认证上下文（管理入口应隐藏）。 */
function 普通用户上下文(): AuthContextValue {
  return {
    user: { id: 'u-user', username: 'alice', role: 'user' },
    loading: false,
    isAdmin: false,
    signIn: async () => {},
    signOut: async () => {},
  };
}

/** 在指定初始路由与认证上下文下渲染布局外壳（附带定位探针）。 */
function renderAt(initialPath: string, ctx: AuthContextValue = 管理员上下文()) {
  return render(
    <MantineProvider>
      <AuthContext.Provider value={ctx}>
        <MemoryRouter initialEntries={[initialPath]}>
          <AppLayout />
          <LocationProbe />
        </MemoryRouter>
      </AuthContext.Provider>
    </MantineProvider>,
  );
}

/** 取导航项的锚点元素（带 data-active 属性，窄态靠 aria-label 定位）。 */
function navLinkByLabel(label: string): HTMLElement {
  // 窄态无可见文字，统一经无障碍名（aria-label）定位锚点
  const el = screen.getByLabelText(label);
  const anchor = el.closest('a');
  if (!anchor) {
    throw new Error(`未找到导航项「${label}」对应的锚点元素`);
  }
  return anchor;
}

describe('AppLayout 折叠图标导航条', () => {
  it('默认窄：导航项仅有 aria-label、无可见文字，提供折叠切换', () => {
    renderAt('/');

    // 窄态：靠 aria-label 可达，但文字不直接可见
    expect(screen.getByLabelText('仪表盘')).toBeInTheDocument();
    expect(screen.queryByText('仪表盘')).not.toBeInTheDocument();
    // 折叠态展示「展开导航」切换控件
    expect(screen.getByLabelText('展开导航')).toBeInTheDocument();
  });

  it('点击展开后导航项显示文字，再点击收起回到窄态', async () => {
    const user = userEvent.setup();
    renderAt('/');

    await user.click(screen.getByLabelText('展开导航'));

    // 展开态：文字可见，切换控件变为「收起导航」
    expect(screen.getByText('仪表盘')).toBeInTheDocument();
    expect(screen.getByLabelText('收起导航')).toBeInTheDocument();

    await user.click(screen.getByLabelText('收起导航'));

    // 收回窄态：文字再次隐藏
    expect(screen.queryByText('仪表盘')).not.toBeInTheDocument();
    expect(screen.getByLabelText('展开导航')).toBeInTheDocument();
  });

  it('窄态每个导航项有可访问名（aria-label）', () => {
    renderAt('/');

    expect(navLinkByLabel('仓库管理')).toBeInTheDocument();
    expect(navLinkByLabel('制品搜索')).toBeInTheDocument();
  });
});

describe('AppLayout 角色门控入口', () => {
  // 管理类入口清单：仅 Admin 可见。
  // FR-99：使用分析 / 审计日志 / 防护监控三个独立入口已收敛为统一「监控」入口。
  const 管理入口 = ['用户管理', '用户组管理', '监控', '防护配置', 'Nexus 迁移', '设置'];
  // 通用入口：所有登录用户可见
  const 通用入口 = ['仪表盘', '仓库管理', '制品搜索', 'Token 管理', '制品上传'];

  it('普通用户看不到任何管理入口，但看得到通用入口', () => {
    renderAt('/', 普通用户上下文());

    for (const label of 管理入口) {
      expect(screen.queryByLabelText(label)).not.toBeInTheDocument();
    }
    for (const label of 通用入口) {
      expect(screen.getByLabelText(label)).toBeInTheDocument();
    }
  });

  it('管理员看得到全部管理入口', () => {
    renderAt('/', 管理员上下文());

    for (const label of [...管理入口, ...通用入口]) {
      expect(screen.getByLabelText(label)).toBeInTheDocument();
    }
  });
});

describe('AppLayout 侧栏导航高亮（fix-B 段精确匹配）', () => {
  it('位于 /monitor 时仅「监控」高亮，「防护配置」不串台（FR-99 整合入口）', () => {
    renderAt('/monitor');

    expect(navLinkByLabel('监控').getAttribute('data-active')).toBe('true');
    expect(navLinkByLabel('防护配置').getAttribute('data-active')).toBeNull();
  });

  it('位于 /protection 时仅「防护配置」高亮，不被「监控」串台', () => {
    renderAt('/protection');

    expect(navLinkByLabel('防护配置').getAttribute('data-active')).toBe('true');
    expect(navLinkByLabel('监控').getAttribute('data-active')).toBeNull();
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

describe('AppLayout 页眉全局搜索（FR-94）', () => {
  it('页眉搜索框可用（非禁用），有可访问名', () => {
    renderAt('/');
    const input = screen.getByLabelText('全局搜索');
    expect(input).toBeInTheDocument();
    expect(input).not.toBeDisabled();
  });

  it('输入关键字回车 → 跳转 /search?q=<关键字>', async () => {
    const user = userEvent.setup();
    renderAt('/');

    const input = screen.getByLabelText('全局搜索');
    await user.type(input, 'lib-core{enter}');

    expect(screen.getByTestId('location-probe')).toHaveTextContent('/search?q=lib-core');
  });

  it('关键字含特殊字符时按 URL 编码跳转', async () => {
    const user = userEvent.setup();
    renderAt('/');

    const input = screen.getByLabelText('全局搜索');
    await user.type(input, 'a b{enter}');

    // 空格被编码为 +（URLSearchParams 序列化），断言落到搜索页且携带 q
    const probe = screen.getByTestId('location-probe');
    expect(probe.textContent).toContain('/search?q=');
    expect(probe.textContent).toContain('a+b');
  });

  it('空关键字回车不跳转（停留原路由）', async () => {
    const user = userEvent.setup();
    renderAt('/');

    const input = screen.getByLabelText('全局搜索');
    await user.type(input, '   {enter}');

    expect(screen.getByTestId('location-probe')).toHaveTextContent('/');
    expect(screen.getByTestId('location-probe')).not.toHaveTextContent('/search');
  });
});
