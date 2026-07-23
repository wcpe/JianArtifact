// 迁移任务详情：轮询进度、取消/续传、报告与 cutover 清单。
import { Alert, Badge, Button, Code, Group, List, Stack, Text } from "@mantine/core";
import { PageHeader } from "@jianartifact/ui";
import { useCallback, useEffect, useState } from "react";
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
import { confirmDanger, notifyError, notifySuccess } from "../lib/feedback";

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

  // running 时 2s 轮询
  useEffect(() => {
    if (task?.status !== "running") {
      return;
    }
    const timer = window.setInterval(reload, 2000);
    return () => window.clearInterval(timer);
  }, [task?.status, reload]);

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
    <>
      <PageHeader
        title={`${t("migrations.detail")} #${task.id}`}
        description={`${t(`migrations.source_${task.sourceType}`)} · ${task.conflictPolicy}`}
        actions={
          <Button component={Link} to="/migrations" variant="default">
            {t("migrations.backList")}
          </Button>
        }
      />
      <Stack>
        <Group>
          <Badge size="lg">{t(`migrations.status_${task.status}`)}</Badge>
          {task.errorMessage && (
            <Text c="red" size="sm">
              {task.errorMessage}
            </Text>
          )}
        </Group>

        <Group>
          {task.status === "planned" && (
            <Button loading={busy} onClick={() => act(() => startMigration(task.id), t("migrations.started"))}>
              {t("migrations.start")}
            </Button>
          )}
          {(task.status === "failed" || task.status === "cancelled") && (
            <Button
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
              loading={busy}
              onClick={() => {
                confirmDanger({
                  title: t("migrations.cancel"),
                  message: t("migrations.cancelConfirm"),
                  confirmLabel: t("common.confirm"),
                  cancelLabel: t("common.cancel"),
                  onConfirm: () => act(() => cancelMigration(task.id), t("migrations.cancelled")),
                });
              }}
            >
              {t("migrations.cancel")}
            </Button>
          )}
          {task.status === "completed" && (
            <Button
              loading={busy}
              onClick={() => act(() => finalizeMigration(task.id), t("migrations.finalized"))}
            >
              {t("migrations.finalize")}
            </Button>
          )}
        </Group>

        {report && (
          <Alert title={t("migrations.report")} color="gray">
            <Text size="sm">
              copied={String(report.totals?.copied ?? 0)} · skipped=
              {String(report.totals?.skipped ?? 0)} · failed={String(report.totals?.failed ?? 0)}
            </Text>
            {report.cutover && typeof report.cutover === "object" && "checklist" in report.cutover && (
              <>
                <Text fw={600} mt="sm">
                  {t("migrations.cutover")}
                </Text>
                <List size="sm">
                  {(report.cutover as { checklist?: string[] }).checklist?.map((item) => (
                    <List.Item key={item}>{item}</List.Item>
                  ))}
                </List>
              </>
            )}
          </Alert>
        )}

        {task.plan && (
          <Code block>{JSON.stringify(task.plan, null, 2)}</Code>
        )}
      </Stack>
    </>
  );
}
