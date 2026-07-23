// 任务生命周期时间线（planned → running → 终态）。
import { Timeline, Text } from "@mantine/core";
import {
  IconCircleCheck,
  IconPlayerPlay,
  IconPlayerStop,
  IconAlertCircle,
  IconFileDescription,
} from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

import type { MigrationTaskStatus } from "../../api/types";

interface Props {
  status: MigrationTaskStatus;
  createdAt?: string;
  startedAt?: string | null;
  finishedAt?: string | null;
  errorMessage?: string | null;
}

function activeIndex(status: MigrationTaskStatus): number {
  switch (status) {
    case "planned":
      return 0;
    case "running":
      return 1;
    case "completed":
    case "failed":
    case "cancelled":
      return 2;
    default:
      return 0;
  }
}

export function MigrationLifecycle({
  status,
  createdAt,
  startedAt,
  finishedAt,
  errorMessage,
}: Props) {
  const { t } = useTranslation();
  const idx = activeIndex(status);
  const endColor =
    status === "completed" ? "green" : status === "failed" ? "red" : status === "cancelled" ? "gray" : "blue";
  const EndIcon =
    status === "completed"
      ? IconCircleCheck
      : status === "failed"
        ? IconAlertCircle
        : status === "cancelled"
          ? IconPlayerStop
          : IconPlayerPlay;

  return (
    <Timeline active={idx} bulletSize={28} lineWidth={2} color={status === "running" ? "blue" : endColor}>
      <Timeline.Item
        bullet={<IconFileDescription size={14} />}
        title={t("migrations.status_planned")}
        color="yellow"
      >
        <Text size="xs" c="dimmed">
          {createdAt || "—"}
        </Text>
        <Text size="xs" c="dimmed">
          {t("migrations.lifecyclePlannedHint")}
        </Text>
      </Timeline.Item>
      <Timeline.Item
        bullet={<IconPlayerPlay size={14} />}
        title={t("migrations.status_running")}
        color="blue"
        lineVariant={status === "planned" ? "dashed" : "solid"}
      >
        <Text size="xs" c="dimmed">
          {startedAt || (status === "planned" ? t("migrations.lifecycleNotStarted") : "—")}
        </Text>
        {status === "running" && (
          <Text size="xs" c="blue">
            {t("migrations.runningHint")}
          </Text>
        )}
      </Timeline.Item>
      <Timeline.Item bullet={<EndIcon size={14} />} title={endTitle(status, t)} color={endColor}>
        <Text size="xs" c="dimmed">
          {finishedAt ||
            (status === "planned" || status === "running"
              ? t("migrations.lifecycleNotFinished")
              : "—")}
        </Text>
        {errorMessage && (
          <Text size="xs" c="red" mt={4}>
            {errorMessage}
          </Text>
        )}
      </Timeline.Item>
    </Timeline>
  );
}

function endTitle(
  status: MigrationTaskStatus,
  t: (k: string) => string,
): string {
  if (status === "completed" || status === "failed" || status === "cancelled") {
    return t(`migrations.status_${status}`);
  }
  return t("migrations.lifecycleEndPending");
}
