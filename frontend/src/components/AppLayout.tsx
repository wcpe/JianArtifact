// 控制台布局外壳：顶部栏 + 侧边导航 + 内容区。导航据角色显隐用户管理。

import { AppShell, Burger, Group, NavLink, ScrollArea, Text, Button } from '@mantine/core';
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
  IconLogout,
} from '@tabler/icons-react';
import { Outlet, useLocation, useNavigate } from 'react-router-dom';
import { useAuth } from '../auth/useAuth';

/** 导航项定义。 */
interface NavItem {
  label: string;
  path: string;
  icon: React.ReactNode;
  adminOnly?: boolean;
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
];

/** 应用布局：渲染导航与子路由出口。 */
export function AppLayout() {
  const [opened, { toggle }] = useDisclosure();
  const { user, isAdmin, signOut } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();

  const handleSignOut = async () => {
    await signOut();
    navigate('/login', { replace: true });
  };

  const visibleItems = NAV_ITEMS.filter((item) => !item.adminOnly || isAdmin);

  return (
    <AppShell
      header={{ height: 56 }}
      navbar={{ width: 240, breakpoint: 'sm', collapsed: { mobile: !opened } }}
      padding="md"
    >
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between">
          <Group>
            <Burger opened={opened} onClick={toggle} hiddenFrom="sm" size="sm" />
            <Text fw={700} size="lg">
              JianArtifact 控制台
            </Text>
          </Group>
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

      <AppShell.Navbar p="md">
        <ScrollArea>
          {visibleItems.map((item) => (
            <NavLink
              key={item.path}
              label={item.label}
              leftSection={item.icon}
              active={
                item.path === '/'
                  ? location.pathname === '/'
                  : location.pathname.startsWith(item.path)
              }
              onClick={() => {
                navigate(item.path);
                if (opened) toggle();
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
