// 控制台外壳（FR-92 重做）：左上 logo 区（SVG + 品牌 + 版本号，点 logo 切换导航展开/收起）
// + 分段导航（浏览 / 管理 / 系统·监控）+ 左下 footer（开源许可 + 折叠按钮）+ 固定 max-width 内容区。
// 收起态仅图标（Tooltip + aria-label 可达）、段间以分隔线代替段头；据角色显隐管理 / 系统入口。

import {
  AppShell,
  Badge,
  Box,
  Burger,
  Divider,
  Group,
  NavLink,
  ScrollArea,
  Stack,
  Text,
  Button,
  Tooltip,
  TextInput,
  UnstyledButton,
} from '@mantine/core';
import { useDisclosure, useDebouncedCallback } from '@mantine/hooks';
import { useEffect, useRef, useState, type KeyboardEvent } from 'react';
import {
  IconLayoutDashboard,
  IconPackage,
  IconSearch,
  IconKey,
  IconUsers,
  IconUpload,
  IconArrowsExchange,
  IconChartDots,
  IconClipboardText,
  IconFileText,
  IconSettings,
  IconServerCog,
  IconLogout,
  IconLogin,
  IconLayoutSidebarLeftExpand,
  IconLayoutSidebarLeftCollapse,
  IconLicense,
  IconArrowUpCircle,
} from '@tabler/icons-react';
import { Outlet, useLocation, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useAuth } from '../auth/useAuth';
import { density } from '../theme/density';
import { useDocumentTitle } from '../hooks/useDocumentTitle';
import { checkUpdate, getHealth } from '../api/endpoints';

/** 品牌蓝（logo 主色，FR-113）：集中一处常量，避免散落魔法值。 */
const BRAND_BLUE = '#228be6';
/** 品牌浅蓝（立方体 / 包裹线稿描边，FR-113）。 */
const BRAND_BLUE_LIGHT = '#a5d8ff';

/**
 * 品牌 logo 矢量图（FR-92；FR-113 改蓝）：蓝底圆角方块 + 浅蓝立方体 / 包裹线稿，寓意「制品 / 打包」。
 * viewBox 24×24，纯内联 SVG（无外部资源、无新增依赖）；尺寸由调用方控制。
 */
function BrandLogo({ size = 28 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      xmlns="http://www.w3.org/2000/svg"
      aria-hidden="true"
      focusable="false"
    >
      {/* 蓝底圆角方块 */}
      <rect x="1.5" y="1.5" width="21" height="21" rx="5" fill={BRAND_BLUE} />
      {/* 浅蓝立方体线稿：顶面菱形 + 三条竖棱（制品 / 包裹寓意） */}
      <path
        d="M12 5.5 L17.5 8.5 L12 11.5 L6.5 8.5 Z"
        stroke={BRAND_BLUE_LIGHT}
        strokeWidth="1.4"
        strokeLinejoin="round"
        fill="none"
      />
      <path
        d="M6.5 8.5 L6.5 15 L12 18 L17.5 15 L17.5 8.5"
        stroke={BRAND_BLUE_LIGHT}
        strokeWidth="1.4"
        strokeLinejoin="round"
        fill="none"
      />
      <path d="M12 11.5 L12 18" stroke={BRAND_BLUE_LIGHT} strokeWidth="1.4" />
    </svg>
  );
}

/** 导航项定义。 */
interface NavItem {
  /** nav 命名空间内的文案 key（渲染时经 t() 解析为本地化标签，FR-111）。 */
  labelKey: string;
  path: string;
  icon: React.ReactNode;
  /** 仅管理员可见。 */
  adminOnly?: boolean;
  /** 匿名访客可见（公开只读浏览入口，FR-95）。未标记的项匿名一律不可见。 */
  publicVisible?: boolean;
}

/** 导航分段：段标题 + 段内项；展开态显段头小灰字，收起态以分隔线代替。 */
interface NavSection {
  /** nav 命名空间内的段标题 key（渲染时经 t() 解析，FR-111）。 */
  titleKey: string;
  items: NavItem[];
}

/**
 * 判定导航项是否对应当前路由：按路径段精确匹配，避免前缀串台。
 * 仅当当前路径等于该项路径、或为其子路径（以「该项路径 + /」开头）时高亮，
 * 故 /system 不会在 /system-logs 下被误判为 active。
 */
function isNavActive(pathname: string, itemPath: string): boolean {
  if (itemPath === '/') {
    return pathname === '/';
  }
  return pathname === itemPath || pathname.startsWith(`${itemPath}/`);
}

// 分段导航（FR-92 已确认设计）：
// - 浏览：仪表盘 / 仓库 / 搜索（仓库、搜索匿名可见，FR-95）
// - 管理：用户与组 / 访问令牌 / 上传 / Nexus 迁移
// - 系统·监控：监控 / 审计日志 / 系统日志（FR-107 路由）/ 设置 / 系统
// 「使用分析」入口已并入监控（FR-99）删除；防护配置入口已并入「设置」页（FR-110）移除。
const NAV_SECTIONS: NavSection[] = [
  {
    titleKey: 'sectionBrowse',
    items: [
      { labelKey: 'dashboard', path: '/', icon: <IconLayoutDashboard size={18} /> },
      {
        labelKey: 'repositories',
        path: '/repositories',
        icon: <IconPackage size={18} />,
        publicVisible: true,
      },
      { labelKey: 'search', path: '/search', icon: <IconSearch size={18} />, publicVisible: true },
    ],
  },
  {
    titleKey: 'sectionManage',
    items: [
      { labelKey: 'usersGroups', path: '/users', icon: <IconUsers size={18} />, adminOnly: true },
      { labelKey: 'tokens', path: '/tokens', icon: <IconKey size={18} /> },
      { labelKey: 'upload', path: '/upload', icon: <IconUpload size={18} /> },
      {
        labelKey: 'migration',
        path: '/migration',
        icon: <IconArrowsExchange size={18} />,
        adminOnly: true,
      },
    ],
  },
  {
    titleKey: 'sectionSystem',
    items: [
      { labelKey: 'monitor', path: '/monitor', icon: <IconChartDots size={18} />, adminOnly: true },
      {
        labelKey: 'audit',
        path: '/audit',
        icon: <IconClipboardText size={18} />,
        adminOnly: true,
      },
      // 系统日志页与 /system-logs 路由由并行 FR-107 创建；本 FR 仅加导航入口
      {
        labelKey: 'systemLogs',
        path: '/system-logs',
        icon: <IconFileText size={18} />,
        adminOnly: true,
      },
      {
        labelKey: 'settings',
        path: '/settings',
        icon: <IconSettings size={18} />,
        adminOnly: true,
      },
      // 系统管理页（FR-109，仅 Admin）：在线更新 + 重启 / 关闭
      { labelKey: 'system', path: '/system', icon: <IconServerCog size={18} />, adminOnly: true },
    ],
  },
];

/**
 * 单个导航项：展开态显示图标+文字；收起（窄）态仅图标，
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
  const { t } = useTranslation('nav');
  const label = t(item.labelKey);
  if (expanded) {
    return (
      <NavLink
        label={label}
        aria-label={label}
        leftSection={item.icon}
        active={active}
        onClick={onSelect}
      />
    );
  }
  return (
    <Tooltip label={label} position="right" withArrow>
      <NavLink aria-label={label} leftSection={item.icon} active={active} onClick={onSelect} />
    </Tooltip>
  );
}

/** 应用布局：渲染 logo 区 + 分段导航 + 左下 footer + 固定 max-width 内容区。 */
export function AppLayout() {
  const { t } = useTranslation('nav');
  // mobileOpened：移动端抽屉开合；navExpanded：桌面侧栏窄/宽（默认窄）。
  const [mobileOpened, { toggle: toggleMobile }] = useDisclosure();
  const [navExpanded, { toggle: toggleNav }] = useDisclosure(false);
  const { user, isAdmin, signOut } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  // 按路由动态设置浏览器标签页标题（FR-113）：「<当前页名> - JianArtifact」
  useDocumentTitle(location.pathname);
  // 页眉全局搜索（FR-94）：输入关键字 → 跳转 /search?q=；回车立即跳，停止输入防抖后自动跳。
  const [searchValue, setSearchValue] = useState('');
  // 全局搜索框引用（FR-114）：供 Ctrl/Cmd+K 快捷键聚焦、Esc 失焦。
  const searchRef = useRef<HTMLInputElement>(null);
  // 控制台版本展示（FR-101）：logo 区下方小灰字常显当前版本号（取自公开 /health，所有用户可见）。
  const [version, setVersion] = useState<string | null>(null);
  // Logo 旁更新徽标（FR-101，仅 Admin、确有可更新时显）：缓存 {当前版本, 最新版本}。
  const [updateInfo, setUpdateInfo] = useState<{ current: string; latest: string } | null>(null);

  // 挂载时查一次健康状态取版本号；失败静默（版本号区不渲染），不阻塞外壳渲染。
  useEffect(() => {
    let cancelled = false;
    getHealth()
      .then((info) => {
        if (!cancelled) setVersion(info.version);
      })
      .catch(() => {
        // 健康检查失败：静默降级，不显版本号、不报错
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // 仅 Admin 挂载时查一次更新：仅「有可用更新」才置徽标；其余（未启用 409 / 无更新 / 失败）静默不显。
  // 只在挂载查一次并缓存，不每次渲染重查（避免 GitHub 限流）；查询走后台、不阻塞渲染。
  useEffect(() => {
    if (!isAdmin) return;
    let cancelled = false;
    checkUpdate()
      .then((result) => {
        if (!cancelled && result.update_available) {
          setUpdateInfo({ current: result.current_version, latest: result.latest_version });
        }
      })
      .catch(() => {
        // 未启用在线更新（409）/ 请求失败：静默吞掉，不显徽标
      });
    return () => {
      cancelled = true;
    };
  }, [isAdmin]);

  // 全局搜索快捷键（FR-114）：Ctrl+K（Mac Cmd+K）聚焦页眉搜索框、Esc 失焦。
  // 挂载时注册、卸载时移除；命中本快捷键即 preventDefault，避免触发浏览器默认行为；
  // 单独处理 mod+K，故不会误触其它输入控件内的按键（仅在 K + 修饰键命中时介入）。
  useEffect(() => {
    const handleGlobalKeyDown = (e: globalThis.KeyboardEvent) => {
      const isModK = (e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === 'k';
      if (isModK) {
        e.preventDefault();
        searchRef.current?.focus();
        return;
      }
      // Esc：仅当焦点在搜索框上时失焦收起（避免越权拦截其它控件的 Esc）。
      if (e.key === 'Escape' && document.activeElement === searchRef.current) {
        searchRef.current?.blur();
      }
    };
    window.addEventListener('keydown', handleGlobalKeyDown);
    return () => window.removeEventListener('keydown', handleGlobalKeyDown);
  }, []);

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

  // 点 logo 区切换导航展开/收起（键盘 Enter / Space 等效）。
  const handleBrandKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      toggleNav();
    }
  };

  // 进入许可页（footer 入口）；移动端抽屉态点击后顺手收起抽屉。
  const gotoLicenses = () => {
    navigate('/licenses');
    if (mobileOpened) toggleMobile();
  };

  const handleLicensesKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      gotoLicenses();
    }
  };

  // 角色感知导航过滤（FR-95）：匿名只见公开浏览入口；登录用户按 adminOnly 门控。
  const isItemVisible = (item: NavItem): boolean =>
    user ? !item.adminOnly || isAdmin : Boolean(item.publicVisible);

  // 按段过滤后仅保留含可见项的段（空段不渲染段头 / 分隔线）。
  const visibleSections = NAV_SECTIONS.map((section) => ({
    titleKey: section.titleKey,
    items: section.items.filter(isItemVisible),
  })).filter((section) => section.items.length > 0);

  const navbarWidth = navExpanded ? density.navbarWidth.expanded : density.navbarWidth.collapsed;

  return (
    <AppShell
      // alt 布局：侧边栏（navbar）占满整列高度、从最顶端起（logo 置于侧栏最上），
      // 页眉（header）只压在右侧内容区上方、不再横跨顶部占据侧边栏顶部（参 JianVideo 版式）
      layout="alt"
      header={{ height: density.headerHeight }}
      navbar={{ width: navbarWidth, breakpoint: 'sm', collapsed: { mobile: !mobileOpened } }}
      padding={density.mainPadding}
    >
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between">
          <Group gap="sm" wrap="nowrap">
            <Burger opened={mobileOpened} onClick={toggleMobile} hiddenFrom="sm" size="sm" />
            {/* 更新徽标（FR-101，仅 Admin 且确有可更新时显）：常显于页眉、点击跳设置页在线更新区。
                置于页眉而非收起态窄导航内，保证任何导航开合态下都可见、不被 64px 窄栏裁剪。 */}
            {updateInfo && (
              <Badge
                color="orange"
                variant="light"
                size="sm"
                style={{ cursor: 'pointer' }}
                leftSection={<IconArrowUpCircle size={12} />}
                role="button"
                tabIndex={0}
                aria-label={t('updateBadgeAriaLabel')}
                onClick={() => navigate('/settings')}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault();
                    navigate('/settings');
                  }
                }}
              >
                {t('updateBadge', { current: updateInfo.current, latest: updateInfo.latest })}
              </Badge>
            )}
          </Group>
          {/* 全局搜索框（FR-94）：回车立即跳、停止输入防抖后自动跳到 /search?q=。
              FR-114：ref 供 Ctrl/Cmd+K 聚焦；右侧「Ctrl K」提示徽标。 */}
          <TextInput
            ref={searchRef}
            visibleFrom="sm"
            size="xs"
            w={240}
            leftSection={<IconSearch size={14} />}
            rightSection={
              <Badge color="gray" variant="light" size="xs" aria-hidden="true">
                Ctrl K
              </Badge>
            }
            rightSectionWidth={56}
            placeholder={t('searchPlaceholder')}
            aria-label={t('searchAriaLabel')}
            value={searchValue}
            onChange={(e) => handleSearchChange(e.currentTarget.value)}
            onKeyDown={handleSearchKeyDown}
          />
          {/* 角色感知页眉（FR-95）：登录态显示用户名 + 登出；匿名态显示「登录」按钮 */}
          {user ? (
            <Group>
              <Text size="sm" c="dimmed">
                {user.username}
                {t('userSuffix', {
                  role: user.role === 'admin' ? t('common:roleAdmin') : t('common:roleUser'),
                })}
              </Text>
              <Button
                variant="subtle"
                size="xs"
                leftSection={<IconLogout size={16} />}
                onClick={handleSignOut}
              >
                {t('signOut')}
              </Button>
            </Group>
          ) : (
            <Button
              variant="light"
              size="xs"
              leftSection={<IconLogin size={16} />}
              onClick={() => navigate('/login')}
            >
              {t('signIn')}
            </Button>
          )}
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p="xs">
        {/* 左上 logo 区（FR-92）：点击「logo + 文字」整体切换导航展开/收起；
            展开态显品牌文字 + 小灰字版本号，收起态只留可点击 SVG。 */}
        <Group
          gap="xs"
          wrap="nowrap"
          mb="xs"
          justify={navExpanded ? 'flex-start' : 'center'}
          role="button"
          tabIndex={0}
          aria-label={t('toggleNav')}
          style={{ cursor: 'pointer' }}
          onClick={toggleNav}
          onKeyDown={handleBrandKeyDown}
        >
          <BrandLogo size={28} />
          {navExpanded && (
            <Stack gap={0}>
              <Text fw={700} size="sm" lh={1.2}>
                JianArtifact
              </Text>
              {version && (
                <Text size="xs" c="dimmed" lh={1.2}>
                  v{version}
                </Text>
              )}
            </Stack>
          )}
        </Group>

        <ScrollArea style={{ flex: 1 }}>
          {visibleSections.map((section, index) => (
            <Box key={section.titleKey} mt={index === 0 ? 0 : 'xs'}>
              {/* 展开态：段头小灰字；收起态：以细分隔线代替段头（首段不加分隔线） */}
              {navExpanded ? (
                <Text size="xs" c="dimmed" fw={600} px="xs" py={4}>
                  {t(section.titleKey)}
                </Text>
              ) : (
                index > 0 && <Divider my={6} />
              )}
              {section.items.map((item) => (
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
            </Box>
          ))}
        </ScrollArea>

        {/* 左下 footer（FR-92）：展开态显「开源许可」+「收起导航」按钮；
            收起（窄）态隐藏许可、只留「展开导航」按钮在底。 */}
        <Box mt="xs" pt="xs" style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}>
          {navExpanded ? (
            <Group justify="space-between" wrap="nowrap">
              <Group
                gap={4}
                c="dimmed"
                wrap="nowrap"
                role="button"
                tabIndex={0}
                aria-label={t('openSourceLicenses')}
                style={{ cursor: 'pointer' }}
                onClick={gotoLicenses}
                onKeyDown={handleLicensesKeyDown}
              >
                <IconLicense size={14} />
                <Text size="xs">{t('openSourceLicenses')}</Text>
              </Group>
              <Tooltip label={t('collapseNav')} position="right" withArrow>
                <UnstyledButton
                  aria-label={t('collapseNav')}
                  onClick={toggleNav}
                  style={{ display: 'flex' }}
                >
                  <IconLayoutSidebarLeftCollapse size={18} />
                </UnstyledButton>
              </Tooltip>
            </Group>
          ) : (
            <Group justify="center">
              <Tooltip label={t('expandNav')} position="right" withArrow>
                <UnstyledButton
                  aria-label={t('expandNav')}
                  onClick={toggleNav}
                  style={{ display: 'flex' }}
                >
                  <IconLayoutSidebarLeftExpand size={18} />
                </UnstyledButton>
              </Tooltip>
            </Group>
          )}
        </Box>
      </AppShell.Navbar>

      <AppShell.Main>
        {/* 固定 max-width 居中内容容器（FR-92）：卡片 / 新内容出现不再撑变形整体布局。 */}
        <Box
          data-testid="content-shell"
          style={{ maxWidth: density.contentMaxWidth, marginInline: 'auto' }}
        >
          <Outlet />
        </Box>
      </AppShell.Main>
    </AppShell>
  );
}
