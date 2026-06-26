// 控制台布局外壳：顶部栏 + 折叠图标导航条 + 内容区（FR-92）。
// 导航默认窄（仅图标 + tooltip / aria-label），可点击展开为图标+文字；据角色显隐管理入口。

import {
  AppShell,
  Burger,
  Group,
  NavLink,
  ScrollArea,
  Text,
  Button,
  Tooltip,
  TextInput,
} from '@mantine/core';
import { useDisclosure, useDebouncedCallback } from '@mantine/hooks';
import { useState, type KeyboardEvent } from 'react';
import {
  IconDashboard,
  IconDatabase,
  IconSearch,
  IconKey,
  IconUsers,
  IconUsersGroup,
  IconShieldLock,
  IconUpload,
  IconActivity,
  IconTransfer,
  IconSettings,
  IconLogout,
  IconLogin,
  IconLayoutSidebarLeftExpand,
  IconLayoutSidebarLeftCollapse,
} from '@tabler/icons-react';
import { Outlet, useLocation, useNavigate } from 'react-router-dom';
import { useAuth } from '../auth/useAuth';
import { density } from '../theme/density';

/** 导航项定义。 */
interface NavItem {
  label: string;
  path: string;
  icon: React.ReactNode;
  /** 仅管理员可见。 */
  adminOnly?: boolean;
  /** 匿名访客可见（公开只读浏览入口，FR-95）。未标记的项匿名一律不可见。 */
  publicVisible?: boolean;
}

/**
 * 判定导航项是否对应当前路由：按路径段精确匹配，避免前缀串台。
 * 仅当当前路径等于该项路径、或为其子路径（以「该项路径 + /」开头）时高亮，
 * 故 /protection 不会在 /protection-monitor 下被误判为 active。
 */
function isNavActive(pathname: string, itemPath: string): boolean {
  if (itemPath === '/') {
    return pathname === '/';
  }
  return pathname === itemPath || pathname.startsWith(`${itemPath}/`);
}

const NAV_ITEMS: NavItem[] = [
  { label: '仪表盘', path: '/', icon: <IconDashboard size={18} /> },
  // 公开浏览入口：匿名访客亦可见（FR-95），用于只读浏览 / 搜索公开制品
  {
    label: '仓库管理',
    path: '/repositories',
    icon: <IconDatabase size={18} />,
    publicVisible: true,
  },
  { label: '制品搜索', path: '/search', icon: <IconSearch size={18} />, publicVisible: true },
  { label: 'Token 管理', path: '/tokens', icon: <IconKey size={18} /> },
  { label: '用户管理', path: '/users', icon: <IconUsers size={18} />, adminOnly: true },
  { label: '用户组管理', path: '/groups', icon: <IconUsersGroup size={18} />, adminOnly: true },
  { label: '防护配置', path: '/protection', icon: <IconShieldLock size={18} />, adminOnly: true },
  { label: '制品上传', path: '/upload', icon: <IconUpload size={18} /> },
  // FR-99：使用分析 / 审计日志 / 防护监控三个独立入口收敛为统一「监控」入口（tab 化整合）
  { label: '监控', path: '/monitor', icon: <IconActivity size={18} />, adminOnly: true },
  { label: 'Nexus 迁移', path: '/migration', icon: <IconTransfer size={18} />, adminOnly: true },
  { label: '设置', path: '/settings', icon: <IconSettings size={18} />, adminOnly: true },
];

/**
 * 单个导航项：展开态显示图标+文字；折叠（窄）态仅图标，
 * 经 Tooltip + aria-label 提供可访问名，保证窄态读屏 / 键盘可用。
 */
function NavItemLink({
  item,
  expanded,
  active,
  onSelect,
}: {
  item: NavItem;
  expanded: boolean;
  active: boolean;
  onSelect: () => void;
}) {
  if (expanded) {
    return (
      <NavLink
        label={item.label}
        aria-label={item.label}
        leftSection={item.icon}
        active={active}
        onClick={onSelect}
      />
    );
  }
  return (
    <Tooltip label={item.label} position="right" withArrow>
      <NavLink aria-label={item.label} leftSection={item.icon} active={active} onClick={onSelect} />
    </Tooltip>
  );
}

/** 应用布局：渲染折叠图标导航与子路由出口。 */
export function AppLayout() {
  // mobileOpened：移动端抽屉开合；navExpanded：桌面侧栏窄/宽（默认窄）。
  const [mobileOpened, { toggle: toggleMobile }] = useDisclosure();
  const [navExpanded, { toggle: toggleNav }] = useDisclosure(false);
  const { user, isAdmin, signOut } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  // 页眉全局搜索（FR-94）：输入关键字 → 跳转 /search?q=；回车立即跳，停止输入防抖后自动跳。
  const [searchValue, setSearchValue] = useState('');

  const handleSignOut = async () => {
    await signOut();
    navigate('/login', { replace: true });
  };

  // 跳转到搜索结果页：空关键字不跳；用 URLSearchParams 统一编码，重复跳同 URL 幂等。
  const gotoSearch = (raw: string) => {
    const keyword = raw.trim();
    if (!keyword) return;
    navigate(`/search?${new URLSearchParams({ q: keyword }).toString()}`);
  };

  const debouncedGotoSearch = useDebouncedCallback(gotoSearch, 300);

  const handleSearchChange = (value: string) => {
    setSearchValue(value);
    debouncedGotoSearch(value);
  };

  const handleSearchKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      gotoSearch(searchValue);
    }
  };

  // 角色感知导航过滤（FR-95）：匿名只见公开浏览入口；登录用户按 adminOnly 门控（FR-92 不回归）。
  const visibleItems = user
    ? NAV_ITEMS.filter((item) => !item.adminOnly || isAdmin)
    : NAV_ITEMS.filter((item) => item.publicVisible);

  const navbarWidth = navExpanded ? density.navbarWidth.expanded : density.navbarWidth.collapsed;

  return (
    <AppShell
      header={{ height: 56 }}
      navbar={{ width: navbarWidth, breakpoint: 'sm', collapsed: { mobile: !mobileOpened } }}
      padding={density.mainPadding}
    >
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between">
          <Group>
            <Burger opened={mobileOpened} onClick={toggleMobile} hiddenFrom="sm" size="sm" />
            <Text fw={700} size="lg">
              JianArtifact 控制台
            </Text>
          </Group>
          {/* 全局搜索框（FR-94）：回车立即跳、停止输入防抖后自动跳到 /search?q= */}
          <TextInput
            visibleFrom="sm"
            size="xs"
            w={240}
            leftSection={<IconSearch size={14} />}
            placeholder="搜索制品（回车或停顿即搜）"
            aria-label="全局搜索"
            value={searchValue}
            onChange={(e) => handleSearchChange(e.currentTarget.value)}
            onKeyDown={handleSearchKeyDown}
          />
          {/* 角色感知页眉（FR-95）：登录态显示用户名 + 登出；匿名态显示「登录」按钮 */}
          {user ? (
            <Group>
              <Text size="sm" c="dimmed">
                {user.username}（{user.role === 'admin' ? '管理员' : '用户'}）
              </Text>
              <Button
                variant="subtle"
                size="xs"
                leftSection={<IconLogout size={16} />}
                onClick={handleSignOut}
              >
                登出
              </Button>
            </Group>
          ) : (
            <Button
              variant="light"
              size="xs"
              leftSection={<IconLogin size={16} />}
              onClick={() => navigate('/login')}
            >
              登录
            </Button>
          )}
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p="xs">
        <Group justify={navExpanded ? 'flex-end' : 'center'} mb="xs">
          <Tooltip label={navExpanded ? '收起导航' : '展开导航'} position="right" withArrow>
            <Button
              variant="subtle"
              size="xs"
              px="xs"
              aria-label={navExpanded ? '收起导航' : '展开导航'}
              onClick={toggleNav}
            >
              {navExpanded ? (
                <IconLayoutSidebarLeftCollapse size={18} />
              ) : (
                <IconLayoutSidebarLeftExpand size={18} />
              )}
            </Button>
          </Tooltip>
        </Group>
        <ScrollArea>
          {visibleItems.map((item) => (
            <NavItemLink
              key={item.path}
              item={item}
              expanded={navExpanded}
              active={isNavActive(location.pathname, item.path)}
              onSelect={() => {
                navigate(item.path);
                if (mobileOpened) toggleMobile();
              }}
            />
          ))}
        </ScrollArea>
      </AppShell.Navbar>

      <AppShell.Main>
        <Outlet />
      </AppShell.Main>
    </AppShell>
  );
}
