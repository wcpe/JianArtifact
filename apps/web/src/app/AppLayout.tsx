// 管理端控制台外壳：左上品牌 logo 区（SVG + 品牌 + 版本号，点 logo 切换导航展开/收起）
// + 分段导航（浏览 / 管理）+ 左下 footer（折叠按钮）+ 固定 max-width 内容区。
// 收起态仅图标（Tooltip + aria-label 可达）、段间以分隔线代替段头；据角色显隐管理入口。
// 视觉沿用旧项目控制台外壳（AppShell layout="alt"）。
import {
  ActionIcon,
  Anchor,
  AppShell,
  Box,
  Breadcrumbs,
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
} from "@mantine/core";
import { useDisclosure, useLocalStorage, useMediaQuery } from "@mantine/hooks";
import {
  IconKey,
  IconLayoutDashboard,
  IconLayoutList,
  IconLayoutSidebarLeftCollapse,
  IconLayoutSidebarLeftExpand,
  IconFileReport,
  IconActivity,
  IconLicense,
  IconLogin,
  IconPackage,
  IconRefresh,
  IconSearch,
  IconSettings,
  IconTransfer,
  IconUsers,
} from "@tabler/icons-react";
import { useEffect, useState, Suspense } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link, Outlet, useLocation, useNavigate } from "react-router-dom";

import { getStatus, listPublicRepositories } from "../api/endpoints";
import { getNetworkActivityCount, subscribeNetworkActivity } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { useLoginModal } from "../auth/LoginModal";
import { BrandLogo } from "../components/BrandLogo";
import { AccountMenu } from "../components/account/AccountMenu";
import { RouteErrorBoundary } from "../components/RouteErrorBoundary";
import { RouteFallback } from "../components/RouteFallback";
import { REFRESH_EVENT } from "../hooks/useAsync";
import { density } from "../theme/density";
import { preloadRoute } from "./preloadRoute";
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

/** 判定导航项是否对应当前路由：按路径段精确匹配，避免前缀串台。
 * 仅当当前路径等于该项路径、或为其子路径（以「该项路径 + /」开头）时高亮。
 */
function isNavActive(pathname: string, itemPath: string): boolean {
  return pathname === itemPath || pathname.startsWith(`${itemPath}/`);
}

/** 页眉面包屑的末级文案覆盖：导航项标签之外更准确的页面名。 */
const NAV_BREADCRUMB_OVERRIDES: Record<string, string> = {
  "/dashboard": "业务仪表盘",
};

/** 面包屑项：label 必填；to 存在时渲染为可点击链接（末级始终不可点）。 */
interface Crumb {
  label: string;
  to?: string;
}

const NAV_SECTIONS: NavSection[] = [
  {
    titleKey: "nav.sectionOverview",
    items: [
      {
        labelKey: "nav.dashboard",
        path: "/dashboard",
        icon: <IconLayoutDashboard size={18} />,
        adminOnly: true,
      },
      { labelKey: "nav.repositories", path: "/repositories", icon: <IconPackage size={18} /> },
    ],
  },
  {
    titleKey: "nav.sectionOperations",
    items: [
      {
        labelKey: "nav.auditLogs",
        path: "/audit-logs",
        icon: <IconFileReport size={18} />,
        adminOnly: true,
      },
      {
        labelKey: "nav.hostMonitoring",
        path: "/host-monitoring",
        icon: <IconActivity size={18} />,
        adminOnly: true,
      },
    ],
  },
  {
    titleKey: "nav.sectionAdministration",
    items: [
      { labelKey: "nav.users", path: "/users", icon: <IconUsers size={18} />, adminOnly: true },
      { labelKey: "nav.tokens", path: "/tokens", icon: <IconKey size={18} /> },
      {
        labelKey: "nav.migrations",
        path: "/migrations",
        icon: <IconTransfer size={18} />,
        adminOnly: true,
      },
      {
        labelKey: "nav.settings",
        path: "/settings",
        icon: <IconSettings size={18} />,
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
  path,
  icon,
  expanded,
  active,
  onSelect,
}: {
  label: string;
  /**
   * 目标路由。既用于 hover / 聚焦时预取该页 chunk，也用于渲染成真正的 `<a href>`——
   * 只用 onClick 导航时链接没有 href，中键新开、复制链接地址、爬虫跟进全都失效。
   */
  path: string;
  icon: ReactNode;
  expanded: boolean;
  active: boolean;
  onSelect: () => void;
}) {
  const prefetch = () => preloadRoute(path);
  if (expanded) {
    return (
      <NavLink
        component={Link}
        to={path}
        label={label}
        aria-label={label}
        leftSection={icon}
        active={active}
        onClick={onSelect}
        onMouseEnter={prefetch}
        onFocus={prefetch}
      />
    );
  }
  return (
    <Tooltip label={label} position="right" withArrow>
      <NavLink
        component={Link}
        to={path}
        aria-label={label}
        leftSection={icon}
        active={active}
        onClick={onSelect}
        onMouseEnter={prefetch}
        onFocus={prefetch}
      />
    </Tooltip>
  );
}

/**
 * 页眉图标按钮（窄屏专用）：页眉在窄屏放不下「图标 + 文字」——文字会被裁掉只剩半个字，
 * 反而看不出是哪个操作。这里只渲染图标，文字经悬停 / **触摸长按**（`events.touch` 让
 * Tooltip 响应 touchstart）与 aria-label 可达，读屏与键盘名不缺。
 */
function HeaderIconAction({
  label,
  onClick,
  disabled,
  children,
}: {
  label: string;
  onClick: () => void;
  disabled?: boolean;
  children: ReactNode;
}) {
  return (
    <Tooltip
      label={label}
      position="bottom"
      openDelay={200}
      // hover 必须显式保留：Tooltip 的 events 是整体替换而非深合并，只写 touch 会把悬停开提示一起关掉。
      events={{ hover: true, focus: false, touch: true }}
    >
      <ActionIcon
        size="md"
        radius="md"
        variant="subtle"
        aria-label={label}
        onClick={onClick}
        disabled={disabled}
      >
        {children}
      </ActionIcon>
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

  // 挂载与登录态变化时查实例状态：版本号仅登录后由后端返回（匿名脱敏）；
  // 仅后端明确允许自举时才导向 /setup，备用节点空库保持只读浏览。
  useEffect(() => {
    let cancelled = false;
    getStatus()
      .then((info) => {
        if (cancelled) return;
        setVersion(info.version || null);
        if (info.bootstrapAllowed === true) {
          navigate("/setup", { replace: true });
        }
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
    // navigate 引用稳定；登录 / 登出后重取以刷新版本号展示。
  }, [isAuthenticated]);

  // FR-55: 未登录时加载公开仓库列表
  useEffect(() => {
    if (!isAuthenticated) {
      listPublicRepositories()
        .then((res) => setPublicRepos(res.items))
        .catch(() => setPublicRepos([]));
    }
  }, [isAuthenticated]);

  // FR-67：登出后落地仓库列表（匿名视图），不再有整页登录。
  // FR-71：登出为异步操作，期间按钮呈 loading 防重复点击。
  const [loggingOut, setLoggingOut] = useState(false);
  const handleLogout = () => {
    setLoggingOut(true);
    void logout()
      .then(() => {
        navigate("/repositories", { replace: true });
      })
      .finally(() => setLoggingOut(false));
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

  // 页眉面包屑：由当前路由推导，覆盖全部页面（导航页 / 详情页 / 搜索 / 开源协议）。
  // 非末级带上 to，渲染为可点击链接——此前一律是纯文本，用户从列表进详情后
  // 既点不动面包屑、侧栏也没有回列表的入口，等于"进去就出不来"。
  const crumbs: Crumb[] = (() => {
    const p = location.pathname;
    // 仓库详情 / 仓库权限：概览 / 仓库(可点) / <name> [/ 访问控制]
    if (p.startsWith("/repositories/")) {
      const rest = decodeURIComponent(p.slice("/repositories/".length));
      const segments = rest.split("/");
      const name = segments[0] ?? "";
      const sub = segments[1];
      const items: Crumb[] = [
        { label: t("nav.sectionOverview") },
        { label: t("nav.repositories"), to: "/repositories" },
        { label: name },
      ];
      if (sub === "acl") items.push({ label: t("acl.title", { defaultValue: "访问控制" }) });
      return items;
    }
    // 迁移向导 / 迁移详情：管理 / 迁移(可点) / <new|#id>
    if (p.startsWith("/migrations/")) {
      const rest = p.slice("/migrations/".length);
      const last = rest === "new" ? t("migrations.new", { defaultValue: "新建迁移" }) : `#${rest}`;
      return [
        { label: t("nav.sectionAdministration") },
        { label: t("nav.migrations"), to: "/migrations" },
        { label: last },
      ];
    }
    // 搜索（不在侧栏导航内）：制品搜索
    if (p === "/search" || p.startsWith("/search?") || p.startsWith("/search/")) {
      return [{ label: t("search.title", { defaultValue: "制品搜索" }) }];
    }
    // 开源协议（侧栏底部独立入口）：开源协议
    if (p.startsWith("/licenses")) {
      return [{ label: t("nav.licenses", { defaultValue: "开源协议" }) }];
    }
    // 其余命中侧栏导航的页面：段标题 + 页面名(可点)
    for (const section of NAV_SECTIONS) {
      for (const item of section.items) {
        if (isNavActive(p, item.path)) {
          return [
            { label: t(section.titleKey) },
            {
              label: NAV_BREADCRUMB_OVERRIDES[item.path] ?? t(item.labelKey),
              to: item.path,
            },
          ];
        }
      }
    }
    return [];
  })();

  // 窄屏只保留末两级面包屑：手机整条会被 truncate 成「概…」，
  // 保留「父级 + 当前页」既有定位感，又留出可点的返回入口。
  const visibleCrumbs = isMobile && crumbs.length > 2 ? crumbs.slice(-2) : crumbs;

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

  // FR-59/FR-71: 刷新当前页——派发全局刷新事件，useAsync 与自管数据组件重新拉取；
  // 按钮进入旋转态，待网络活动归零（且满足最短时长防闪烁）后恢复。
  const [refreshing, setRefreshing] = useState(false);
  const handleRefresh = () => {
    if (refreshing) return;
    setRefreshing(true);
    window.dispatchEvent(new CustomEvent(REFRESH_EVENT));
  };

  useEffect(() => {
    if (!refreshing) return;
    let minElapsed = false;
    let idle = getNetworkActivityCount() === 0;
    let finished = false;
    const finish = () => {
      if (finished) return;
      finished = true;
      setRefreshing(false);
    };
    const tryFinish = () => {
      if (minElapsed && idle) finish();
    };
    // 最短旋转 400ms：即便请求瞬间返回也有可感知的反馈，避免图标闪烁。
    const minTimer = window.setTimeout(() => {
      minElapsed = true;
      tryFinish();
    }, 400);
    // 最长 10s 兜底：全局网络计数会被**任何**在途请求占用（含其他组件的后台轮询），
    // 只等计数归零时，一个挂起的慢请求就能把刷新按钮永久锁在禁用旋转态——
    // 用户看到的就是"卡住了，连刷新都点不动"。
    const maxTimer = window.setTimeout(finish, 10_000);
    const unsubscribe = subscribeNetworkActivity((count) => {
      idle = count === 0;
      tryFinish();
    });
    return () => {
      window.clearTimeout(minTimer);
      window.clearTimeout(maxTimer);
      unsubscribe();
    };
  }, [refreshing]);

  return (
    <AppShell
      layout="alt"
      header={{ height: density.headerHeight }}
      navbar={{ width: navbarWidth, breakpoint: "sm", collapsed: { mobile: !mobileOpened } }}
      padding={density.mainPadding}
    >
      <AppShell.Header>
        <Group h="100%" px={{ base: "xs", sm: "md" }} wrap="nowrap" justify="space-between">
          <Group gap="sm" wrap="nowrap" style={{ flex: 1, minWidth: 0 }}>
            <Burger
              opened={mobileOpened}
              onClick={toggleMobile}
              aria-label={t("nav.toggleNav")}
              hiddenFrom="sm"
              size="sm"
            />
            {/* 页眉面包屑（全端）：概览 / 业务仪表盘 等；移动端替代品牌文案，末级常显 */}
            {crumbs.length > 0 ? (
              <Breadcrumbs
                separator="/"
                data-testid="app-breadcrumbs"
                style={{ minWidth: 0, flex: 1, overflow: "hidden" }}
              >
                {visibleCrumbs.map((crumb, index) => {
                  const isLast = index === visibleCrumbs.length - 1;
                  const key = `${crumb.label}-${index}`;
                  // 非末级且有目标路由 → 可点击链接（回上级列表）；末级是当前页，保持纯文本。
                  if (crumb.to && !isLast) {
                    return (
                      <Anchor
                        key={key}
                        component={Link}
                        to={crumb.to}
                        size="sm"
                        c="dimmed"
                        fw={500}
                        underline="hover"
                        truncate
                        style={{ minWidth: 0 }}
                      >
                        {crumb.label}
                      </Anchor>
                    );
                  }
                  return (
                    <Text
                      key={key}
                      size="sm"
                      c={isLast ? undefined : "dimmed"}
                      fw={isLast ? 600 : 500}
                      lh={1.2}
                      truncate
                      style={isLast ? { flexShrink: 0 } : { minWidth: 0 }}
                    >
                      {crumb.label}
                    </Text>
                  );
                })}
              </Breadcrumbs>
            ) : (
              <Group gap="xs" wrap="nowrap" hiddenFrom="sm">
                <BrandLogo size={24} />
                <Text fw={700} size="sm">
                  {t("common.appName")}
                </Text>
              </Group>
            )}
          </Group>
          {/* FR-59: Header 搜索栏 */}
          <Group
            gap="sm"
            wrap="nowrap"
            style={{ flex: 2, minWidth: 0 }}
            justify="center"
            visibleFrom="sm"
          >
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
          {/* flexShrink: 0：页眉右侧按钮带文字后不该被压缩截断，让面包屑去占剩余空间
              （面包屑已按窄屏 slice(-2) 收敛）。 */}
          <Group
            gap="xs"
            wrap="nowrap"
            justify="flex-end"
            style={{ flex: 1, minWidth: 0, flexShrink: 0 }}
          >
            {/* 窄屏：搜索 / 刷新退化为纯图标（页眉放不下文字，硬塞会被裁成半个字），
                文字经悬停或长按 Tooltip 可达；桌面仍按全站约定「图标 + 文字」。 */}
            {isMobile ? (
              <>
                <HeaderIconAction
                  label={t("common.search", { defaultValue: "搜索" })}
                  onClick={() => navigate("/search")}
                >
                  <IconSearch size={16} />
                </HeaderIconAction>
                <HeaderIconAction
                  label={t("common.refresh", { defaultValue: "刷新" })}
                  onClick={handleRefresh}
                  disabled={refreshing}
                >
                  <IconRefresh
                    size={16}
                    style={refreshing ? { animation: "ja-spin 0.9s linear infinite" } : undefined}
                  />
                </HeaderIconAction>
              </>
            ) : (
              /* FR-59/FR-71: 刷新按钮——刷新期间禁用并旋转 */
              <Button
                size="compact-sm"
                variant="subtle"
                leftSection={
                  <IconRefresh
                    size={16}
                    style={refreshing ? { animation: "ja-spin 0.9s linear infinite" } : undefined}
                  />
                }
                aria-label={t("common.refresh", { defaultValue: "刷新" })}
                onClick={handleRefresh}
                disabled={refreshing}
              >
                {t("common.refresh", { defaultValue: "刷新" })}
              </Button>
            )}
            {user ? (
              <AccountMenu
                user={user}
                roleLabel={roleLabel}
                accountLabel={t("nav.accountLabel")}
                logoutLabel={t("common.logout")}
                loggingOutLabel={t("nav.loggingOut")}
                loggingOut={loggingOut}
                onLogout={handleLogout}
              />
            ) : isMobile ? (
              <Button
                size="compact-sm"
                variant="light"
                leftSection={<IconLogin size={16} />}
                aria-label={t("auth.login", { defaultValue: "登录" })}
                onClick={() => openLogin()}
              >
                {t("auth.login", { defaultValue: "登录" })}
              </Button>
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
              {isAuthenticated && version ? (
                <Text size="xs" c="dimmed" lh={1.2}>
                  v{version}
                </Text>
              ) : null}
            </Stack>
          ) : null}
        </Group>

        <ScrollArea style={{ flex: 1 }}>
          {/* FR-55: 未登录 — 精简侧边栏。
              补「全部仓库」固定入口：此前匿名侧栏只列公开仓库名，进入某个仓库详情后
              没有任何路径回到列表（面包屑当时也不可点），等于"进去就出不来"。 */}
          {!isAuthenticated ? (
            <Box>
              {expanded && (
                <Text size="xs" c="dimmed" fw={600} px="xs" py={4}>
                  {t("nav.publicRepos", { defaultValue: "公开仓库" })}
                </Text>
              )}
              <NavItemLink
                label={t("nav.allRepositories", { defaultValue: "全部仓库" })}
                path="/repositories"
                icon={<IconLayoutList size={18} />}
                expanded={expanded}
                active={location.pathname === "/repositories"}
                onSelect={() => {
                  navigate("/repositories");
                  if (mobileOpened) closeMobile();
                }}
              />
              {publicRepos.length === 0 ? (
                expanded ? (
                  <Text size="xs" c="dimmed" px="xs" py={4}>
                    {t("nav.noPublicRepos", { defaultValue: "暂无公开仓库" })}
                  </Text>
                ) : null
              ) : (
                publicRepos.map((repo) => (
                  <NavItemLink
                    key={repo.name}
                    label={repo.name}
                    path={`/repositories/${repo.name}`}
                    icon={<IconPackage size={18} />}
                    expanded={expanded}
                    active={isNavActive(location.pathname, `/repositories/${repo.name}`)}
                    onSelect={() => {
                      navigate(`/repositories/${repo.name}`);
                      if (mobileOpened) closeMobile();
                    }}
                  />
                ))
              )}
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
                    path={item.path}
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

        {isAuthenticated && isAdmin ? (
          <Box
            mt="xs"
            pt="xs"
            style={{ borderTop: "1px solid var(--mantine-color-default-border)" }}
          >
            <NavItemLink
              label={t("nav.licenses")}
              path="/licenses"
              icon={<IconLicense size={18} />}
              expanded={expanded}
              active={isNavActive(location.pathname, "/licenses")}
              onSelect={() => {
                navigate("/licenses");
                if (mobileOpened) closeMobile();
              }}
            />
          </Box>
        ) : null}

        {/* 左下仅保留导航展开控制，开源协议独立置于其上方。 */}
        <Box
          mt="xs"
          pt="xs"
          visibleFrom="sm"
          style={{ borderTop: "1px solid var(--mantine-color-default-border)" }}
        >
          {expanded ? (
            <NavLink
              label={t("nav.collapseNav")}
              aria-label={t("nav.collapseNav")}
              leftSection={<IconLayoutSidebarLeftCollapse size={18} />}
              onClick={toggleNav}
            />
          ) : (
            <Tooltip label={t("nav.expandNav")} position="right" withArrow>
              <NavLink
                aria-label={t("nav.expandNav")}
                leftSection={<IconLayoutSidebarLeftExpand size={18} />}
                onClick={toggleNav}
              />
            </Tooltip>
          )}
        </Box>
      </AppShell.Navbar>

      <AppShell.Main>
        {/* 内容容器：限定最大宽度并居中，超宽屏下不再把表格与卡片无节制拉长。
            此前该容器只有注释声称「固定 max-width」，实际仅 width:100%，
            导致 density.contentMaxWidth 长期空转、各页各自写 maw。 */}
        <Box
          data-testid="content-shell"
          style={{ width: "100%", maxWidth: density.contentMaxWidth, marginInline: "auto" }}
        >
          {/* FR-70：懒加载页面在布局内挂 Suspense，路由切换保持侧栏/页眉不闪。 */}
          {/* 错误边界按路径强制重建：否则一次懒加载失败会把错误态带到后续所有页面。
              没有它时，chunk 加载失败（发版后旧 chunk 404、网络抖动、dev 依赖重构建 504）
              会让整棵树崩成白屏。 */}
          <RouteErrorBoundary key={location.pathname}>
            <Suspense fallback={<RouteFallback />}>
              <Outlet />
            </Suspense>
          </RouteErrorBoundary>
        </Box>
      </AppShell.Main>
    </AppShell>
  );
}
