// 仓库详情：Tab 布局（浏览/配置/ACL）。管理员可见配置与 ACL，普通用户仅浏览。
import {
  Badge,
  Button,
  Card,
  Divider,
  Group,
  Modal,
  MultiSelect,
  Select,
  Skeleton,
  Stack,
  Switch,
  Table,
  Tabs,
  TagsInput,
  Text,
  Textarea,
  TextInput,
  Title,
  Tooltip,
} from "@mantine/core";
import {
  IconDeviceFloppy,
  IconPencil,
  IconPlus,
  IconRefresh,
  IconTag,
  IconTrash,
  IconX,
} from "@tabler/icons-react";
import { useMediaQuery } from "@mantine/hooks";
import { EmptyState } from "@jianartifact/ui";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";

import { PageShell } from "../app/PageShell";
import { currentLocaleTag } from "../i18n/current";
import { RepoBrowser, type RepoDownloadTrendView } from "../components/repo/RepoBrowser";
import { AsyncBoundary } from "../components/AsyncBoundary";
import {
  getAcl,
  getRepositoryDownloadTrend,
  listAllRepositories,
  listUserGroups,
  listUsers,
  recheckConnection,
  renameRepository,
  setAcl,
  setRepositoryOnline,
  updateRepository,
} from "../api/endpoints";
import type {
  ConnectionStatusValue,
  AclAction,
  AclEntry,
  AclSubjectType,
  RepoVisibility,
  Repository,
  User,
  UserGroup,
} from "../api/types";
import { ApiError } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { useIsAdmin } from "../auth/useIsAdmin";
import { useAsync } from "../hooks/useAsync";
import {
  ACL_ACTIONS,
  ACL_ACTION_HINT_KEYS,
  ACL_ACTION_LABEL_KEYS,
  aclSubjectKey,
  aclSubjectLabel,
  aclSubjectType,
  buildAclEntry,
} from "../lib/acl";
import { CONN_COLOR, CONN_LABEL_KEY } from "../lib/connectionStatus";
import { formatBytes, formatCount, formatStamp } from "../lib/format";
import { OpsHelpButton } from "../components/ops/OpsKit";
import {
  QUOTA_NEAR_RATIO,
  QUOTA_STATE_COLOR,
  QUOTA_STATE_LABEL_KEY,
  QUOTA_UNITS,
  hasQuotaLimit,
  parseNonNegativeInt,
  parseQuotaBytes,
  quotaState,
  repoQuotaState,
  splitQuotaBytes,
  type QuotaState,
  type QuotaUnit,
} from "../lib/quota";
import { notifyError, notifySuccess } from "../lib/feedback";
import { formatUtcToLocalDate } from "../lib/timeFormat";
import { density } from "../theme/density";

export function RepositoryDetailPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { name = "" } = useParams();
  // FR-145：搜索直达的命中路径（仅作进入时的初始定位提示，不随后续交互写回）。
  const [searchParams, setSearchParams] = useSearchParams();
  const highlightPath = searchParams.get("highlight") ?? undefined;
  const { user } = useAuth();
  // 管理员判定走集中工具（FR-36）：角色口径只有一处，页面不再自己解释 role 字段。
  const isAdmin = useIsAdmin();
  const requestedTab = searchParams.get("tab");
  const tab =
    requestedTab === "config" || requestedTab === "acl" || requestedTab === "browse"
      ? isAdmin || requestedTab === "browse"
        ? requestedTab
        : "browse"
      : "browse";
  const changeTab = (value: string | null) => {
    if (!value) return;
    const next = new URLSearchParams(searchParams);
    next.set("tab", value);
    setSearchParams(next, { replace: true });
  };
  // 有登录即可尝试上传（后端校验 write）
  const allowUpload = Boolean(user);

  // 拉取仓库信息：统一走公开列表 API——匿名可读且响应带 artifactCount/totalSize 统计。
  // 此前匿名走 getRepositoryUsage（该端点只含 format/type/description），匿名浏览公开
  // 仓库详情时概览带显示「制品数 0 · 体积 0 B」（用户反馈）；列表项字段足以覆盖所需。
  // 匿名访问 private 仓库时列表不含该项 → null，由下方「未认证」分支处理。
  const repoState = useAsync(
    () =>
      listAllRepositories().then(
        (list) => list.items.find((r) => r.name === name || r.aliases?.includes(name)) ?? null,
      ),
    [name],
    { cacheKey: `repo:detail:${name}` },
  );
  const repo = repoState.data ?? null;
  // 端点 B（非契约 /download-trend）：全时段累计下载次数 + 近 24h 补零趋势；
  // 权限与仓库树同级，匿名 private 会 401——失败时页头统计与小图各自降级，不拖垮详情页。
  const downloadTrendState = useAsync(() => getRepositoryDownloadTrend(name), [name], {
    cacheKey: `repo:download-trend:${name}`,
  });
  const downloadTrend = downloadTrendState.data ?? null;
  // 趋势点映射为 TrendChart 的 {label, value}：label 与仪表盘同用桶起点本地时间戳。
  const downloadTrendPoints = (downloadTrend?.trend ?? []).map((point) => ({
    label: formatStamp(point.from),
    value: point.downloadCount,
  }));
  // 下传给 RepoBrowser 的趋势视图（渲染在右侧详情卡片顶部）：
  // 下载趋势是仓库级信息，与「当前选中文件」无关，故由页面取数后统一交给浏览区展示，
  // 避免 RepoBrowser 重复请求同一端点。三态与原先顶层整行渲染时完全一致。
  const downloadTrendView: RepoDownloadTrendView = {
    points: downloadTrend ? downloadTrendPoints : null,
    loading: downloadTrendState.loading && !downloadTrend,
    error: Boolean(downloadTrendState.error) && !downloadTrend,
    // 折叠态摘要用：页面已取的全时段累计下载数，接口未就绪/失败为 null（不新增请求）。
    total: downloadTrend?.totalDownloadCount ?? null,
  };
  // 窄屏（< 48em）：页签与徽章必然折成两行，面板上边距也收一档，尽可能把高度留给内容。
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;

  return (
    <PageShell testId="repo-detail-shell">
      <Tabs
        value={tab}
        onChange={changeTab}
        styles={{ tab: { paddingTop: 6, paddingBottom: 6 } }}
        style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
      >
        {/* 页头与页签并排：宽屏一行放得下（省掉整整一行），窄屏自动折成
            「页签 / 徽章 + 关闭」两行。仓库名由页眉面包屑承担，此处只表达仓库属性。 */}
        <Group justify="space-between" align="center" wrap="wrap" gap="xs" mb="xs">
          <Tabs.List
            data-testid="repo-detail-tabs"
            style={{
              position: "sticky",
              top: 0,
              zIndex: 1,
              flexWrap: "nowrap",
              overflowX: "auto",
              background: "var(--mantine-color-body)",
            }}
          >
            <Tabs.Tab value="browse">{t("repoDetail.tabBrowse")}</Tabs.Tab>
            {isAdmin && <Tabs.Tab value="config">{t("repoDetail.tabConfig")}</Tabs.Tab>}
            {isAdmin && <Tabs.Tab value="acl">{t("repoDetail.tabAcl")}</Tabs.Tab>}
          </Tabs.List>

          {/* 中间概览：页签与右侧徽章之间原本是 900px 空白带（1440 实测），
              用仓库自身的只读统计填上，顺便让「这个仓库多大/多新/上游通不通」一眼可见。
              窄屏（<48em）隐藏——那里空间要留给页签与徽章。 */}
          {repo ? (
            <Group
              gap="lg"
              wrap="nowrap"
              align="center"
              visibleFrom="md"
              style={{ flex: 1, justifyContent: "center", minWidth: 0 }}
            >
              <DetailStat
                label={t("repoDetail.statArtifacts")}
                value={(repo.artifactCount ?? 0).toLocaleString(currentLocaleTag())}
              />
              <DetailStat
                label={t("repoDetail.statSize")}
                value={formatBytes(repo.totalSize ?? 0)}
              />
              {/* 上游连接状态不在这里重复展示：它属于「配置」页签的正交信息，
                  同一文案在两个位置出现既冗余、也让按文本定位的用例产生歧义。 */}
              <DetailStat
                label={t("repoDetail.statCreatedAt")}
                value={formatUtcToLocalDate(repo.createdAt)}
              />
              {/* 总下载次数：端点 B 的全时段累计（与仓库树 downloadCount 同口径）；
                  接口未就绪（匿名 401 / 加载中）时不渲染，避免「0」假象。 */}
              {downloadTrend ? (
                <DetailStat
                  label={t("repoDetail.statDownloads")}
                  value={formatCount(downloadTrend.totalDownloadCount)}
                />
              ) : null}
            </Group>
          ) : null}

          {/* 徽章与「关闭」都取紧凑尺寸：它们只是属性标注与返回入口，不该占掉一行。 */}
          <Group gap={4} wrap="wrap" align="center">
            {repo ? (
              <>
                <Badge size="xs" variant="light">
                  {repo.format}
                </Badge>
                <Badge size="xs" variant="outline" color="gray">
                  {repo.type}
                </Badge>
                <Badge
                  size="xs"
                  variant="light"
                  color={repo.visibility === "public" ? "blue" : "gray"}
                >
                  {repo.visibility === "public"
                    ? t("repositories.visibilityPublic")
                    : t("repositories.visibilityPrivate")}
                </Badge>
                {/* FR-41：配额状态（接近上限 / 已超限）对所有登录用户可见，只读。 */}
                <QuotaStatusBadge repo={repo} />
              </>
            ) : (
              // 慢接口下也保持页头有形，避免左侧长时间是空的。
              <Skeleton height={20} width={150} radius="sm" />
            )}
            {/* FR-74：未登录的登录入口收敛到页眉（AppLayout），此处仅保留返回。 */}
            <Button
              size="compact-xs"
              variant="default"
              leftSection={<IconX size={14} />}
              onClick={() => navigate("/repositories")}
            >
              {t("common.close")}
            </Button>
          </Group>
        </Group>

        {/* FR-81：描述独立成一行，最多一行高（不再用页头大标题）。 */}
        {repo?.description ? (
          <Text size="xs" c="dimmed" lineClamp={1} mb="xs">
            {repo.description}
          </Text>
        ) : null}

        {/* 下载趋势不再占用页面顶部整行：改由 RepoBrowser 渲染在右侧详情卡片顶部
            （见 RepoBrowser 的 downloadTrend 区域），此处只负责取数并下传。 */}

        {/* 浏览 Tab：嵌入现有 RepoBrowser（填满剩余高度，内部面板各自滚动） */}
        <Tabs.Panel
          value="browse"
          pt={isNarrow ? "xs" : "sm"}
          style={{ flex: 1, minHeight: 0, overflow: "hidden" }}
        >
          <RepoBrowser
            repoName={name}
            allowUpload={allowUpload}
            publicMode={!user}
            highlightPath={highlightPath}
            downloadTrend={downloadTrendView}
          />
        </Tabs.Panel>

        {/* 配置 Tab：仅管理员，展示仓库信息 + 可修改 visibility */}
        {isAdmin && (
          <Tabs.Panel
            value="config"
            pt={isNarrow ? "xs" : "sm"}
            style={{ flex: 1, minHeight: 0, overflowY: "auto" }}
          >
            <ConfigTab repoName={name} repo={repo} onUpdated={repoState.reload} />
          </Tabs.Panel>
        )}

        {/* ACL Tab：仅管理员，内联 ACL 管理 */}
        {isAdmin && (
          <Tabs.Panel
            value="acl"
            pt={isNarrow ? "xs" : "sm"}
            style={{ flex: 1, minHeight: 0, overflowY: "auto" }}
          >
            <AclPanel repoName={name} />
          </Tabs.Panel>
        )}
      </Tabs>
    </PageShell>
  );
}

/** 页头中间的紧凑统计项：小字灰标签 + 加粗值，用于填掉页签与徽章之间的空白带。 */
function DetailStat({ label, value, color }: { label: string; value: string; color?: string }) {
  return (
    <Group gap={6} wrap="nowrap" align="baseline">
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Text size="sm" fw={600} c={color}>
        {value}
      </Text>
    </Group>
  );
}

/**
 * FR-41：用量行的文字颜色——只在需要预警的两个状态着色（接近上限橙、已超限红）；
 * 正常与不限保持默认前景色（灰色会被误读成「禁用/失效」）。
 */
function quotaLineColor(state: QuotaState): string | undefined {
  return state === "near" || state === "over" ? QUOTA_STATE_COLOR[state] : undefined;
}

/**
 * FR-41：页头的仓库配额状态徽章（管理员与非管理员都能看到，只读）。
 *
 * 只在「接近上限 / 已超限」时渲染：正常与不限状态下没有信息量，不该给每个仓库都挂一个绿徽章。
 * 超限的含义是服务端开始拒绝写入（HTTP 429 / quota_exceeded），故用红色并给出明确提示。
 */
function QuotaStatusBadge({ repo }: { repo: Repository }) {
  const { t } = useTranslation();
  const state = repoQuotaState(repo);
  if (state !== "near" && state !== "over") return null;
  const label = t(QUOTA_STATE_LABEL_KEY[state]);
  return (
    <Tooltip
      label={
        state === "over"
          ? t("quota.overHint")
          : t("quota.nearHint", { percent: Math.round(QUOTA_NEAR_RATIO * 100) })
      }
      position="top"
      withArrow
    >
      <Badge
        size="xs"
        variant="light"
        color={QUOTA_STATE_COLOR[state]}
        data-testid="repo-quota-badge"
      >
        {label}
      </Badge>
    </Tooltip>
  );
}

/**
 * 别名前端校验：返回错误提示的 i18n 键（无错误返回 null）。
 * 规则：非空、不得等于仓库主名、不得重复；后端仍会做全局唯一性兜底。
 */
function aliasValidationKey(list: string[], repoName: string): string | null {
  const seen = new Set<string>();
  for (const raw of list) {
    const alias = raw.trim();
    if (!alias) return "repoDetail.configAliasInvalid";
    if (alias === repoName) return "repoDetail.configAliasSelf";
    if (seen.has(alias)) return "repoDetail.configAliasTaken";
    seen.add(alias);
  }
  return null;
}

/** 配置 Tab：展示仓库基本信息，可修改 visibility 与描述并保存。 */
function ConfigTab({
  repoName,
  repo,
  onUpdated,
}: {
  repoName: string;
  repo: Repository | null;
  onUpdated: () => void;
}) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [visibility, setVisibility] = useState<RepoVisibility>("private");
  const [description, setDescription] = useState("");
  const [members, setMembers] = useState<string[]>([]);
  // 仓库别名：与主名共享命名空间、全局唯一；随「保存配置」一并覆盖式提交。
  const [aliases, setAliases] = useState<string[]>([]);
  // FR-41 存储配额：字节按所选单位（MB/GB/字节）填写，保存时换算成字节；0 或留空 = 不限。
  // 存成字符串是为了区分「空输入」（= 不限）与「非法输入」（负数/非数字，需拦截）。
  const [quotaBytesText, setQuotaBytesText] = useState("0");
  const [quotaBytesUnit, setQuotaBytesUnit] = useState<QuotaUnit>("GB");
  const [quotaAssetsText, setQuotaAssetsText] = useState("0");
  // FR-41 代理缓存保留天数（仅 proxy 仓库可设）；0 或留空 = 关闭（不自动清理）。
  const [cacheRetentionText, setCacheRetentionText] = useState("0");
  const [saving, setSaving] = useState(false);
  const [online, setOnline] = useState(true);
  const [togglingOnline, setTogglingOnline] = useState(false);
  const [rechecking, setRechecking] = useState(false);
  // FR-114：连接状态（本地 state 承载重测后的即时更新，避免等待整页刷新）
  const [connStatus, setConnStatus] = useState<ConnectionStatusValue | null>(null);
  // 重命名弹窗：仅管理员；成功后旧名自动转为别名，需导航到新名详情。
  const [renameOpened, setRenameOpened] = useState(false);
  const [renameValue, setRenameValue] = useState("");
  const [renaming, setRenaming] = useState(false);
  const [renameError, setRenameError] = useState<string | null>(null);
  // group 成员候选：当前列表中同格式、非本仓的仓库名。
  const reposState = useAsync(() => listAllRepositories(), [], {
    cacheKey: "repositories:options",
  });
  const memberOptions = (reposState.data?.items ?? [])
    .filter((r) => r.format === repo?.format && r.name !== repo?.name)
    .map((r) => r.name);

  // 仓库信息就绪后同步初始 visibility/描述/members/aliases/online/连接状态
  useEffect(() => {
    if (repo) {
      setVisibility(repo.visibility);
      setDescription(repo.description ?? "");
      setMembers(repo.members ?? []);
      setAliases(repo.aliases ?? []);
      // FR-41：配额回显成「数值 + 单位」（能整除 GB 就用 GB），避免管理员对着字节数心算。
      const quotaBytesInput = splitQuotaBytes(repo.quotaBytes);
      setQuotaBytesText(quotaBytesInput.value);
      setQuotaBytesUnit(quotaBytesInput.unit);
      setQuotaAssetsText(String(hasQuotaLimit(repo.quotaAssets) ? repo.quotaAssets : 0));
      // FR-41：保留天数 0 / 缺省 = 关闭（对非 proxy 仓库后端恒为 0，这里一并按关闭渲染）。
      setCacheRetentionText(
        String(hasQuotaLimit(repo.cacheRetentionDays) ? repo.cacheRetentionDays : 0),
      );
      setOnline(repo.online ?? true);
      setConnStatus(repo.connectionStatus?.status ?? null);
    }
  }, [repo]);

  // 别名前端校验：返回错误提示的 i18n 键（无错误返回 null）。后端仍会兜底校验。
  const aliasErrorKey = aliasValidationKey(aliases, repo?.name ?? repoName);

  // 配额输入校验：null 表示非法（非数字 / 负数）。空串按 0（不限 / 关闭）处理。
  const quotaBytesValue = parseQuotaBytes(quotaBytesText, quotaBytesUnit);
  const quotaAssetsValue = parseNonNegativeInt(quotaAssetsText);
  const cacheRetentionValue = parseNonNegativeInt(cacheRetentionText);
  // FR-41：存储配额只有承载写入的 hosted 仓库可设——group 不承载写入，proxy 的缓存在读取回源
  // 路径上落盘、没有准入预检与流式早拒，后端对这两类仓库提交非 0 配额直接 400。
  // 因此配额两项按类型出现/隐藏，校验与提交也只对 hosted 生效（与「保留天数仅 proxy」同一范式）。
  const quotaEditable = repo?.type === "hosted";
  const quotaInvalid =
    (quotaEditable && (quotaBytesValue === null || quotaAssetsValue === null)) ||
    cacheRetentionValue === null;
  // 当前用量（来自仓库响应的只读统计字段）与上限的状态：正常 / 接近上限 / 已超限。
  const bytesState = quotaState(repo?.totalSize, repo?.quotaBytes);
  const assetsState = quotaState(repo?.artifactCount, repo?.quotaAssets);
  const repoQuota = repoQuotaState(repo ?? {});
  const quotaUnlimitedText = t("quota.unlimited");

  // FR-113：online/offline 开关（仅管理员；本地运维状态，不参与复制）。
  const handleToggleOnline = (next: boolean) => {
    setTogglingOnline(true);
    setRepositoryOnline(repoName, next)
      .then(() => {
        setOnline(next);
        // FR-114：离线后状态优先显示「离线」；上线后回到内存态。
        setConnStatus(next ? (repo?.connectionStatus?.status ?? "READY") : "OFFLINE");
        onUpdated();
        notifySuccess(t("common.saved"));
      })
      .catch(notifyError)
      .finally(() => setTogglingOnline(false));
  };

  // FR-114：手动重测上游连接（仅 online proxy），成功后用返回状态即时更新。
  const handleRecheck = () => {
    setRechecking(true);
    recheckConnection(repoName)
      .then((status) => {
        setConnStatus(status.status);
        onUpdated();
        notifySuccess(t("repoDetail.configRecheckOk"));
      })
      .catch(notifyError)
      .finally(() => setRechecking(false));
  };

  const handleSave = () => {
    // 别名非法时不提交（与后端兜底一致，但先给出可读提示）。
    if (aliasErrorKey) {
      notifyError(t(aliasErrorKey));
      return;
    }
    // FR-41：配额非法（负数 / 非数字）时不提交，避免把 NaN 或负值发给服务端。
    if (quotaInvalid) {
      notifyError(t("repoDetail.configQuotaInvalid"));
      return;
    }
    setSaving(true);
    const normalizedAliases = aliases.map((a) => a.trim()).filter(Boolean);
    const patch: {
      visibility: RepoVisibility;
      description: string;
      members?: string[];
      aliases: string[];
      quotaBytes?: number;
      quotaAssets?: number;
      cacheRetentionDays?: number;
    } = {
      visibility,
      description,
      ...(repo?.type === "group" ? { members } : {}),
      aliases: normalizedAliases,
      // 配额只有 hosted 可设：其他类型提交非 0 值会被后端 400，而非 hosted 也从不渲染这两个输入，
      // 故整字段按类型条件提交（0 = 不限必须显式提交，否则「缺省 = 不修改」会让清空上限静默失效）。
      ...(quotaEditable
        ? { quotaBytes: quotaBytesValue ?? 0, quotaAssets: quotaAssetsValue ?? 0 }
        : {}),
      // 保留天数只对 proxy 有意义：其他类型提交非 0 值会被后端拒绝（400），
      // 而非 proxy 也从不渲染该输入，故整字段按类型条件提交。
      ...(repo?.type === "proxy" ? { cacheRetentionDays: cacheRetentionValue ?? 0 } : {}),
    };
    updateRepository(repoName, patch)
      .then(() => {
        notifySuccess(t("common.saved"));
        onUpdated();
      })
      .catch(notifyError)
      .finally(() => setSaving(false));
  };

  const openRename = () => {
    setRenameValue(repo?.name ?? repoName);
    setRenameError(null);
    setRenameOpened(true);
  };

  // 重命名提交：非空、不得与当前名相同；成功后旧名转为别名，导航到新名详情
  // （路由参数 name 已失效，不导航会 404 / 停留在旧地址）。
  const handleRename = () => {
    const newName = renameValue.trim();
    if (!newName) {
      setRenameError(t("repoDetail.configRenameEmpty"));
      return;
    }
    if (newName === (repo?.name ?? repoName)) {
      setRenameError(t("repoDetail.configRenameSame"));
      return;
    }
    setRenaming(true);
    setRenameError(null);
    renameRepository(repoName, newName)
      .then(() => {
        setRenameOpened(false);
        notifySuccess(t("repoDetail.configRenameOk"));
        onUpdated();
        navigate(`/repositories/${encodeURIComponent(newName)}`);
      })
      .catch((err: unknown) => {
        // 409：名称或别名被占用（与后端 conflict 语义一致）。
        if (err instanceof ApiError && (err.status === 409 || err.code === "conflict")) {
          setRenameError(t("repoDetail.configRenameTaken"));
          return;
        }
        notifyError(err);
      })
      .finally(() => setRenaming(false));
  };

  if (!repo) {
    return <Text c="dimmed">{t("common.loading")}</Text>;
  }

  return (
    <Card withBorder padding={density.cardPadding} radius="md" maw={520}>
      <Stack gap="md">
        {/* 标题行右侧放「重命名」入口：它与配置区其它修改同级，属于管理操作。 */}
        <Group justify="space-between" align="center">
          <Title order={5}>{t("repoDetail.configTitle")}</Title>
          <Button
            size="compact-xs"
            variant="light"
            leftSection={<IconPencil size={14} />}
            onClick={openRename}
          >
            {t("repoDetail.configRename")}
          </Button>
        </Group>

        {/* 基本信息（只读展示） */}
        <Stack gap="xs">
          <Text size="sm" c="dimmed">
            {t("repoDetail.configName")}：{repo.name}
          </Text>
          <Text size="sm" c="dimmed">
            {t("repoDetail.configFormat")}：{repo.format}
          </Text>
          <Text size="sm" c="dimmed">
            {t("repoDetail.configType")}：{repo.type}
          </Text>
        </Stack>

        {/* FR-113：online/offline 开关（本地运维状态，不参与复制） */}
        <Switch
          label={t("repoDetail.configOnlineLabel", { defaultValue: "在线" })}
          description={t("repoDetail.configOnlineHint", {
            defaultValue: "离线后 group 读将跳过该仓库；本状态不随复制传播",
          })}
          checked={online}
          disabled={togglingOnline}
          onChange={(e) => handleToggleOnline(e.currentTarget.checked)}
        />

        {/* FR-114：连接状态展示 + 手动重测（仅 proxy 仓库可重测） */}
        <Group gap="sm" align="center" wrap="wrap">
          <Text size="sm" c="dimmed">
            {t("repoDetail.configConnection", { defaultValue: "上游连接状态" })}：
          </Text>
          {connStatus ? (
            <Badge variant="light" color={CONN_COLOR[connStatus] ?? "gray"} size="sm">
              {t(CONN_LABEL_KEY[connStatus] ?? "repositories.statusReady")}
            </Badge>
          ) : (
            <Text size="sm" c="dimmed">
              {t("repositories.statusNone", { defaultValue: "—" })}
            </Text>
          )}
          {repo.type === "proxy" && (
            <Button
              size="xs"
              variant="light"
              loading={rechecking}
              disabled={!online}
              onClick={handleRecheck}
              leftSection={<IconRefresh size={14} />}
            >
              {rechecking
                ? t("repoDetail.configRechecking", { defaultValue: "探测中…" })
                : t("repoDetail.configRecheck", { defaultValue: "重新探测" })}
            </Button>
          )}
        </Group>

        {/* 可见性修改 */}
        <Select
          label={t("repoDetail.configVisibility")}
          data={[
            { value: "private", label: t("repoDetail.configVisibilityPrivate") },
            { value: "public", label: t("repoDetail.configVisibilityPublic") },
          ]}
          value={visibility}
          onChange={(v) => v && setVisibility(v as RepoVisibility)}
          allowDeselect={false}
        />

        {/* FR-81：仓库描述，详情页页头展示 */}
        <Textarea
          label={t("repoDetail.configDescription")}
          placeholder={t("repoDetail.configDescriptionPlaceholder")}
          autosize
          minRows={2}
          maxRows={4}
          value={description}
          onChange={(e) => setDescription(e.currentTarget.value)}
        />

        {/* 仓库别名：与主名共享命名空间、全局唯一；可增删多个，随保存一并提交 */}
        <Stack gap={4}>
          <TagsInput
            label={t("repoDetail.configAliases")}
            description={t("repoDetail.configAliasesHint")}
            placeholder={t("repoDetail.configAliasAdd")}
            leftSection={<IconTag size={14} />}
            value={aliases}
            onChange={setAliases}
            error={aliasErrorKey ? t(aliasErrorKey) : undefined}
            clearable
          />
          {aliases.length === 0 ? (
            <Text size="xs" c="dimmed">
              {t("repoDetail.configAliasesEmpty")}
            </Text>
          ) : null}
        </Stack>

        {/* group 仓库：成员仓库（members）编辑 */}
        {repo.type === "group" && (
          <MultiSelect
            label={t("repoDetail.configMembers", { defaultValue: "成员仓库" })}
            description={t("repoDetail.configMembersHint", {
              defaultValue: "选择聚合进本 group 的仓库",
            })}
            data={memberOptions}
            searchable
            value={members}
            onChange={setMembers}
          />
        )}

        {/* FR-41 存储配额：用量展示（只读统计）+ 上限编辑（0 = 不限）。
            用量口径与服务端拒绝点一致：逻辑字节 SUM(asset.size) 与制品计数 COUNT(*)。 */}
        <Divider />
        <Stack gap="xs" data-testid="repo-quota-section">
          <Group justify="space-between" align="center" wrap="wrap">
            <Text size="sm" fw={600}>
              {t("repoDetail.configQuotaTitle")}
            </Text>
            <Badge
              variant="light"
              size="sm"
              color={QUOTA_STATE_COLOR[repoQuota]}
              data-testid="repo-quota-state"
            >
              {t(QUOTA_STATE_LABEL_KEY[repoQuota])}
            </Badge>
          </Group>
          {/* 说明文案按类型切换：hosted 讲「0 = 不限」，非 hosted 讲清为什么没有输入框。 */}
          <Text size="xs" c="dimmed">
            {quotaEditable
              ? t("repoDetail.configQuotaHint")
              : t("repoDetail.configQuotaHostedOnly")}
          </Text>

          {/* 当前用量 / 上限：每行各自的颜色独立（可能一个维度接近上限、另一个正常）；
              正常与不限都不着色，避免把「不限」渲染成看起来像禁用的灰字。 */}
          <Text size="sm" c={quotaLineColor(bytesState)}>
            {t("repoDetail.configQuotaUsageBytes", {
              used: formatBytes(repo.totalSize ?? 0),
              limit: hasQuotaLimit(repo.quotaBytes)
                ? formatBytes(repo.quotaBytes)
                : quotaUnlimitedText,
            })}
          </Text>
          <Text size="sm" c={quotaLineColor(assetsState)}>
            {t("repoDetail.configQuotaUsageAssets", {
              used: formatCount(repo.artifactCount ?? 0),
              limit: hasQuotaLimit(repo.quotaAssets)
                ? formatCount(repo.quotaAssets)
                : quotaUnlimitedText,
            })}
          </Text>
          {repoQuota === "over" ? (
            <Text size="xs" c="red" data-testid="repo-quota-over-notice">
              {t("repoDetail.configQuotaOverNotice")}
            </Text>
          ) : repoQuota === "near" ? (
            <Text size="xs" c="orange" data-testid="repo-quota-near-notice">
              {t("repoDetail.configQuotaNearNotice", {
                percent: Math.round(QUOTA_NEAR_RATIO * 100),
              })}
            </Text>
          ) : null}

          {/* 上限编辑（仅 hosted）：字节按 MB/GB 填写（不必心算字节），保存时换算成字节。 */}
          {quotaEditable ? (
            <>
              <Group align="flex-end" gap="xs" wrap="nowrap">
                <TextInput
                  style={{ flex: 1 }}
                  label={t("repoDetail.configQuotaBytes")}
                  value={quotaBytesText}
                  error={quotaBytesValue === null ? t("repoDetail.configQuotaInvalid") : undefined}
                  onChange={(e) => setQuotaBytesText(e.currentTarget.value)}
                />
                <Select
                  label={t("repoDetail.configQuotaUnit")}
                  data={QUOTA_UNITS.map((unit) => ({ value: unit, label: unit }))}
                  value={quotaBytesUnit}
                  onChange={(value) => value && setQuotaBytesUnit(value as QuotaUnit)}
                  allowDeselect={false}
                  w={92}
                />
              </Group>
              {quotaBytesValue === null ? null : (
                <Text size="xs" c="dimmed">
                  {quotaBytesValue > 0
                    ? t("repoDetail.configQuotaBytesConverted", {
                        size: formatBytes(quotaBytesValue),
                        bytes: formatCount(quotaBytesValue),
                      })
                    : t("repoDetail.configQuotaUnlimitedInput")}
                </Text>
              )}

              <TextInput
                label={t("repoDetail.configQuotaAssets")}
                value={quotaAssetsText}
                error={quotaAssetsValue === null ? t("repoDetail.configQuotaInvalid") : undefined}
                onChange={(e) => setQuotaAssetsText(e.currentTarget.value)}
              />
              {quotaAssetsValue === null ? null : (
                <Text size="xs" c="dimmed">
                  {quotaAssetsValue > 0
                    ? t("repoDetail.configQuotaAssetsConverted", {
                        count: formatCount(quotaAssetsValue),
                      })
                    : t("repoDetail.configQuotaUnlimitedInput")}
                </Text>
              )}
            </>
          ) : null}

          {/* FR-41 代理缓存保留：只对 proxy 有意义（hosted/group 无「可重拉的缓存」语义，
              后端对这些类型提交非 0 值会直接拒绝），因此整块按类型出现/隐藏。 */}
          {repo.type === "proxy" ? (
            <>
              <TextInput
                label={t("repoDetail.configCacheRetention")}
                description={t("repoDetail.configCacheRetentionHint")}
                value={cacheRetentionText}
                error={
                  cacheRetentionValue === null
                    ? t("repoDetail.configCacheRetentionInvalid")
                    : undefined
                }
                onChange={(e) => setCacheRetentionText(e.currentTarget.value)}
              />
              {cacheRetentionValue === null ? null : (
                <Text size="xs" c="dimmed">
                  {cacheRetentionValue > 0
                    ? t("repoDetail.configCacheRetentionValue", {
                        days: formatCount(cacheRetentionValue),
                      })
                    : t("repoDetail.configCacheRetentionOff")}
                </Text>
              )}
            </>
          ) : null}
        </Stack>

        <Group justify="flex-end">
          <Button
            onClick={handleSave}
            loading={saving}
            leftSection={<IconDeviceFloppy size={16} />}
          >
            {t("repoDetail.configSave")}
          </Button>
        </Group>
      </Stack>

      {/* 重命名弹窗：成功后旧名自动转为别名，旧链接仍可访问。 */}
      <Modal
        opened={renameOpened}
        onClose={() => setRenameOpened(false)}
        title={t("repoDetail.configRenameTitle")}
      >
        <Stack gap="sm">
          <TextInput
            label={t("repoDetail.configRenameNewName")}
            description={t("repoDetail.configRenameHint")}
            value={renameValue}
            error={renameError}
            onChange={(e) => {
              setRenameValue(e.currentTarget.value);
              if (renameError) setRenameError(null);
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter") handleRename();
            }}
          />
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setRenameOpened(false)}>
              {t("common.cancel")}
            </Button>
            <Button loading={renaming} onClick={handleRename}>
              {t("repoDetail.configRenameConfirm")}
            </Button>
          </Group>
        </Stack>
      </Modal>
    </Card>
  );
}

/**
 * ACL 管理面板：从 AclPage 提取的核心逻辑，接收 repoName prop。
 *
 * FR-36：主体从「只有用户」扩展到「用户 / 用户组」，动作从三档细化为六档。
 *
 * 覆盖写语义：PUT /repositories/{name}/acl 是**整份替换**，因此任何改动都必须基于
 * `entries`（当前完整列表）构造请求体——只提交新增/修改的那一条会把其余条目全部抹掉。
 * 这里所有写操作都走 `setEntries`（就地改这一份列表），保存时整体下发，正是为此。
 */
function AclPanel({ repoName }: { repoName: string }) {
  const { t } = useTranslation();

  // 并行拉取 ACL 条目、用户列表与用户组列表：
  // - 用户列表用于「用户」主体的下拉与 id→用户名映射；
  // - 用户组列表用于「用户组」主体的下拉与 id→组名映射（组主体条目没有 subjectId，
  //   没有这份映射就只能显示成裸 ID）。
  const state = useAsync(
    () =>
      Promise.all([
        getAcl(repoName),
        listUsers({ page_size: 100 }),
        listUserGroups({ page_size: 100 }),
      ]).then(([acl, users, groups]) => ({ acl, users, groups })),
    [repoName],
    { cacheKey: `repo:acl-editor:${repoName}` },
  );

  const [entries, setEntries] = useState<AclEntry[]>([]);
  const [users, setUsers] = useState<User[]>([]);
  const [groups, setGroups] = useState<UserGroup[]>([]);
  const [saving, setSaving] = useState(false);

  // 新增条目输入：主体类型 + 主体 Select + 权限 Select，点击添加才落入 entries
  const [newSubjectType, setNewSubjectType] = useState<AclSubjectType>("user");
  const [newSubjectId, setNewSubjectId] = useState<string | null>(null);
  const [newAction, setNewAction] = useState<AclAction>("read");
  // 同一主体重复添加时给出提示（不静默丢弃、也不静默覆盖已有条目的权限）。
  const [duplicateHint, setDuplicateHint] = useState(false);

  useEffect(() => {
    if (state.data) {
      setEntries(state.data.acl.items);
      setUsers(state.data.users.items);
      setGroups(state.data.groups.items);
    }
  }, [state.data]);

  // id → 展示名的两份映射；列表中按主体类型取对应那份
  const userNames = useMemo(() => {
    const m = new Map<number, string>();
    for (const u of users) m.set(u.id, u.username);
    return m;
  }, [users]);
  const groupNames = useMemo(() => {
    const m = new Map<number, string>();
    for (const g of groups) m.set(g.id, g.name);
    return m;
  }, [groups]);

  /** 新增下拉的候选主体：按选中类型取用户或用户组；值用字符串 id，提交时转回 number。 */
  const subjectOptions = useMemo(
    () =>
      newSubjectType === "group"
        ? groups.map((g) => ({ value: String(g.id), label: g.name }))
        : users.map((u) => ({ value: String(u.id), label: u.username })),
    [groups, users, newSubjectType],
  );

  // 已占用主体：新增时从下拉里排除，避免同主体重复授权（后端会按唯一约束拒绝保存）
  const usedKeys = useMemo(() => new Set(entries.map(aclSubjectKey)), [entries]);
  const availableSubjectOptions = useMemo(
    () => subjectOptions.filter((o) => !usedKeys.has(`${newSubjectType}:${Number(o.value)}`)),
    [subjectOptions, usedKeys, newSubjectType],
  );

  const actionOptions = ACL_ACTIONS.map((action) => ({
    value: action,
    label: t(ACL_ACTION_LABEL_KEYS[action]),
  }));
  const subjectTypeOptions = [
    { value: "user", label: t("acl.subjectUser") },
    { value: "group", label: t("acl.subjectGroup") },
  ];

  /** 按稳定标识更新条目：类型切换会重写两个 ID 列，保证两列互斥。 */
  const updateEntry = (key: string, patch: Partial<AclEntry>) => {
    setEntries((prev) => prev.map((e) => (aclSubjectKey(e) === key ? { ...e, ...patch } : e)));
  };

  const removeEntry = (key: string) => {
    setEntries((prev) => prev.filter((e) => aclSubjectKey(e) !== key));
  };

  const addEntry = () => {
    if (!newSubjectId) return;
    const entry = buildAclEntry(newSubjectType, Number(newSubjectId), newAction);
    if (usedKeys.has(aclSubjectKey(entry))) {
      setDuplicateHint(true);
      return;
    }
    setDuplicateHint(false);
    // 追加而不是替换：保存是覆盖写，请求体必须始终是完整列表。
    setEntries((prev) => [...prev, entry]);
    setNewSubjectId(null);
    setNewAction("read");
  };

  const handleSave = () => {
    setSaving(true);
    // 整份下发：后端按 PutAclRequest 替换该仓库的全部条目。
    setAcl(repoName, entries)
      .then(() => notifySuccess(t("common.saved")))
      .catch(notifyError)
      .finally(() => setSaving(false));
  };

  const subjectLabel = (entry: AclEntry) =>
    aclSubjectLabel(
      entry,
      { userNames, groupNames },
      {
        user: (id) => t("acl.userIdFallback", { id }),
        group: (id) => t("acl.groupIdFallback", { id }),
      },
    );

  return (
    <Stack gap="md">
      <AsyncBoundary state={state}>
        {() => (
          <>
            {/* 覆盖写提示：整份替换这点不说明，用户会以为「添加」只提交了一条。 */}
            <Text size="xs" c="dimmed">
              {t("acl.overlayHint")}
            </Text>
            {entries.length === 0 ? (
              <EmptyState message={t("acl.empty")} />
            ) : (
              <Table>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t("acl.subject")}</Table.Th>
                    <Table.Th>{t("acl.action")}</Table.Th>
                    <Table.Th>{t("common.actions")}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {entries.map((entry) => {
                    // 稳定标识而不是数组下标：删除/重排后 React 不会复用错行。
                    const key = aclSubjectKey(entry);
                    return (
                      <Table.Tr key={key}>
                        {/* 主体只读：改主体等于换一条授权，删掉重加更明确，
                            也避免「改了 ID 但列表里同主体已有另一条」的重复授权。 */}
                        <Table.Td>
                          <Group gap="xs" wrap="nowrap">
                            <Badge
                              size="sm"
                              variant="light"
                              color={aclSubjectType(entry) === "group" ? "violet" : "blue"}
                            >
                              {aclSubjectType(entry) === "group"
                                ? t("acl.subjectGroup")
                                : t("acl.subjectUser")}
                            </Badge>
                            <Text size="sm" truncate>
                              {subjectLabel(entry)}
                            </Text>
                          </Group>
                        </Table.Td>
                        <Table.Td>
                          <Select
                            w={140}
                            data={actionOptions}
                            allowDeselect={false}
                            value={entry.action}
                            onChange={(v) => v && updateEntry(key, { action: v as AclAction })}
                          />
                        </Table.Td>
                        <Table.Td>
                          <Button
                            size="compact-xs"
                            variant="subtle"
                            color="red"
                            leftSection={<IconTrash size={14} />}
                            aria-label={t("common.delete")}
                            onClick={() => removeEntry(key)}
                          >
                            {t("common.delete")}
                          </Button>
                        </Table.Td>
                      </Table.Tr>
                    );
                  })}
                </Table.Tbody>
              </Table>
            )}

            {/* 新增条目行：选主体类型 + 选主体 + 选权限 + 添加按钮 */}
            <Group align="flex-end" gap="sm">
              <Select
                label={t("acl.subjectType")}
                data={subjectTypeOptions}
                allowDeselect={false}
                value={newSubjectType}
                onChange={(v) => {
                  // 换类型即换候选集：原主体 ID 对新类型无意义，必须清空。
                  setNewSubjectType((v as AclSubjectType) ?? "user");
                  setNewSubjectId(null);
                  setDuplicateHint(false);
                }}
                w={120}
              />
              <Select
                label={t("acl.subject")}
                placeholder={
                  newSubjectType === "group"
                    ? t("acl.subjectGroupPlaceholder")
                    : t("acl.userPlaceholder")
                }
                data={availableSubjectOptions}
                value={newSubjectId}
                onChange={(value) => {
                  setNewSubjectId(value);
                  setDuplicateHint(false);
                }}
                searchable
                w={240}
                nothingFoundMessage={t("common.empty")}
              />
              <Select
                label={t("acl.action")}
                data={actionOptions}
                allowDeselect={false}
                value={newAction}
                onChange={(v) => v && setNewAction(v as AclAction)}
                w={140}
              />
              <Button
                variant="light"
                onClick={addEntry}
                disabled={!newSubjectId}
                leftSection={<IconPlus size={14} />}
              >
                {t("acl.addEntry")}
              </Button>
              <OpsHelpButton
                title={t("acl.actionHintTitle")}
                items={ACL_ACTIONS.map((action) => ({
                  label: t(ACL_ACTION_LABEL_KEYS[action]),
                  value: t(ACL_ACTION_HINT_KEYS[action]),
                }))}
              />
            </Group>
            {duplicateHint ? (
              <Text size="xs" c="red">
                {t("acl.duplicateSubject")}
              </Text>
            ) : null}

            <Group justify="flex-end">
              <Button
                onClick={handleSave}
                loading={saving}
                leftSection={<IconDeviceFloppy size={16} />}
              >
                {t("acl.save")}
              </Button>
            </Group>
          </>
        )}
      </AsyncBoundary>
    </Stack>
  );
}
