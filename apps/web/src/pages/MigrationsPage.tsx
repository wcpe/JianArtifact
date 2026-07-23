// 迁移任务列表：卡片式可视化，服务端分页，状态/来源/规模摘要。
import {
  Alert,
  Badge,
  Button,
  Card,
  Group,
  Pagination,
  Select,
  SimpleGrid,
  Stack,
  Text,
  ThemeIcon,
} from "@mantine/core";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import {
  IconArrowRight,
  IconClock,
  IconFolder,
  IconLink,
  IconPackage,
  IconPlus,
  IconRefresh,
} from "@tabler/icons-react";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { listMigrations } from "../api/endpoints";
import type { MigrationTask } from "../api/types";
import {
  planEstimatedAssets,
  sourceColor,
  statusColor,
} from "../components/migration/status";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { useAsync } from "../hooks/useAsync";
import { density } from "../theme/density";

const PAGE_SIZE_OPTIONS = ["10", "20", "50"] as const;

function SourceIcon({ type }: { type: MigrationTask["sourceType"] }) {
  if (type === "online_rest") {
    return <IconLink size={16} />;
  }
  if (type === "offline_dir") {
    return <IconFolder size={16} />;
  }
  return <IconPackage size={16} />;
}

export function MigrationsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);

  const state = useAsync(
    () => listMigrations({ page, page_size: pageSize }),
    [page, pageSize],
  );

  // 活跃任务横幅：额外拉一页靠前任务（id 降序），覆盖多数近期 running/planned
  const gate = useAsync(() => listMigrations({ page: 1, page_size: 50 }), []);

  const activeHint = useMemo(() => {
    if (gate.loading || !gate.data) {
      return null;
    }
    const items = gate.data.items;
    const running = items.find((x) => x.status === "running");
    const planned = items.find((x) => x.status === "planned");
    if (running) {
      return { kind: "running" as const, task: running };
    }
    if (planned) {
      return { kind: "planned" as const, task: planned };
    }
    return null;
  }, [gate.loading, gate.data]);

  const total = state.data?.total ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / pageSize) || 1);

  // 页码越界（删任务后 total 变小）时回退
  useEffect(() => {
    if (state.data && page > totalPages) {
      setPage(totalPages);
    }
  }, [state.data, page, totalPages]);

  const pageSummary = useMemo(() => {
    if (!state.data) {
      return { running: 0, planned: 0, completed: 0, failed: 0, cancelled: 0 };
    }
    const items = state.data.items;
    return {
      running: items.filter((x) => x.status === "running").length,
      planned: items.filter((x) => x.status === "planned").length,
      completed: items.filter((x) => x.status === "completed").length,
      failed: items.filter((x) => x.status === "failed").length,
      cancelled: items.filter((x) => x.status === "cancelled").length,
    };
  }, [state.data]);

  const onNew = () => {
    if (activeHint?.kind === "running") {
      navigate(`/migrations/${activeHint.task.id}`);
      return;
    }
    navigate("/migrations/new");
  };

  const changePageSize = (v: string | null) => {
    const n = Number(v) || 20;
    setPageSize(n);
    setPage(1);
  };

  return (
    <Stack gap="md">
      <PageHeader
        title={t("migrations.title")}
        description={t("migrations.description")}
        actions={
          <Group gap="xs">
            <Button
              variant="default"
              leftSection={<IconRefresh size={16} />}
              onClick={() => {
                state.reload();
                gate.reload();
              }}
              loading={state.loading || gate.loading}
            >
              {t("common.refresh")}
            </Button>
            <Button
              leftSection={
                activeHint?.kind === "running" ? (
                  <IconArrowRight size={16} />
                ) : (
                  <IconPlus size={16} />
                )
              }
              onClick={onNew}
            >
              {activeHint?.kind === "running"
                ? t("migrations.gotoDetail")
                : t("migrations.new")}
            </Button>
          </Group>
        }
      />

      {state.data && total > 0 && (
        <SimpleGrid cols={{ base: 2, sm: 5 }} spacing={density.gridSpacing}>
          {(
            [
              { label: t("migrations.summaryTotal"), value: total, color: "blue" },
              {
                label: t("migrations.status_running"),
                value: pageSummary.running,
                color: "cyan",
                pageOnly: true,
              },
              {
                label: t("migrations.status_planned"),
                value: pageSummary.planned,
                color: "yellow",
                pageOnly: true,
              },
              {
                label: t("migrations.status_completed"),
                value: pageSummary.completed,
                color: "green",
                pageOnly: true,
              },
              {
                label: t("migrations.status_failed"),
                value: pageSummary.failed,
                color: "red",
                pageOnly: true,
              },
            ] as const
          ).map((s) => (
            <Card key={s.label} withBorder padding="sm" radius="md">
              <Text size="xs" c="dimmed" fw={600}>
                {s.label}
                {"pageOnly" in s && s.pageOnly ? (
                  <Text span size="xs" c="dimmed" fw={400}>
                    {" "}
                    ({t("migrations.summaryPageOnly")})
                  </Text>
                ) : null}
              </Text>
              <Text fw={700} size="xl" c={s.color}>
                {s.value}
              </Text>
            </Card>
          ))}
        </SimpleGrid>
      )}

      {activeHint && (
        <Alert
          color={activeHint.kind === "running" ? "blue" : "orange"}
          title={t("migrations.hasActiveBanner")}
        >
          <Group justify="space-between" align="center" wrap="wrap">
            <Text size="sm">
              #{activeHint.task.id} · {t(`migrations.status_${activeHint.task.status}`)} ·{" "}
              {t(`migrations.source_${activeHint.task.sourceType}`)}
            </Text>
            <Button size="xs" onClick={() => navigate(`/migrations/${activeHint.task.id}`)}>
              {t("migrations.gotoDetail")}
            </Button>
          </Group>
        </Alert>
      )}

      <AsyncBoundary state={state}>
        {(data) =>
          data.items.length === 0 ? (
            <EmptyState message={t("common.empty")} description={t("migrations.emptyHint")} />
          ) : (
            <Stack gap="sm">
              <Group justify="space-between" wrap="wrap" gap="sm">
                <Text size="sm" c="dimmed">
                  {t("migrations.pageInfo", {
                    from: (page - 1) * pageSize + 1,
                    to: Math.min(page * pageSize, total),
                    total,
                  })}
                </Text>
                <Group gap="xs">
                  <Text size="sm" c="dimmed">
                    {t("migrations.pageSize")}
                  </Text>
                  <Select
                    size="xs"
                    w={80}
                    allowDeselect={false}
                    data={[...PAGE_SIZE_OPTIONS]}
                    value={String(pageSize)}
                    onChange={changePageSize}
                  />
                </Group>
              </Group>
              {data.items.map((task) => (
                <TaskCard
                  key={task.id}
                  task={task}
                  onOpen={() => navigate(`/migrations/${task.id}`)}
                />
              ))}
              {totalPages > 1 && (
                <Group justify="center" mt="xs">
                  <Pagination
                    value={page}
                    onChange={setPage}
                    total={totalPages}
                    siblings={1}
                    boundaries={1}
                  />
                </Group>
              )}
            </Stack>
          )
        }
      </AsyncBoundary>
    </Stack>
  );
}

function TaskCard({ task, onOpen }: { task: MigrationTask; onOpen: () => void }) {
  const { t } = useTranslation();
  const repos = task.plan?.repositories ?? [];
  const est = planEstimatedAssets(repos);
  const cfg = task.sourceConfig as Record<string, unknown> | undefined;
  const target =
    (typeof cfg?.url === "string" && cfg.url) ||
    (typeof cfg?.path === "string" && cfg.path) ||
    "—";

  return (
    <Card
      withBorder
      padding={density.cardPadding}
      radius="md"
      style={{ cursor: "pointer" }}
      onClick={onOpen}
    >
      <Group justify="space-between" align="flex-start" wrap="wrap" gap="md">
        <Group align="flex-start" gap="md" wrap="nowrap" style={{ flex: 1, minWidth: 0 }}>
          <ThemeIcon
            size={44}
            radius="md"
            variant="light"
            color={sourceColor(task.sourceType)}
          >
            <SourceIcon type={task.sourceType} />
          </ThemeIcon>
          <Stack gap={4} style={{ minWidth: 0, flex: 1 }}>
            <Group gap="xs">
              <Text fw={700}>#{task.id}</Text>
              <Badge color={statusColor(task.status)} variant="light" tt="none">
                {t(`migrations.status_${task.status}`)}
              </Badge>
              <Badge color={sourceColor(task.sourceType)} variant="outline" tt="none">
                {t(`migrations.source_${task.sourceType}`)}
              </Badge>
            </Group>
            <Text size="sm" c="dimmed" lineClamp={1} ff="monospace">
              {target}
            </Text>
            <Group gap="md">
              <Text size="xs" c="dimmed">
                {t("migrations.planRepoCount", { n: repos.length })}
              </Text>
              {est > 0 && (
                <Text size="xs" c="dimmed">
                  {t("migrations.statEstimated", { n: est })}
                </Text>
              )}
              <Group gap={4}>
                <IconClock size={12} />
                <Text size="xs" c="dimmed">
                  {task.createdAt}
                </Text>
              </Group>
            </Group>
          </Stack>
        </Group>
        <Button
          variant="light"
          size="sm"
          rightSection={<IconArrowRight size={14} />}
          onClick={(e) => {
            e.stopPropagation();
            onOpen();
          }}
        >
          {t("migrations.detail")}
        </Button>
      </Group>
    </Card>
  );
}
