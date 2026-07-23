// 迁移任务详情：可视化进度 / 计划 / 报告 / 切换清单，不展示 raw JSON。
import {
  Alert,
  Badge,
  Button,
  Card,
  Divider,
  Group,
  List,
  SimpleGrid,
  Stack,
  Text,
  ThemeIcon,
  Title,
} from "@mantine/core";
import { PageHeader } from "@jianartifact/ui";
import {
  IconAlertTriangle,
  IconPlayerPlay,
  IconPlayerStop,
  IconRefresh,
  IconRocket,
} from "@tabler/icons-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useParams } from "react-router-dom";

import {
  cancelMigration,
  finalizeMigration,
  getMigration,
  getMigrationReport,
  resumeMigration,
  startMigration,
} from "../api/endpoints";
import type { MigrationReport, MigrationTask } from "../api/types";
import { MigrationLifecycle } from "../components/migration/MigrationLifecycle";
import { MigrationProgressPanel } from "../components/migration/MigrationProgressPanel";
import { MigrationRepoTable } from "../components/migration/MigrationRepoTable";
import { MigrationSourceCard } from "../components/migration/MigrationSourceCard";
import { MigrationStatCards } from "../components/migration/MigrationStatCards";
import {
  parseTotals,
  planEstimatedAssets,
  statusColor,
} from "../components/migration/status";
import { confirmDanger, notifyError, notifySuccess } from "../lib/feedback";
import { density } from "../theme/density";

export function MigrationDetailPage() {
  const { t } = useTranslation();
  const { id } = useParams();
  const taskId = Number(id);
  const [task, setTask] = useState<MigrationTask | null>(null);
  const [report, setReport] = useState<MigrationReport | null>(null);
  const [busy, setBusy] = useState(false);

  const reload = useCallback(() => {
    if (!Number.isFinite(taskId) || taskId <= 0) {
      return;
    }
    getMigration(taskId)
      .then(setTask)
      .catch((e: Error) => notifyError(e.message));
    getMigrationReport(taskId)
      .then(setReport)
      .catch(() => setReport(null));
  }, [taskId]);

  useEffect(() => {
    reload();
  }, [reload]);

  // running 时 1s 轮询，及时显示 found/copied 进度
  useEffect(() => {
    if (task?.status !== "running") {
      return;
    }
    const timer = window.setInterval(reload, 1000);
    return () => window.clearInterval(timer);
  }, [task?.status, reload]);

  const totals = useMemo(
    () => parseTotals((report?.totals ?? {}) as Record<string, unknown>),
    [report],
  );
  const progressMeta = useMemo(() => {
    const raw = (report?.totals ?? {}) as Record<string, unknown>;
    const num = (k: string) => {
      const v = raw[k];
      return typeof v === "number" ? v : Number(v) || 0;
    };
    return {
      phase: typeof raw.phase === "string" ? raw.phase : "",
      found: num("found"),
      processed: num("processed"),
      total: num("total"),
      percent: num("percent"),
      message: typeof raw.message === "string" ? raw.message : "",
      currentRepo: typeof raw.currentRepo === "string" ? raw.currentRepo : "",
      estimated: planEstimatedAssets(task?.plan?.repositories),
    };
  }, [report, task?.plan?.repositories]);
  const estimated = progressMeta.estimated;
  const failures = useMemo(() => {
    const list = report?.failures;
    if (!Array.isArray(list)) {
      return [] as { repo?: string; path?: string; error?: string }[];
    }
    return list.map((f) => {
      const m = f as Record<string, unknown>;
      return {
        repo: typeof m.repo === "string" ? m.repo : undefined,
        path: typeof m.path === "string" ? m.path : undefined,
        error: typeof m.error === "string" ? m.error : undefined,
      };
    });
  }, [report]);
  const cutoverItems = useMemo(() => {
    const c = report?.cutover;
    if (c && typeof c === "object" && "checklist" in c) {
      const list = (c as { checklist?: unknown }).checklist;
      if (Array.isArray(list)) {
        return list.filter((x): x is string => typeof x === "string");
      }
    }
    return [] as string[];
  }, [report]);
  const warnings = task?.plan?.warnings ?? [];
  const repos = task?.plan?.repositories ?? [];

  if (!Number.isFinite(taskId) || taskId <= 0) {
    return <Text c="red">{t("common.error")}</Text>;
  }

  if (!task) {
    return <Text>{t("common.loading")}</Text>;
  }

  const act = (fn: () => Promise<unknown>, okMsg: string) => {
    setBusy(true);
    fn()
      .then(() => {
        notifySuccess(okMsg);
        reload();
      })
      .catch((e: Error) => notifyError(e.message || t("common.error")))
      .finally(() => setBusy(false));
  };

  return (
    <Stack gap="md">
      <PageHeader
        title={`${t("migrations.detail")} #${task.id}`}
        description={t("migrations.detailDesc")}
        actions={
          <Group gap="xs">
            <Button
              variant="default"
              leftSection={<IconRefresh size={16} />}
              onClick={reload}
              disabled={busy}
            >
              {t("common.retry")}
            </Button>
            <Button component={Link} to="/migrations" variant="default">
              {t("migrations.backList")}
            </Button>
          </Group>
        }
      />

      {/* 状态条 + 操作 */}
      <Card withBorder padding={density.cardPadding} radius="md">
        <Group justify="space-between" align="flex-start" wrap="wrap">
          <Group gap="sm">
            <Badge size="xl" variant="light" color={statusColor(task.status)} tt="none">
              {t(`migrations.status_${task.status}`)}
            </Badge>
            <div>
              <Text size="sm" c="dimmed">
                {t(`migrations.source_${task.sourceType}`)} · {task.conflictPolicy}
              </Text>
              <Text size="xs" c="dimmed">
                {t("migrations.createdAt")}: {task.createdAt}
                {task.startedAt ? ` · ${t("migrations.startedAt")}: ${task.startedAt}` : ""}
                {task.finishedAt ? ` · ${t("migrations.finishedAt")}: ${task.finishedAt}` : ""}
              </Text>
            </div>
          </Group>
          <Group gap="xs">
            {task.status === "planned" && (
              <Button
                leftSection={<IconPlayerPlay size={16} />}
                loading={busy}
                onClick={() => act(() => startMigration(task.id), t("migrations.started"))}
              >
                {t("migrations.start")}
              </Button>
            )}
            {(task.status === "failed" || task.status === "cancelled") && (
              <Button
                leftSection={<IconRocket size={16} />}
                loading={busy}
                onClick={() => act(() => resumeMigration(task.id), t("migrations.resumed"))}
              >
                {t("migrations.resume")}
              </Button>
            )}
            {(task.status === "planned" || task.status === "running") && (
              <Button
                color="red"
                variant="light"
                leftSection={<IconPlayerStop size={16} />}
                loading={busy}
                onClick={() => {
                  confirmDanger({
                    title: t("migrations.cancel"),
                    message: t("migrations.cancelConfirm"),
                    confirmLabel: t("common.confirm"),
                    cancelLabel: t("common.cancel"),
                    onConfirm: () =>
                      act(() => cancelMigration(task.id), t("migrations.cancelled")),
                  });
                }}
              >
                {t("migrations.cancel")}
              </Button>
            )}
            {task.status === "completed" && (
              <Button
                leftSection={<IconRocket size={16} />}
                loading={busy}
                onClick={() => {
                  confirmDanger({
                    title: t("migrations.finalize"),
                    message: t("migrations.finalizeConfirm"),
                    confirmLabel: t("common.confirm"),
                    cancelLabel: t("common.cancel"),
                    onConfirm: () =>
                      act(() => finalizeMigration(task.id), t("migrations.finalized")),
                  });
                }}
              >
                {t("migrations.finalize")}
              </Button>
            )}
          </Group>
        </Group>
        {task.errorMessage && (
          <Alert
            mt="md"
            color="red"
            icon={<IconAlertTriangle size={16} />}
            title={t("migrations.errorTitle")}
          >
            {task.errorMessage}
          </Alert>
        )}
        {task.status === "planned" && (
          <Alert mt="md" color="yellow" title={t("migrations.plannedHint")}>
            {t("migrations.explicitStartHint")}
          </Alert>
        )}
        {task.status === "completed" &&
          totals.copied === 0 &&
          totals.skipped > 0 &&
          totals.failed === 0 && (
            <Alert mt="md" color="teal" title={t("migrations.allSkippedTitle")}>
              {t("migrations.allSkippedHint", { n: totals.skipped })}
            </Alert>
          )}
        {task.status === "completed" && (
          <Alert mt="md" color="gray" title={t("migrations.finalizeWhat")}>
            {t("migrations.finalizeExplain")}
          </Alert>
        )}
      </Card>

      {/* 统计 + 进度 */}
      <MigrationStatCards totals={totals} estimated={estimated || undefined} />
      {(task.status === "running" ||
        task.status === "completed" ||
        task.status === "failed" ||
        totals.copied + totals.skipped + totals.failed > 0) && (
        <MigrationProgressPanel totals={totals} status={task.status} meta={progressMeta} />
      )}

      <SimpleGrid cols={{ base: 1, md: 2 }} spacing={density.gridSpacing}>
        <Stack gap="md">
          <Title order={5}>{t("migrations.sectionSource")}</Title>
          <MigrationSourceCard
            sourceType={task.sourceType}
            conflictPolicy={task.conflictPolicy}
            sourceConfig={task.sourceConfig as Record<string, unknown> | undefined}
            credentialRef={task.credentialRef}
          />
          <Title order={5}>{t("migrations.sectionLifecycle")}</Title>
          <Card withBorder padding={density.cardPadding} radius="md">
            <MigrationLifecycle
              status={task.status}
              createdAt={task.createdAt}
              startedAt={task.startedAt}
              finishedAt={task.finishedAt}
              errorMessage={task.errorMessage}
            />
          </Card>
        </Stack>

        <Stack gap="md">
          <Group justify="space-between">
            <Title order={5}>{t("migrations.sectionPlan")}</Title>
            <Badge variant="light" color="blue">
              {t("migrations.planRepoCount", { n: repos.length })}
            </Badge>
          </Group>
          <Card withBorder padding={density.cardPadding} radius="md">
            {warnings.length > 0 && (
              <Alert color="yellow" title={t("migrations.warnings")} mb="sm">
                <List size="sm" spacing={4}>
                  {warnings.map((w) => (
                    <List.Item key={w}>{w}</List.Item>
                  ))}
                </List>
              </Alert>
            )}
            <MigrationRepoTable repositories={repos} maxHeight={420} />
          </Card>
        </Stack>
      </SimpleGrid>

      {/* 失败明细 */}
      {failures.length > 0 && (
        <Card withBorder padding={density.cardPadding} radius="md">
          <Group gap="xs" mb="sm">
            <ThemeIcon color="red" variant="light" size="sm">
              <IconAlertTriangle size={14} />
            </ThemeIcon>
            <Title order={5}>{t("migrations.failuresTitle")}</Title>
            <Badge color="red" variant="light">
              {failures.length}
            </Badge>
          </Group>
          <Stack gap="xs">
            {failures.slice(0, 50).map((f, i) => (
              <Card key={`${f.repo}-${f.path}-${i}`} withBorder padding="sm" radius="sm" bg="var(--mantine-color-red-0)">
                <Text size="sm" fw={600}>
                  {f.repo || "—"}
                  {f.path ? ` / ${f.path}` : ""}
                </Text>
                <Text size="xs" c="red">
                  {f.error || t("common.error")}
                </Text>
              </Card>
            ))}
          </Stack>
        </Card>
      )}

      {/* 切换清单 */}
      {cutoverItems.length > 0 && (
        <Card withBorder padding={density.cardPadding} radius="md">
          <Title order={5} mb="sm">
            {t("migrations.cutover")}
          </Title>
          <Text size="sm" c="dimmed" mb="sm">
            {t("migrations.cutoverHint")}
          </Text>
          <Divider mb="sm" />
          <List size="sm" spacing="xs" type="ordered">
            {cutoverItems.map((item) => (
              <List.Item key={item}>{item}</List.Item>
            ))}
          </List>
        </Card>
      )}
    </Stack>
  );
}
