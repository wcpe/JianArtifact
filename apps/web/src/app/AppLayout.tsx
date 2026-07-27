// 管理端控制台外壳：左上品牌 logo 区（SVG + 品牌 + 版本号，点 logo 切换导航展开/收起）
// + 分段导航（浏览 / 管理）+ 左下 footer（折叠按钮）+ 固定 max-width 内容区。
// 收起态仅图标（Tooltip + aria-label 可达）、段间以分隔线代替段头；据角色显隐管理入口。
// 视觉沿用旧项目控制台外壳（AppShell layout="alt"）。
import {
  ActionIcon,
  AppShell,
  Box,
  Burger,
  Button,
  Divider,
  Group,
  NavLink,
  ScrollArea,
  Stack,
  Text,
  TextInput,
  Tooltip,
  UnstyledButton,
} from "@mantine/core";
import { useDisclosure, useLocalStorage, useMediaQuery } from "@mantine/hooks";
import {
  IconKey,
  IconLayoutDashboard,
  IconLayoutSidebarLeftCollapse,
  IconLayoutSidebarLeftExpand,
  IconLicense,
  IconLogin,
  IconLogout,
  IconPackage,
  IconRefresh,
  IconSearch,
  IconTransfer,
  IconUsers,
} from "@tabler/icons-react";
import { useEffect, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Outlet, useLocation, useNavigate } from "react-router-dom";

import { getStatus, listPublicRepositories } from "../api/endpoints";
import { useAuth } from "../auth/AuthContext";
import { useLoginModal } from "../auth/LoginModal";
import { BrandLogo } from "../components/BrandLogo";
import { density } from "../theme/density";
import type { Repository } from "../api/types";

/** 导航项定义。 */
interface NavItem {
  labelKey: string;
  path: string;
  icon: ReactNode;
  /** 仅管理员可见。 */
  adminOnly?: boolean;
}

/** 导航分段：段标题 + 段内项。 */
interface NavSection {
  titleKey: string;
  items: NavItem[];
}

/**
 * 判定导航项是否对应当前路由：按路径段精确匹配，避免前缀串台。
 * 仅当当前路径等于该项路径、或为其子路径（以「该项路径 + /」开头）时高亮。
 */
function isNavActive(pathname: string, itemPath: string): boolean {
  return pathname === itemPath || pathname.startsWith(`${itemPath}/`);
}

// 分段导航（仅 0.2.0 已有页面）：
// - 浏览：仪表盘 / 仓库
// - 管理：用户（仅 Admin）/ 访问令牌
const NAV_SECTIONS: NavSection[] = [
  {
    titleKey: "nav.sectionBrowse",
    items: [
      { labelKey: "nav.dashboard", path: "/dashboard", icon: <IconLayoutDashboard size={18} /> },
      { labelKey: "nav.repositories", path: "/repositories", icon: <IconPackage size={18} /> },
    ],
  },
  {
    titleKey: "nav.sectionManage",
    items: [
      { labelKey: "nav.users", path: "/users", icon: <IconUsers size={18} />, adminOnly: true },
      { labelKey: "nav.tokens", path: "/tokens", icon: <IconKey size={18} /> },
      {
        labelKey: "nav.migrations",
        path: "/migrations",
        icon: <IconTransfer size={18} />,
        adminOnly: true,
      },
    ],
  },
];

/**
 * 单个导航项：展开态显示图标+文字；收起（窄）态仅图标，
 * 经 Tooltip + aria-label 提供可访问名，保证窄态读屏 / 键盘可用。
 */
function NavItemLink({
  label,
  icon,
  expanded,
  active,
  onSelect,
}: {
  label: string;
  icon: ReactNode;
  expanded: boolean;
  active: boolean;
  onSelect: () => void;
}) {
  if (expanded) {
    return (
      <NavLink
        label={label}
        aria-label={label}
        leftSection={icon}
        active={active}
        onClick={onSelect}
      />
    );
  }
  return (
    <Tooltip label={label} position="right" withArrow>
      <NavLink aria-label={label} leftSection={icon} active={active} onClick={onSelect} />
    </Tooltip>
  );
}

/** 应用外壳：logo 区 + 分段导航 + 左下 footer + 固定 max-width 内容区。
 * 未登录用户看到精简侧边栏（仅公开仓库列表 + 搜索 + 登录入口）。
 */
export function AppLayout() {
  const { t } = useTranslation();
  const { user, logout } = useAuth();
  const { openLogin } = useLoginModal();
  const navigate = useNavigate();
  const location = useLocation();
  const isAuthenticated = Boolean(user);
  // mobileOpened：移动端抽屉开合。
  const [mobileOpened, { toggle: toggleMobile, close: closeMobile }] = useDisclosure();
  // navExpanded：桌面侧栏窄/宽偏好，持久化到 localStorage。
  const [navExpanded, setNavExpanded] = useLocalStorage<boolean>({
    key: "jianartifact.navExpanded",
    defaultValue: true,
    getInitialValueInEffect: false,
  });
  // isMobile：是否处于移动端。
  const isMobile = useMediaQuery("(max-width: 48em)") ?? false;
  const toggleNav = () => setNavExpanded((value) => !value);
  const [version, setVersion] = useState<string | null>(null);
  // FR-55: 未登录时拉取公开仓库列表供侧边栏展示
  const [publicRepos, setPublicRepos] = useState<Repository[]>([]);
  // FR-59: 搜索栏状态
  const [searchQuery, setSearchQuery] = useState("");

  const isAdmin = user?.role === "admin";
  const expanded = isMobile ? true : navExpanded;

  // 挂载时查一次实例状态取版本号；空库实例导向 /setup 引导自举（整页登录已删除，FR-67）。
  useEffect(() => {
    let cancelled = false;
    getStatus()
      .then((info) => {
        if (cancelled) return;
        setVersion(info.version);
        if (info.userCount === 0) {
          navigate("/setup", { replace: true });
        }
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
    // navigate 引用稳定，仅挂载时执行一次。
  }, []);

  // FR-55: 未登录时加载公开仓库列表
  useEffect(() => {
    if (!isAuthenticated) {
      listPublicRepositories()
        .then((res) => setPublicRepos(res.items))
        .catch(() => setPublicRepos([]));
    }
  }, [isAuthenticated]);

  // FR-67：登出后落地仓库列表（匿名视图），不再有整页登录。
  const handleLogout = () => {
    void logout().then(() => {
      navigate("/repositories", { replace: true });
    });
  };

  // 点 logo 区：桌面切换导航展开/收起；移动端关闭抽屉（键盘 Enter / Space 等效）。
  const handleBrandClick = () => {
    if (isMobile) {
      closeMobile();
    } else {
      toggleNav();
    }
  };
  const handleBrandKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      handleBrandClick();
    }
  };

  // 角色感知导航过滤：非管理员隐藏 adminOnly 项。
  const isItemVisible = (item: NavItem): boolean => !item.adminOnly || isAdmin;

  // 按段过滤后仅保留含可见项的段。
  const visibleSections = NAV_SECTIONS.map((section) => ({
    titleKey: section.titleKey,
    items: section.items.filter(isItemVisible),
  })).filter((section) => section.items.length > 0);

  // 侧栏宽度
  const navbarWidth = {
    base: density.navbarWidth.expanded,
    sm: navExpanded ? density.navbarWidth.expanded : density.navbarWidth.collapsed,
  };
  const roleLabel = isAdmin ? t("common.roleAdmin") : t("common.roleUser");

  // FR-59: Header 搜索提交
  const handleSearch = () => {
    if (searchQuery.trim()) {
      navigate(`/search?q=${encodeURIComponent(searchQuery.trim())}`);
      setSearchQuery("");
    }
  };

  // FR-59: 刷新当前页
  const handleRefresh = () => {
    // 触发页面组件重新加载（通过导航到当前路径不会刷新，用 key trick）
    navigate(location.pathname + location.search, { replace: true });
    // 强制刷新：通过全局事件通知子组件
    window.dispatchEvent(new CustomEvent("jianartifact:refresh"));
  };

  return (
    <AppShell
      layout="alt"
      header={{ height: density.headerHeight }}
      navbar={{ width: navbarWidth, breakpoint: "sm", collapsed: { mobile: !mobileOpened } }}
      padding={density.mainPadding}
    >
      <AppShell.Header>
        <Group h="100%" px="md" wrap="nowrap" justify="space-between">
          <Group gap="sm" wrap="nowrap" style={{ flex: 1, minWidth: 0 }}>
            <Burger opened={mobileOpened} onClick={toggleMobile} hiddenFrom="sm" size="sm" />
            <Group gap="xs" wrap="nowrap" hiddenFrom="sm">
              <BrandLogo size={24} />
              <Text fw={700} size="sm">
                {t("common.appName")}
              </Text>
            </Group>
          </Group>
          {/* FR-59: Header 搜索栏 */}
          <Group gap="sm" wrap="nowrap" style={{ flex: 2, minWidth: 0 }} justify="center">
            <TextInput
              placeholder={t("search.placeholder", { defaultValue: "搜索制品..." })}
              size="xs"
              leftSection={<IconSearch size={14} />}
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.currentTarget.value)}
              onKeyDown={(e) => e.key === "Enter" && handleSearch()}
              style={{ maxWidth: 320, flex: 1 }}
            />
          </Group>
          <Group gap="sm" wrap="nowrap" justify="flex-end" style={{ flex: 1, minWidth: 0 }}>
            {/* FR-59: 刷新按钮 */}
            <Tooltip label={t("common.refresh", { defaultValue: "刷新" })}>
              <ActionIcon variant="subtle" onClick={handleRefresh}>
                <IconRefresh size={18} />
              </ActionIcon>
            </Tooltip>
            {user ? (
              <Group gap="sm" wrap="nowrap">
                <Text size="sm" c="dimmed" truncate>
                  {user.username}
                  {t("nav.userSuffix", { role: roleLabel })}
                </Text>
                <Button
                  variant="subtle"
                  size="xs"
                  leftSection={<IconLogout size={16} />}
                  onClick={handleLogout}
                >
                  {t("common.logout")}
                </Button>
              </Group>
            ) : (
              <Button
                variant="light"
                size="xs"
                leftSection={<IconLogin size={16} />}
                onClick={() => openLogin()}
              >
                {t("auth.login", { defaultValue: "登录" })}
              </Button>
            )}
          </Group>
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p="xs">
        {/* logo 区 */}
        <Group
          gap="xs"
          wrap="nowrap"
          mb="xs"
          justify={expanded ? "flex-start" : "center"}
          role="button"
          tabIndex={0}
          aria-label={t("nav.toggleNav")}
          style={{ cursor: "pointer" }}
          onClick={handleBrandClick}
          onKeyDown={handleBrandKeyDown}
        >
          <BrandLogo size={28} />
          {expanded ? (
            <Stack gap={0}>
              <Text fw={700} size="sm" lh={1.2}>
                {t("common.appName")}
              </Text>
              {version ? (
                <Text size="xs" c="dimmed" lh={1.2}>
                  v{version}
                </Text>
              ) : null}
            </Stack>
          ) : null}
        </Group>

        <ScrollArea style={{ flex: 1 }}>
          {/* FR-55: 未登录 — 精简侧边栏 */}
          {!isAuthenticated ? (
            <Box>
              {expanded && (
                <Text size="xs" c="dimmed" fw={600} px="xs" py={4}>
                  {t("nav.publicRepos", { defaultValue: "公开仓库" })}
                </Text>
              )}
              {publicRepos.map((repo) => (
                <NavItemLink
                  key={repo.name}
                  label={repo.name}
                  icon={<IconPackage size={18} />}
                  expanded={expanded}
                  active={isNavActive(location.pathname, `/repositories/${repo.name}`)}
                  onSelect={() => {
                    navigate(`/repositories/${repo.name}`);
                    if (mobileOpened) closeMobile();
                  }}
                />
              ))}
            </Box>
          ) : (
            /* 已登录 — 完整导航 */
            visibleSections.map((section, index) => (
              <Box key={section.titleKey} mt={index === 0 ? 0 : "xs"}>
                {expanded ? (
                  <Text size="xs" c="dimmed" fw={600} px="xs" py={4}>
                    {t(section.titleKey)}
                  </Text>
                ) : (
                  index > 0 && <Divider my={6} />
                )}
                {section.items.map((item) => (
                  <NavItemLink
                    key={item.path}
                    label={t(item.labelKey)}
                    icon={item.icon}
                    expanded={expanded}
                    active={isNavActive(location.pathname, item.path)}
                    onSelect={() => {
                      navigate(item.path);
                      if (mobileOpened) closeMobile();
                    }}
                  />
                ))}
              </Box>
            ))
          )}
        </ScrollArea>

        {/* 左下 footer */}
        <Box
          mt="xs"
          pt="xs"
          visibleFrom="sm"
          style={{ borderTop: "1px solid var(--mantine-color-default-border)" }}
        >
          {expanded ? (
            <Group justify="space-between" wrap="nowrap">
              {/* FR-72: 开源协议入口（登录/匿名均可见） */}
              <UnstyledButton
                aria-label={t("nav.licenses")}
                onClick={() => {
                  navigate("/licenses");
                  if (mobileOpened) closeMobile();
                }}
                style={{ display: "flex", alignItems: "center", gap: 6 }}
              >
                <IconLicense size={16} />
                <Text size="xs" c="dimmed">
                  {t("nav.licenses")}
                </Text>
              </UnstyledButton>
              <Tooltip label={t("nav.collapseNav")} position="right" withArrow>
                <UnstyledButton
                  aria-label={t("nav.collapseNav")}
                  onClick={toggleNav}
                  style={{ display: "flex" }}
                >
                  <IconLayoutSidebarLeftCollapse size={18} />
                </UnstyledButton>
              </Tooltip>
            </Group>
          ) : (
            <Stack gap="xs" align="center">
              {/* FR-72: 折叠态开源协议入口（仅图标） */}
              <Tooltip label={t("nav.licenses")} position="right" withArrow>
                <UnstyledButton
                  aria-label={t("nav.licenses")}
                  onClick={() => navigate("/licenses")}
                  style={{ display: "flex" }}
                >
                  <IconLicense size={16} />
                </UnstyledButton>
              </Tooltip>
              <Tooltip label={t("nav.expandNav")} position="right" withArrow>
                <UnstyledButton
                  aria-label={t("nav.expandNav")}
                  onClick={toggleNav}
                  style={{ display: "flex" }}
                >
                  <IconLayoutSidebarLeftExpand size={18} />
                </UnstyledButton>
              </Tooltip>
            </Stack>
          )}
        </Box>
      </AppShell.Navbar>

      <AppShell.Main>
        {/* 固定 max-width 居中内容容器：新内容出现不再撑变形整体布局。 */}
        <Box
          data-testid="content-shell"
          style={{ maxWidth: density.contentMaxWidth, marginInline: "auto" }}
        >
          <Outlet />
        </Box>
      </AppShell.Main>
    </AppShell>
  );
}
