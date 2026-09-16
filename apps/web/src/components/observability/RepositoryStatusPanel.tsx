// 仓库状态面板（v0.8.0）：替代原「上游自动阻止」面板。
// - 左：DonutChart 展示各连接状态的仓库数量分布（可用/自动阻止/不可用/离线/未连接）；
// - 右：全部仓库的状态明细（状态点 + 仓库名 + 类型 + 状态徽章），点击跳仓库详情；
// - 状态推导：offline 优先 OFFLINE；hosted 在线视为可用；proxy/group 取 connectionStatus（缺省 READY 未连接）。
import { DonutChart } from "@mantine/charts";
import {
  Badge,
  Box,
  Button,
  Card,
  Group,
  Skeleton,
  Stack,
  Text,
  ThemeIcon,
  UnstyledButton,
} from "@mantine/core";
import { useMediaQuery } from "@mantine/hooks";
import { IconBox, IconEye, IconPinned } from "@tabler/icons-react";
import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import type { ConnectionStatusValue, Repository } from "../../api/types";
import { usePinnedRepos } from "../../hooks/usePinnedRepos";
import { CONN_COLOR, CONN_LABEL_KEY } from "../../lib/connectionStatus";
import { density } from "../../theme/density";

const STATUS_ORDER: ConnectionStatusValue[] = [
  "AVAILABLE",
  "AUTO_BLOCKED",
  "UNAVAILABLE",
  "OFFLINE",
  "READY",
];

// 明细区最多露出 8 行，超出走内部滚动 + 「查看全部」出口。
// 明细是逐行列出全部仓库的，档位放大后（×16 可达上百个）若不限高，左栏会被撑到几千像素，
// 整页再也读不动；高度按「8 行 + 行间距」算出，与列表行高（padding 5+5 + size=sm 行高 + 1px 边框）对齐，
// 避免阈值与可视行数各写一套而漂移。
const LIST_VISIBLE_ROWS = 8;
const LIST_ROW_HEIGHT = 34;
const LIST_ROW_GAP = 6;
const LIST_MAX_HEIGHT =
  LIST_VISIBLE_ROWS * LIST_ROW_HEIGHT + (LIST_VISIBLE_ROWS - 1) * LIST_ROW_GAP;

/** 面板用的仓库状态：offline 优先；hosted 在线即可用；proxy/group 取 connectionStatus。 */
export function repoStatusOf(repo: Repository): ConnectionStatusValue {
  if (repo.online === false) return "OFFLINE";
  if (repo.type === "hosted") return "AVAILABLE";
  return repo.connectionStatus?.status ?? "READY";
}

function StatusDot({ color }: { color: string }) {
  return (
    <Box
      w={8}
      h={8}
      bg={`var(--mantine-color-${color}-6)`}
      style={{ borderRadius: "50%", flexShrink: 0 }}
    />
  );
}

export function RepositoryStatusPanel({
  repos,
  loading,
}: {
  repos: Repository[];
  loading: boolean;
}) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { isPinned, sortPinnedFirst } = usePinnedRepos();
  // 与 `visibleFrom="sm"` 同一断点：只有真正可见时才挂载环形图。
  const isWide = useMediaQuery("(min-width: 48em)") ?? false;

  // 置顶的仓库排最前，其余保持原顺序。
  const orderedRepos = useMemo(() => sortPinnedFirst(repos), [repos, sortPinnedFirst]);
  // 明细超出可视行数：限高滚动 + 页眉给出「查看全部」出口。
  const hasOverflow = orderedRepos.length > LIST_VISIBLE_ROWS;

  const counts = useMemo(() => {
    const map = new Map<ConnectionStatusValue, number>();
    orderedRepos.forEach((repo) => {
      const status = repoStatusOf(repo);
      map.set(status, (map.get(status) ?? 0) + 1);
    });
    return map;
  }, [orderedRepos]);

  const donutData = useMemo(
    () =>
      STATUS_ORDER.filter((status) => (counts.get(status) ?? 0) > 0).map((status) => ({
        name: t(CONN_LABEL_KEY[status]),
        value: counts.get(status) ?? 0,
        color: CONN_COLOR[status],
      })),
    [counts, t],
  );

  return (
    <Card
      withBorder
      radius="md"
      padding={density.cardPadding}
      h="100%"
      style={{ display: "flex", flexDirection: "column" }}
    >
      <Group justify="space-between" mb="xs" wrap="wrap" gap="xs">
        <Group gap="xs">
          <ThemeIcon variant="light" color="indigo" size="sm" radius="md">
            <IconBox size={14} />
          </ThemeIcon>
          <Text fw={700}>{t("dashboard.repoStatusTitle")}</Text>
        </Group>
        <Group gap="xs" wrap="nowrap">
          <Badge color="indigo" variant="light" size="lg">
            {t("dashboard.repoStatusTotal", { count: repos.length })}
          </Badge>
          {/* 明细被限高裁掉时的出口：去仓库列表看全量（那里有筛选与分页）。 */}
          {hasOverflow ? (
            <Button
              variant="subtle"
              size="xs"
              onClick={() => navigate("/repositories")}
              aria-label={t("dashboard.repoStatusViewAll")}
              leftSection={<IconEye size={14} />}
            >
              {t("dashboard.repoStatusViewAll")}
            </Button>
          ) : null}
        </Group>
      </Group>

      {loading && repos.length === 0 ? (
        <Group align="center" gap="lg" style={{ flex: 1 }}>
          <Skeleton height={140} width={140} radius="md" />
          <Stack gap="xs" style={{ flex: 1 }}>
            {Array.from({ length: 4 }).map((_, index) => (
              <Skeleton key={index} height={36} radius="md" />
            ))}
          </Stack>
        </Group>
      ) : (
        <Group align="center" gap="lg" wrap="nowrap" style={{ flex: 1, minHeight: 0 }}>
          {/* 状态分布环形图。窄屏（< sm）本就被 `visibleFrom` 隐藏，这里连图都不挂载：
              recharts 的 ResponsiveContainer 在 display:none 容器里会量到 0×0 并刷警告，
              手机上也白跑一次图表渲染。 */}
          <Box
            visibleFrom="sm"
            style={{ display: "flex", justifyContent: "center", flexShrink: 0 }}
          >
            {isWide && donutData.length > 0 ? (
              <DonutChart data={donutData} size={112} thickness={18} withTooltip />
            ) : null}
          </Box>

          {/* 状态明细：单列行（名称完整展示）= 状态点 + 仓库名（置顶带图钉） + 类型 + 状态徽章。
              明细是逐行列出全部仓库的，这里限高滚动，仓库再多也不会把左栏拉长。 */}
          <Box
            data-testid="repo-status-list"
            style={{
              flex: 1,
              minWidth: 0,
              overflowY: "auto",
              maxHeight: LIST_MAX_HEIGHT,
              // 常驻滚动条槽位：有/无滚动条时行宽一致，徽章不会左右跳。
              scrollbarGutter: "stable",
            }}
            pr={4}
          >
            <Stack gap={LIST_ROW_GAP}>
              {orderedRepos.map((repo) => {
                const status = repoStatusOf(repo);
                const color = CONN_COLOR[status];
                const pinned = isPinned(repo.name);
                return (
                  <UnstyledButton
                    key={repo.name}
                    onClick={() => navigate(`/repositories/${repo.name}`)}
                    aria-label={repo.name}
                    style={{
                      display: "block",
                      padding: "5px 8px",
                      borderRadius: "var(--mantine-radius-md)",
                      border: "1px solid var(--mantine-color-default-border)",
                    }}
                  >
                    <Group justify="space-between" wrap="nowrap" gap="xs">
                      <Group gap={8} wrap="nowrap" style={{ minWidth: 0, flex: 1 }}>
                        <StatusDot color={color} />
                        <Text size="sm" fw={600} truncate style={{ minWidth: 0 }}>
                          {repo.name}
                        </Text>
                        {pinned ? (
                          <IconPinned size={12} color="var(--mantine-color-yellow-6)" aria-hidden />
                        ) : null}
                      </Group>
                      <Group gap={6} wrap="nowrap">
                        <Badge size="xs" variant="default">
                          {t(`dashboard.repoType_${repo.type}`)}
                        </Badge>
                        <Badge size="xs" variant="light" color={color}>
                          {t(CONN_LABEL_KEY[status])}
                        </Badge>
                      </Group>
                    </Group>
                  </UnstyledButton>
                );
              })}
            </Stack>
          </Box>
        </Group>
      )}
    </Card>
  );
}
