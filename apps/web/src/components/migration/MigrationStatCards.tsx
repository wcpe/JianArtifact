// 迁移结果四宫格：复制 / 跳过 / 失败 / 合计。
import { Card, Group, SimpleGrid, Stack, Text, ThemeIcon } from "@mantine/core";
import {
  IconAlertTriangle,
  IconCircleCheck,
  IconPlayerSkipForward,
  IconStack2,
} from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

import { density } from "../../theme/density";
import type { Totals } from "./status";

interface Props {
  totals: Totals;
  /** 计划估算资产数（可选） */
  estimated?: number;
}

export function MigrationStatCards({ totals, estimated }: Props) {
  const { t } = useTranslation();
  const sum = totals.copied + totals.skipped + totals.failed;
  const items = [
    {
      key: "copied",
      label: t("migrations.progressCopied"),
      value: totals.copied,
      color: "green",
      icon: IconCircleCheck,
    },
    {
      key: "skipped",
      label: t("migrations.progressSkipped"),
      value: totals.skipped,
      color: "gray",
      icon: IconPlayerSkipForward,
    },
    {
      key: "failed",
      label: t("migrations.progressFailed"),
      value: totals.failed,
      color: "red",
      icon: IconAlertTriangle,
    },
    {
      key: "total",
      label: t("migrations.statProcessed"),
      value: sum,
      color: "blue",
      icon: IconStack2,
      hint:
        estimated && estimated > 0 ? t("migrations.statEstimated", { n: estimated }) : undefined,
    },
  ] as const;

  return (
    <SimpleGrid cols={{ base: 2, sm: 4 }} spacing={density.gridSpacing}>
      {items.map((item) => {
        const Icon = item.icon;
        return (
          <Card key={item.key} withBorder padding={density.cardPadding} radius="md">
            <Group justify="space-between" align="flex-start" wrap="nowrap">
              <Stack gap={2}>
                <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
                  {item.label}
                </Text>
                <Text fw={700} size="xl" lh={1.2}>
                  {item.value.toLocaleString()}
                </Text>
                {"hint" in item && item.hint ? (
                  <Text size="xs" c="dimmed">
                    {item.hint}
                  </Text>
                ) : null}
              </Stack>
              <ThemeIcon variant="light" color={item.color} size="lg" radius="md">
                <Icon size={18} />
              </ThemeIcon>
            </Group>
          </Card>
        );
      })}
    </SimpleGrid>
  );
}
