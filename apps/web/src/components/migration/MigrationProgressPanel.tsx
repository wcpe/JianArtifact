// 迁移进度条：优先用后端 percent；枚举显示 found/total，搬运显示 copied/skipped/failed。
import { Card, Group, Progress, Stack, Text, ThemeIcon } from "@mantine/core";
import { IconSearch, IconTransfer } from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

import { density } from "../../theme/density";
import type { Totals } from "./status";
import type { MigrationTaskStatus } from "../../api/types";

export interface ProgressMeta {
  phase?: string;
  found?: number;
  processed?: number;
  total?: number;
  percent?: number;
  message?: string;
  currentRepo?: string;
  estimated?: number;
}

interface Props {
  totals: Totals;
  status: MigrationTaskStatus;
  meta?: ProgressMeta;
  animated?: boolean;
}

export function MigrationProgressPanel({ totals, status, meta, animated }: Props) {
  const { t } = useTranslation();
  const phase = meta?.phase ?? "";
  const found = meta?.found ?? 0;
  const processed = meta?.processed ?? 0;
  const total = meta?.total ?? 0;
  const estimated = meta?.estimated ?? 0;
  const message = meta?.message ?? "";
  const currentRepo = meta?.currentRepo ?? "";
  const backendPct = meta?.percent ?? 0;

  const sum = totals.copied + totals.skipped + totals.failed;
  const enumerating = status === "running" && phase === "enumerating";
  const copying = status === "running" && phase === "copying";

  let pct = 0;
  if (status === "completed") {
    pct = 100;
  } else if (backendPct > 0) {
    pct = Math.min(99, backendPct);
  } else if (enumerating) {
    const denom = total > 0 ? total : estimated;
    pct = denom > 0 ? Math.min(50, Math.round((found / denom) * 50)) : found > 0 ? 15 : 5;
  } else if (copying || status === "running") {
    const denom = total > 0 ? total : estimated;
    pct = denom > 0 ? Math.min(99, Math.round((sum / denom) * 100)) : sum > 0 ? 40 : 10;
  }

  const title = enumerating
    ? t("migrations.phaseEnumeratingTitle")
    : copying || status === "running"
      ? t("migrations.phaseCopyingTitle")
      : t("migrations.progressTitle");

  const hint =
    message ||
    (enumerating
      ? t("migrations.phaseEnumeratingProgress", {
          found,
          total: total > 0 ? total : estimated > 0 ? estimated : "?",
        })
      : status === "running"
        ? t("migrations.runningHint")
        : t("migrations.progressNoEstimate"));

  const share = (n: number) => (sum > 0 ? Math.round((n / sum) * 100) : 0);
  const Icon = enumerating ? IconSearch : IconTransfer;

  return (
    <Card withBorder padding={density.cardPadding} radius="md">
      <Stack gap="md">
        <Group justify="space-between" align="flex-start" wrap="nowrap">
          <Group gap="sm" style={{ flex: 1, minWidth: 0 }} wrap="nowrap">
            <ThemeIcon
              size={44}
              radius="md"
              variant="light"
              color={status === "failed" ? "red" : status === "completed" ? "green" : "blue"}
            >
              <Icon size={22} />
            </ThemeIcon>
            <div style={{ minWidth: 0, flex: 1 }}>
              <Text size="sm" fw={700}>
                {title}
              </Text>
              <Text size="sm" c="dimmed" lineClamp={2}>
                {hint}
              </Text>
              {currentRepo && status === "running" && (
                <Text size="xs" c="blue" mt={4} fw={600}>
                  {t("migrations.currentRepo")}: {currentRepo}
                </Text>
              )}
            </div>
          </Group>
          <Text
            fw={800}
            size="2rem"
            lh={1}
            c={status === "failed" ? "red" : status === "completed" ? "green" : "blue"}
          >
            {status === "running" && pct === 0 ? "…" : `${pct}%`}
          </Text>
        </Group>

        <Progress
          value={pct}
          size={18}
          radius="xl"
          animated={animated ?? status === "running"}
          striped={status === "running"}
          color={status === "failed" ? "red" : status === "completed" ? "green" : "blue"}
        />

        {/* 大数字：枚举 / 搬运 */}
        {status === "running" && enumerating && (
          <Group gap="xl" justify="center">
            <StatBig label={t("migrations.foundLabel")} value={found} color="blue" />
            <Text size="xl" c="dimmed" fw={300}>
              /
            </Text>
            <StatBig
              label={t("migrations.estimateLabel")}
              value={total > 0 ? total : estimated}
              color="gray"
            />
          </Group>
        )}

        {(status === "running" && copying) || sum > 0 ? (
          <Stack gap={6}>
            {copying && total > 0 && (
              <Text size="sm" ta="center" c="dimmed">
                {t("migrations.processedOf", {
                  done: processed > 0 ? processed : sum,
                  total,
                })}
              </Text>
            )}
            {sum > 0 && (
              <>
                <Progress.Root size="lg" radius="xl">
                  <Progress.Section value={share(totals.copied)} color="green">
                    <Progress.Label>
                      {share(totals.copied) >= 10 ? t("migrations.progressCopied") : ""}
                    </Progress.Label>
                  </Progress.Section>
                  <Progress.Section value={share(totals.skipped)} color="gray">
                    <Progress.Label>
                      {share(totals.skipped) >= 10 ? t("migrations.progressSkipped") : ""}
                    </Progress.Label>
                  </Progress.Section>
                  <Progress.Section value={share(totals.failed)} color="red">
                    <Progress.Label>
                      {share(totals.failed) >= 10 ? t("migrations.progressFailed") : ""}
                    </Progress.Label>
                  </Progress.Section>
                </Progress.Root>
                <Group gap="lg" justify="center">
                  <Text size="sm">
                    <Text span c="green" fw={700}>
                      {totals.copied}
                    </Text>{" "}
                    {t("migrations.progressCopied")}
                  </Text>
                  <Text size="sm">
                    <Text span c="dimmed" fw={700}>
                      {totals.skipped}
                    </Text>{" "}
                    {t("migrations.progressSkipped")}
                  </Text>
                  <Text size="sm">
                    <Text span c="red" fw={700}>
                      {totals.failed}
                    </Text>{" "}
                    {t("migrations.progressFailed")}
                  </Text>
                </Group>
              </>
            )}
          </Stack>
        ) : null}
      </Stack>
    </Card>
  );
}

function StatBig({ label, value, color }: { label: string; value: number; color: string }) {
  return (
    <Stack gap={0} align="center">
      <Text fw={800} size="1.75rem" lh={1.1} c={color}>
        {value.toLocaleString()}
      </Text>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
        {label}
      </Text>
    </Stack>
  );
}
