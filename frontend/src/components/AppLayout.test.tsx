// 控制台外壳重做测试（FR-92：logo 区 + 分段导航 + 点 logo 切换 + 左下许可 + 全局 max-width）：
// 1) 左上 logo 区显 SVG + 「JianArtifact」+ 小灰字版本号，点「logo+文字」整体切换展开/收起；
// 2) 分段导航（浏览 / 管理 / 系统·监控），删「使用分析」入口、加「系统日志」入口跳 /system-logs；
// 3) 左下 footer：展开显许可+收起按钮、收起隐藏许可只留展开按钮；
// 4) 不回归：角色门控（FR-95）、active 段精确匹配（fix-B）、页眉搜索（FR-94）、更新徽标（FR-101）、内容区 max-width。

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

/** 探针：把当前路由的 pathname + search 暴露到 DOM，供断言跳转。 */
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

/** 在指定初始路由与认证上下文下渲染外壳（附带定位探针）。 */
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

/** 取导航项的锚点元素（带 data-active 属性，收起态靠 aria-label 定位）。 */
function navLinkByLabel(label: string): HTMLElement {
  // 收起态无可见文字，统一经无障碍名（aria-label）定位锚点
  const el = screen.getByLabelText(label);
  const anchor = el.closest('a');
  if (!anchor) {
    throw new Error(`未找到导航项「${label}」对应的锚点元素`);
  }
  return anchor;
}

describe('AppLayout 左上 logo 区（品牌 + 版本号 + 点 logo 切换）', () => {
  it('默认收起态：logo 区只显 SVG，文字「JianArtifact」隐藏，仍可点击展开', () => {
    renderAt('/');

    // 品牌 logo 切换控件可达（带 aria-label / 切换语义），但品牌文字收起态不直接可见
    const toggle = screen.getByLabelText('切换导航展开收起');
    expect(toggle).toBeInTheDocument();
    expect(screen.queryByText('JianArtifact')).not.toBeInTheDocument();
  });

  it('点击 logo（含文字）整体切换：展开后显「JianArtifact」与版本号，再点收起', async () => {
    const user = userEvent.setup();
    renderAt('/');

    await user.click(screen.getByLabelText('切换导航展开收起'));

    // 展开态：品牌文字与小灰字版本号可见
    expect(screen.getByText('JianArtifact')).toBeInTheDocument();
    expect(await screen.findByText('v0.4.0')).toBeInTheDocument();

    // 再点 logo 收起：品牌文字与版本号再次隐藏
    await user.click(screen.getByLabelText('切换导航展开收起'));
    expect(screen.queryByText('JianArtifact')).not.toBeInTheDocument();
    expect(screen.queryByText('v0.4.0')).not.toBeInTheDocument();
  });

  it('键盘可达：logo 区聚焦后按 Enter 切换展开', async () => {
    const user = userEvent.setup();
    renderAt('/');

    const toggle = screen.getByLabelText('切换导航展开收起');
    toggle.focus();
    await user.keyboard('{Enter}');

    expect(screen.getByText('JianArtifact')).toBeInTheDocument();
  });
});

describe('AppLayout 分段导航（浏览 / 管理 / 系统·监控）', () => {
  it('展开态显示三个段头，且关键入口归属正确', async () => {
    const user = userEvent.setup();
    renderAt('/', 管理员上下文());

    await user.click(screen.getByLabelText('切换导航展开收起'));

    // 三个分段段头（小灰字）均渲染
    expect(screen.getByText('浏览')).toBeInTheDocument();
    expect(screen.getByText('管理')).toBeInTheDocument();
    expect(screen.getByText('系统 · 监控')).toBeInTheDocument();

    // 段内关键入口可达（展开态文字可见）
    expect(screen.getByText('仪表盘')).toBeInTheDocument();
    expect(screen.getByText('用户与组')).toBeInTheDocument();
    expect(screen.getByText('系统日志')).toBeInTheDocument();
  });

  it('收起态：段头文字不渲染（以分隔线代替），导航项仅图标 + aria-label', () => {
    renderAt('/', 管理员上下文());

    // 收起态：段头文字不可见
    expect(screen.queryByText('浏览')).not.toBeInTheDocument();
    expect(screen.queryByText('管理')).not.toBeInTheDocument();
    // 导航项仍经 aria-label 可达
    expect(navLinkByLabel('仓库')).toBeInTheDocument();
    expect(navLinkByLabel('搜索')).toBeInTheDocument();
  });
});

describe('AppLayout 导航入口增删（删使用分析、加系统日志）', () => {
  it('「使用分析」导航入口已删除（Admin 上下文亦不可见）', () => {
    renderAt('/', 管理员上下文());

    expect(screen.queryByLabelText('使用分析')).not.toBeInTheDocument();
  });

  it('新增「系统日志」入口可见（仅 Admin），点击跳转 /system-logs', async () => {
    const user = userEvent.setup();
    renderAt('/', 管理员上下文());

    const entry = screen.getByLabelText('系统日志');
    expect(entry).toBeInTheDocument();

    await user.click(entry);
    expect(screen.getByTestId('location-probe')).toHaveTextContent('/system-logs');
  });

  it('普通用户看不到「系统日志」入口（沿用 isAdmin 门控）', () => {
    renderAt('/', 普通用户上下文());

    expect(screen.queryByLabelText('系统日志')).not.toBeInTheDocument();
  });
});

describe('AppLayout 角色门控入口（不回归 FR-95）', () => {
  // 管理 / 系统类入口：仅 Admin 可见（按重做后的分段导航清单）。
  const 受控入口 = ['用户与组', 'Nexus 迁移', '监控', '审计日志', '系统日志', '防护配置', '设置'];
  // 通用入口：所有登录用户可见
  const 通用入口 = ['仪表盘', '仓库', '搜索', '访问令牌', '上传'];

  it('普通用户看不到任何管理 / 系统入口，但看得到通用入口', () => {
    renderAt('/', 普通用户上下文());

    for (const label of 受控入口) {
      expect(screen.queryByLabelText(label)).not.toBeInTheDocument();
    }
    for (const label of 通用入口) {
      expect(screen.getByLabelText(label)).toBeInTheDocument();
    }
  });

  it('管理员看得到全部入口', () => {
    renderAt('/', 管理员上下文());

    for (const label of [...受控入口, ...通用入口]) {
      expect(screen.getByLabelText(label)).toBeInTheDocument();
    }
  });
});

describe('AppLayout 侧栏导航高亮（fix-B 段精确匹配，不回归）', () => {
  it('位于 /monitor 时仅「监控」高亮，「防护配置」不串台', () => {
    renderAt('/monitor');

    expect(navLinkByLabel('监控').getAttribute('data-active')).toBe('true');
    expect(navLinkByLabel('防护配置').getAttribute('data-active')).toBeNull();
  });

  it('位于 /protection 时仅「防护配置」高亮，不被「监控」串台（段精确匹配）', () => {
    renderAt('/protection');

    expect(navLinkByLabel('防护配置').getAttribute('data-active')).toBe('true');
    expect(navLinkByLabel('监控').getAttribute('data-active')).toBeNull();
  });

  it('位于子路径 /repositories/libs 时「仓库」仍高亮（按段匹配）', () => {
    renderAt('/repositories/libs');

    expect(navLinkByLabel('仓库').getAttribute('data-active')).toBe('true');
  });

  it('位于根路径 / 时仅「仪表盘」高亮，不波及其他项', () => {
    renderAt('/');

    expect(navLinkByLabel('仪表盘').getAttribute('data-active')).toBe('true');
    expect(navLinkByLabel('仓库').getAttribute('data-active')).toBeNull();
  });
});

describe('AppLayout 页眉全局搜索（FR-94，不回归）', () => {
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

  it('空关键字回车不跳转（停留原路由）', async () => {
    const user = userEvent.setup();
    renderAt('/');

    const input = screen.getByLabelText('全局搜索');
    await user.type(input, '   {enter}');

    expect(screen.getByTestId('location-probe')).toHaveTextContent('/');
    expect(screen.getByTestId('location-probe')).not.toHaveTextContent('/search');
  });
});

describe('AppLayout 匿名访客外壳（FR-95，不回归）', () => {
  // 公开导航项：匿名可见
  const 公开导航 = ['仓库', '搜索'];
  // 受限入口：匿名一律不可见（含通用登录项与全部管理 / 系统项）
  const 受限入口 = [
    '仪表盘',
    '访问令牌',
    '上传',
    '用户与组',
    'Nexus 迁移',
    '监控',
    '审计日志',
    '系统日志',
    '防护配置',
    '设置',
  ];

  it('匿名态页眉显示「登录」按钮，不显示用户名与登出', () => {
    renderAt('/repositories', 匿名上下文());

    expect(screen.getByRole('button', { name: '登录' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '登出' })).not.toBeInTheDocument();
  });

  it('匿名态导航只留公开项，不显示任何受限 / 管理 / 系统入口', () => {
    renderAt('/repositories', 匿名上下文());

    for (const label of 公开导航) {
      expect(screen.getByLabelText(label)).toBeInTheDocument();
    }
    for (const label of 受限入口) {
      expect(screen.queryByLabelText(label)).not.toBeInTheDocument();
    }
  });

  it('点击页眉「登录」跳转到 /login', async () => {
    const user = userEvent.setup();
    renderAt('/repositories', 匿名上下文());

    await user.click(screen.getByRole('button', { name: '登录' }));

    expect(screen.getByTestId('location-probe')).toHaveTextContent('/login');
  });
});

describe('AppLayout 更新徽标（FR-101，不回归）', () => {
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
});

describe('AppLayout 左下 footer：开源许可 + 折叠按钮（FR-92 重做）', () => {
  it('展开态：footer 显示「开源许可」入口与「收起导航」按钮', async () => {
    const user = userEvent.setup();
    renderAt('/', 匿名上下文());

    await user.click(screen.getByLabelText('切换导航展开收起'));

    expect(screen.getByLabelText('开源许可')).toBeInTheDocument();
    expect(screen.getByText('开源许可')).toBeInTheDocument();
    // footer 折叠按钮（与 logo 切换并存，区分语义）
    expect(screen.getByLabelText('收起导航')).toBeInTheDocument();
  });

  it('收起（窄）态：footer 隐藏许可，只留「展开导航」按钮在底', async () => {
    renderAt('/', 匿名上下文());

    // 等健康检查 resolve，确认收起态下许可不渲染
    await waitFor(() => expect(api.getHealth).toHaveBeenCalled());
    expect(screen.queryByLabelText('开源许可')).not.toBeInTheDocument();
    // 收起态 footer 仍保留展开按钮
    expect(screen.getByLabelText('展开导航')).toBeInTheDocument();
    // 外壳照常渲染（导航可用）
    expect(screen.getByLabelText('仓库')).toBeInTheDocument();
  });

  it('展开态点击「开源许可」跳转 /licenses', async () => {
    const user = userEvent.setup();
    renderAt('/', 匿名上下文());

    await user.click(screen.getByLabelText('切换导航展开收起'));
    await user.click(screen.getByLabelText('开源许可'));
    expect(screen.getByTestId('location-probe')).toHaveTextContent('/licenses');
  });

  it('点击 footer「展开导航」按钮亦可展开（与 logo 切换等效）', async () => {
    const user = userEvent.setup();
    renderAt('/', 匿名上下文());

    await user.click(screen.getByLabelText('展开导航'));
    // 展开后 footer 切到「收起导航」、品牌文字可见
    expect(screen.getByLabelText('收起导航')).toBeInTheDocument();
    expect(screen.getByText('JianArtifact')).toBeInTheDocument();
  });
});

describe('AppLayout 内容区固定 max-width（FR-92 防撑变形）', () => {
  it('内容区包一层带最大宽度的居中容器（data-testid=content-shell）', () => {
    renderAt('/', 管理员上下文());

    const shell = screen.getByTestId('content-shell');
    expect(shell).toBeInTheDocument();
    // 居中容器设置了 maxWidth 内联样式（具体取值取 density 常量，不依赖像素断言等值）
    expect(shell.style.maxWidth).not.toBe('');
  });
});
