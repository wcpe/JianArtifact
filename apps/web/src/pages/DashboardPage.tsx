// 仪表盘：展示实例状态概览（版本 / 就绪 / 初始化 / 用户数 / 迁移版本）。
import { Card, SimpleGrid, Text } from "@mantine/core";
import { PageHeader } from "@jianartifact/ui";
import { useTranslation } from "react-i18next";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { getStatus } from "../api/endpoints";
import { useAsync } from "../hooks/useAsync";
import { density } from "../theme/density";

function StatCard({ label, value }: { label: string; value: string }) {
  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
      <Text size="sm" c="dimmed">
        {label}
      </Text>
      <Text size="xl" fw={700}>
        {value}
      </Text>
    </Card>
  );
}

export function DashboardPage() {
  const { t } = useTranslation();
  const state = useAsync(getStatus, []);

  return (
    <>
      <PageHeader title={t("dashboard.title")} description={t("dashboard.description")} />
      <AsyncBoundary state={state}>
        {(status) => (
          <SimpleGrid cols={{ base: 1, sm: 2, lg: 4 }} spacing={density.gridSpacing}>
            <StatCard label={t("dashboard.version")} value={status.version} />
            <StatCard label={t("dashboard.ready")} value={status.ready ? t("dashboard.yes") : t("dashboard.no")} />
            <StatCard
              label={t("dashboard.initialized")}
              value={status.initialized ? t("dashboard.yes") : t("dashboard.no")}
            />
            <StatCard label={t("dashboard.userCount")} value={String(status.userCount)} />
            <StatCard label={t("dashboard.migration")} value={status.migrationVersion} />
          </SimpleGrid>
        )}
      </AsyncBoundary>
    </>
  );
}
