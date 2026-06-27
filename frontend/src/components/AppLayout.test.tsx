// 控制台布局外壳测试（FR-92 折叠图标导航条 + 角色门控 + fix-B 高亮）：
// 1) 默认窄（图标条，仅 aria-label，无可见文字）/ 点击切换可展开为图标+文字、再切回；
// 2) 角色门控：非 Admin 看不到管理入口，Admin 看得到；
// 3) active 高亮按路径段精确匹配（保持 fix-B）——/protection 不被 /protection-monitor 串台。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { AppLayout } from './AppLayout';
import { AuthContext, type AuthContextValue } from '../auth/AuthContext';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type { HealthInfo, UpdateCheck } from '../api/types';

/** 样例健康响应（含构建版本号）。 */
const 样例健康: HealthInfo = { status: 'ok', version: '0.4.0', port: 9999 };

/** 有可用更新的更新检查结果。 */
const 有更新检查: UpdateCheck = {
  current_version: '0.4.0',
  latest_version: '0.5.0',
  update_available: true,
  asset_name: 'jianartifact-x86_64',
  notes: '',
};

/** 无可用更新的更新检查结果。 */
const 无更新检查: UpdateCheck = {
  current_version: '0.5.0',
  latest_version: '0.5.0',
  update_available: false,
  asset_name: 'jianartifact-x86_64',
  notes: '',
};

beforeEach(() => {
  // 默认：健康检查成功、更新检查未启用（409），各用例按需覆盖
  vi.spyOn(api, 'getHealth').mockResolvedValue(样例健康);
  vi.spyOn(api, 'checkUpdate').mockRejectedValue(new ApiError(409, 'conflict', '在线更新未启用'));
});

afterEach(() => {
  vi.restoreAllMocks();
});

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

/** 构造匿名访客认证上下文（未登录、会话已恢复完毕）。 */
function 匿名上下文(): AuthContextValue {
  return {
    user: null,
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

describe('AppLayout 匿名访客 shell（FR-95）', () => {
  // 公开导航项：匿名可见
  const 公开导航 = ['仓库管理', '制品搜索'];
  // 受限入口：匿名一律不可见（含通用登录项与全部管理项）
  const 受限入口 = [
    '仪表盘',
    'Token 管理',
    '制品上传',
    '用户管理',
    '用户组管理',
    '监控',
    '防护配置',
    'Nexus 迁移',
    '设置',
  ];

  it('匿名态页眉显示「登录」按钮，不显示用户名与登出', () => {
    renderAt('/repositories', 匿名上下文());

    expect(screen.getByRole('button', { name: '登录' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '登出' })).not.toBeInTheDocument();
  });

  it('匿名态导航只留公开项，不显示任何受限 / 管理 / 上传入口', () => {
    renderAt('/repositories', 匿名上下文());

    for (const label of 公开导航) {
      expect(screen.getByLabelText(label)).toBeInTheDocument();
    }
    for (const label of 受限入口) {
      expect(screen.queryByLabelText(label)).not.toBeInTheDocument();
    }
  });

  it('匿名态保留页眉全局搜索（FR-94 不回归）', () => {
    renderAt('/repositories', 匿名上下文());

    const input = screen.getByLabelText('全局搜索');
    expect(input).toBeInTheDocument();
    expect(input).not.toBeDisabled();
  });

  it('点击页眉「登录」跳转到 /login', async () => {
    const user = userEvent.setup();
    renderAt('/repositories', 匿名上下文());

    await user.click(screen.getByRole('button', { name: '登录' }));

    expect(screen.getByTestId('location-probe')).toHaveTextContent('/login');
  });
});

describe('AppLayout 更新徽标（FR-101）', () => {
  it('Admin 且有可用更新：显示「更新: cur → latest」徽标，点击跳 /settings', async () => {
    const user = userEvent.setup();
    vi.spyOn(api, 'checkUpdate').mockResolvedValue(有更新检查);
    renderAt('/', 管理员上下文());

    const badge = await screen.findByLabelText('有可用更新，点击前往设置页升级');
    expect(badge).toHaveTextContent('更新');
    expect(badge).toHaveTextContent('0.4.0');
    expect(badge).toHaveTextContent('0.5.0');

    await user.click(badge);
    expect(screen.getByTestId('location-probe')).toHaveTextContent('/settings');
  });

  it('Admin 但无可用更新：不显示徽标', async () => {
    vi.spyOn(api, 'checkUpdate').mockResolvedValue(无更新检查);
    renderAt('/', 管理员上下文());

    // 等更新检查 resolve 后断言徽标不渲染
    await waitFor(() => expect(api.checkUpdate).toHaveBeenCalled());
    expect(screen.queryByLabelText('有可用更新，点击前往设置页升级')).not.toBeInTheDocument();
  });

  it('Admin 但在线更新未启用（409）：静默不显徽标，不阻塞渲染', async () => {
    // beforeEach 默认 checkUpdate 抛 409
    renderAt('/', 管理员上下文());

    await waitFor(() => expect(api.checkUpdate).toHaveBeenCalled());
    expect(screen.queryByLabelText('有可用更新，点击前往设置页升级')).not.toBeInTheDocument();
    // 外壳照常渲染（导航可用）
    expect(screen.getByLabelText('仪表盘')).toBeInTheDocument();
  });

  it('非 Admin（普通用户）：不查更新、不显徽标', async () => {
    vi.spyOn(api, 'checkUpdate').mockResolvedValue(有更新检查);
    renderAt('/', 普通用户上下文());

    await waitFor(() => expect(api.getHealth).toHaveBeenCalled());
    expect(api.checkUpdate).not.toHaveBeenCalled();
    expect(screen.queryByLabelText('有可用更新，点击前往设置页升级')).not.toBeInTheDocument();
  });

  it('匿名访客：不查更新、不显徽标', async () => {
    vi.spyOn(api, 'checkUpdate').mockResolvedValue(有更新检查);
    renderAt('/repositories', 匿名上下文());

    await waitFor(() => expect(api.getHealth).toHaveBeenCalled());
    expect(api.checkUpdate).not.toHaveBeenCalled();
    expect(screen.queryByLabelText('有可用更新，点击前往设置页升级')).not.toBeInTheDocument();
  });
});

describe('AppLayout 底部版本号与开源许可入口（FR-101）', () => {
  it('展开态：底部显示当前版本号 v{version} 文字', async () => {
    const user = userEvent.setup();
    renderAt('/', 匿名上下文());

    await user.click(screen.getByLabelText('展开导航'));
    expect(await screen.findByText('v0.4.0')).toBeInTheDocument();
  });

  it('匿名访客也能看到版本号（所有用户可见）', async () => {
    renderAt('/repositories', 匿名上下文());
    // 折叠态版本号经 aria-label / Tooltip 呈现
    expect(await screen.findByLabelText('当前版本 v0.4.0')).toBeInTheDocument();
  });

  it('健康检查失败：版本号区静默不渲染，不阻塞外壳', async () => {
    vi.spyOn(api, 'getHealth').mockRejectedValue(new ApiError(500, 'error', '失败'));
    renderAt('/', 匿名上下文());

    await waitFor(() => expect(api.getHealth).toHaveBeenCalled());
    expect(screen.queryByLabelText(/当前版本/)).not.toBeInTheDocument();
    // 外壳照常渲染
    expect(screen.getByLabelText('仓库管理')).toBeInTheDocument();
  });

  it('开源许可按钮点击跳转 /licenses', async () => {
    const user = userEvent.setup();
    renderAt('/', 匿名上下文());

    await user.click(screen.getByLabelText('开源许可'));
    expect(screen.getByTestId('location-probe')).toHaveTextContent('/licenses');
  });

  it('折叠态：开源许可入口经 aria-label 可达（icon + Tooltip）', () => {
    renderAt('/', 匿名上下文());
    // 折叠（窄）态：无可见「开源许可」文字，但 aria-label 可达
    expect(screen.getByLabelText('开源许可')).toBeInTheDocument();
    expect(screen.queryByText('开源许可')).not.toBeInTheDocument();
  });

  it('展开态：开源许可入口显示可见文字', async () => {
    const user = userEvent.setup();
    renderAt('/', 匿名上下文());

    await user.click(screen.getByLabelText('展开导航'));
    expect(screen.getByText('开源许可')).toBeInTheDocument();
  });
});
