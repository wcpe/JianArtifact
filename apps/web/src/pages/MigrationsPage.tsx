// 迁移任务列表：状态 / 来源 / 时间 + 新建向导入口（admin）。
import { Badge, Button, Group, Table } from "@mantine/core";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { listMigrations } from "../api/endpoints";
import type { MigrationTask, MigrationTaskStatus } from "../api/types";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { useAsync } from "../hooks/useAsync";

function statusColor(status: MigrationTaskStatus): string {
  switch (status) {
    case "completed":
      return "green";
    case "running":
      return "blue";
    case "failed":
      return "red";
    case "cancelled":
      return "gray";
    default:
      return "yellow";
  }
}

export function MigrationsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const state = useAsync(() => listMigrations({ page_size: 100 }), []);

  return (
    <>
      <PageHeader
        title={t("migrations.title")}
        description={t("migrations.description")}
        actions={
          <Button onClick={() => navigate("/migrations/new")}>{t("migrations.new")}</Button>
        }
      />
      <AsyncBoundary state={state}>
        {(data) =>
          data.items.length === 0 ? (
            <EmptyState message={t("common.empty")} description={t("migrations.emptyHint")} />
          ) : (
            <Table striped highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>ID</Table.Th>
                  <Table.Th>{t("migrations.status")}</Table.Th>
                  <Table.Th>{t("migrations.sourceType")}</Table.Th>
                  <Table.Th>{t("migrations.conflictPolicy")}</Table.Th>
                  <Table.Th>{t("migrations.createdAt")}</Table.Th>
                  <Table.Th>{t("common.actions")}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {data.items.map((task: MigrationTask) => (
                  <Table.Tr key={task.id}>
                    <Table.Td>{task.id}</Table.Td>
                    <Table.Td>
                      <Badge color={statusColor(task.status)} variant="light">
                        {t(`migrations.status_${task.status}`)}
                      </Badge>
                    </Table.Td>
                    <Table.Td>{t(`migrations.source_${task.sourceType}`)}</Table.Td>
                    <Table.Td>{task.conflictPolicy}</Table.Td>
                    <Table.Td>{task.createdAt}</Table.Td>
                    <Table.Td>
                      <Group gap="xs">
                        <Button
                          size="compact-xs"
                          variant="light"
                          onClick={() => navigate(`/migrations/${task.id}`)}
                        >
                          {t("migrations.detail")}
                        </Button>
                      </Group>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )
        }
      </AsyncBoundary>
    </>
  );
}
