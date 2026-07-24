// 来源与冲突策略的结构化卡片（不展示 JSON）。
import { Badge, Card, Group, SimpleGrid, Stack, Text, ThemeIcon } from "@mantine/core";
import { IconFolder, IconLink, IconPackage, IconShieldLock } from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

import type { MigrationConflictPolicy, MigrationSourceType } from "../../api/types";
import { density } from "../../theme/density";
import { sourceColor, sourceConfigSummary } from "./status";

interface Props {
  sourceType: MigrationSourceType;
  conflictPolicy: MigrationConflictPolicy;
  sourceConfig?: Record<string, unknown> | null;
  credentialRef?: string | null;
}

export function MigrationSourceCard({
  sourceType,
  conflictPolicy,
  sourceConfig,
  credentialRef,
}: Props) {
  const { t } = useTranslation();
  const summary = sourceConfigSummary(sourceConfig ?? undefined);
  const Icon =
    sourceType === "online_rest"
      ? IconLink
      : sourceType === "offline_dir"
        ? IconFolder
        : IconPackage;

  return (
    <Card withBorder padding={density.cardPadding} radius="md">
      <Stack gap="md">
        <Group justify="space-between">
          <Group gap="sm">
            <ThemeIcon variant="light" color={sourceColor(sourceType)} size="lg" radius="md">
              <Icon size={18} />
            </ThemeIcon>
            <div>
              <Text size="xs" c="dimmed" fw={600}>
                {t("migrations.sourceType")}
              </Text>
              <Text fw={600}>{t(`migrations.source_${sourceType}`)}</Text>
            </div>
          </Group>
          <Badge variant="light" color="gray">
            {t("migrations.conflictPolicy")}: {conflictPolicy}
          </Badge>
        </Group>

        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
          {summary.url && <Field label={t("migrations.url")} value={summary.url} mono />}
          {summary.path && <Field label={t("migrations.path")} value={summary.path} mono />}
          {credentialRef && (
            <Group gap="xs" align="flex-start">
              <ThemeIcon size="sm" variant="light" color="orange">
                <IconShieldLock size={12} />
              </ThemeIcon>
              <div>
                <Text size="xs" c="dimmed">
                  {t("migrations.credentialRef")}
                </Text>
                <Text size="sm" fw={500}>
                  {credentialRef}
                </Text>
              </div>
            </Group>
          )}
          {summary.includeRepositories && summary.includeRepositories.length > 0 && (
            <div style={{ gridColumn: "1 / -1" }}>
              <Text size="xs" c="dimmed" mb={4}>
                {t("migrations.includeRepos")}
              </Text>
              <Group gap={6}>
                {summary.includeRepositories.map((name) => (
                  <Badge key={name} variant="outline" size="sm">
                    {name}
                  </Badge>
                ))}
              </Group>
            </div>
          )}
        </SimpleGrid>
      </Stack>
    </Card>
  );
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Text
        size="sm"
        fw={500}
        ff={mono ? "monospace" : undefined}
        style={{ wordBreak: "break-all" }}
      >
        {value}
      </Text>
    </div>
  );
}
