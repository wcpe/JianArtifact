// 仓库状态面板（v0.8.0）：替代原「上游自动阻止」面板。
// - 左：DonutChart 展示各连接状态的仓库数量分布（可用/自动阻止/不可用/离线/未连接）；
// - 右：全部仓库的状态明细（状态点 + 仓库名 + 类型 + 状态徽章），点击跳仓库详情；
// - 状态推导：offline 优先 OFFLINE；hosted 在线视为可用；proxy/group 取 connectionStatus（缺省 READY 未连接）。
import { DonutChart } from "@mantine/charts";
import {
  Badge,
  Box,
  Card,
  Group,
  Skeleton,
  Stack,
  Text,
  ThemeIcon,
  UnstyledButton,
} from "@mantine/core";
import { IconBox, IconPinned } from "@tabler/icons-react";
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

  // 置顶的仓库排最前，其余保持原顺序。
  const orderedRepos = useMemo(() => sortPinnedFirst(repos), [repos, sortPinnedFirst]);

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
      <Group justify="space-between" mb="xs" wrap="wrap">
        <Group gap="xs">
          <ThemeIcon variant="light" color="indigo" size="sm" radius="md">
            <IconBox size={14} />
          </ThemeIcon>
          <Text fw={700}>{t("dashboard.repoStatusTitle")}</Text>
        </Group>
        <Badge color="indigo" variant="light" size="lg">
          {t("dashboard.repoStatusTotal", { count: repos.length })}
        </Badge>
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
          {/* 状态分布环形图 */}
          <Box
            visibleFrom="sm"
            style={{ display: "flex", justifyContent: "center", flexShrink: 0 }}
          >
            {donutData.length > 0 ? (
              <DonutChart data={donutData} size={112} thickness={18} withTooltip />
            ) : null}
          </Box>

          {/* 状态明细：单列行（名称完整展示）= 状态点 + 仓库名（置顶带图钉） + 类型 + 状态徽章 */}
          <Box style={{ flex: 1, minWidth: 0, overflowY: "auto" }}>
            <Stack gap={6}>
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
                          <IconPinned size={12} color="var(--mantine-color-amber-6)" aria-hidden />
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
