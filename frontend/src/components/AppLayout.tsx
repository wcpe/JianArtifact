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
import { useDisclosure } from '@mantine/hooks';
import {
  IconDashboard,
  IconDatabase,
  IconSearch,
  IconKey,
  IconUsers,
  IconUsersGroup,
  IconChartBar,
  IconShieldLock,
  IconUpload,
  IconHistory,
  IconShield,
  IconTransfer,
  IconSettings,
  IconLogout,
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
  adminOnly?: boolean;
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
  { label: '仓库管理', path: '/repositories', icon: <IconDatabase size={18} /> },
  { label: '制品搜索', path: '/search', icon: <IconSearch size={18} /> },
  { label: 'Token 管理', path: '/tokens', icon: <IconKey size={18} /> },
  { label: '用户管理', path: '/users', icon: <IconUsers size={18} />, adminOnly: true },
  { label: '用户组管理', path: '/groups', icon: <IconUsersGroup size={18} />, adminOnly: true },
  { label: '使用分析', path: '/analytics', icon: <IconChartBar size={18} />, adminOnly: true },
  { label: '防护配置', path: '/protection', icon: <IconShieldLock size={18} />, adminOnly: true },
  { label: '制品上传', path: '/upload', icon: <IconUpload size={18} /> },
  { label: '审计日志', path: '/audit', icon: <IconHistory size={18} />, adminOnly: true },
  {
    label: '防护监控',
    path: '/protection-monitor',
    icon: <IconShield size={18} />,
    adminOnly: true,
  },
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

  const handleSignOut = async () => {
    await signOut();
    navigate('/login', { replace: true });
  };

  const visibleItems = NAV_ITEMS.filter((item) => !item.adminOnly || isAdmin);

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
          {/* 全局搜索框占位：搜索逻辑由 FR-94 实现，本 FR 仅留位置、禁用不接逻辑 */}
          <TextInput
            visibleFrom="sm"
            disabled
            size="xs"
            w={240}
            leftSection={<IconSearch size={14} />}
            placeholder="全局搜索（即将上线）"
            aria-label="全局搜索（即将上线）"
          />
          <Group>
            <Text size="sm" c="dimmed">
              {user?.username}（{user?.role === 'admin' ? '管理员' : '用户'}）
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
