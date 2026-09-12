// 审计工作台（单列表布局）：顶部 KPI 卡片指标 + OpsSection 记录表（工具行含筛选）。
// 页面标题由页眉面包屑承担；外层锁定视口高度（calc(100dvh - 页眉 - 内容边距)）、
// 列表在剩余高度内滚动；风险批次改为「风险状态」筛选维度，详情走行内展开与抽屉深链。
import { useCallback, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Alert, Box, Button, Group, Stack, Text } from "@mantine/core";
import {
  IconAlertCircle,
  IconAlertTriangle,
  IconBell,
  IconCircleCheck,
  IconCircleX,
  IconClock,
  IconFileAnalytics,
  IconHourglass,
  IconNetwork,
  IconPercentage,
  IconServer,
  IconUsers,
} from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

import { ForbiddenState } from "@jianartifact/ui";

import { OpsKpiBand } from "../ops/OpsKit";
import { formatDuration } from "./labels";
import { AttentionDrawer, dispatchGlobalRefresh } from "./AttentionDrawer";
import { RecordsStream } from "./RecordsStream";
import { useAuditQuery } from "./useAuditQuery";

export function AuditWorkbenchView() {
  const { t } = useTranslation();
  const [searchParams, setSearchParams] = useSearchParams();
  const model = useAuditQuery();
  const urlAttentionId = searchParams.get("attentionId");
  // 深链批次已变化的本地提示（snapshotStale 只覆盖列表读取错误，抽屉 onStale 需独立记录）。
  const [staleNotice, setStaleNotice] = useState(false);

  const clearAttention = useCallback(() => {
    const next = new URLSearchParams(searchParams);
    next.delete("attentionId");
    setSearchParams(next, { replace: true });
  }, [searchParams, setSearchParams]);

  const handleAcknowledged = useCallback(() => {
    clearAttention();
    model.reloadAll();
    dispatchGlobalRefresh();
  }, [clearAttention, model]);

  const handleStale = useCallback(() => {
    clearAttention();
    setStaleNotice(true);
    model.reloadAll();
  }, [clearAttention, model]);

  const dismissStaleAndReload = useCallback(() => {
    setStaleNotice(false);
    model.reloadAll();
  }, [model]);

  const openAttention = useCallback(
    (id: string) => {
      const next = new URLSearchParams(searchParams);
      next.set("attentionId", id);
      setSearchParams(next);
    },
    [searchParams, setSearchParams],
  );

  const investigate = useCallback(
    (keyword: string) => {
      model.setDraft(keyword);
      model.setSearch(keyword);
    },
    [model],
  );

  if (model.summary.forbidden) {
    return <ForbiddenState message={t("auditWorkbench.forbidden")} />;
  }

  const summary = model.summary.data;

  return (
    <Box
      data-testid="audit-workbench"
      style={{
        height: "calc(100dvh - var(--app-shell-header-height) - var(--app-shell-padding) * 2)",
        minHeight: 480,
        display: "flex",
        flexDirection: "column",
        // KPI 卡片区与下方列表分区卡之间留出更明显的呼吸区（内联样式须用具体值）。
        gap: 20,
        overflow: "hidden",
      }}
    >
      {model.summary.refreshError ? (
        <Alert
          color="yellow"
          title={t("auditWorkbench.refreshErrorTitle")}
          icon={<IconAlertTriangle size={16} />}
        >
          <Text size="sm">{t("auditWorkbench.refreshErrorBody")}</Text>
        </Alert>
      ) : null}
      {model.snapshotStale || staleNotice ? (
        <Alert
          color="yellow"
          title={t("auditWorkbench.staleTitle")}
          icon={<IconAlertTriangle size={16} />}
        >
          <Group justify="space-between" align="center">
            <Text size="sm">{t("auditWorkbench.staleBody")}</Text>
            <Button size="xs" variant="light" onClick={dismissStaleAndReload}>
              {t("auditWorkbench.staleAction")}
            </Button>
          </Group>
        </Alert>
      ) : null}
      {model.summary.error && !model.summary.refreshError && !summary ? (
        <Alert
          color="red"
          title={t("auditWorkbench.loadErrorTitle")}
          icon={<IconAlertTriangle size={16} />}
        >
          <Stack gap="sm">
            <Text size="sm">{model.summary.error.message}</Text>
            <Button size="xs" variant="light" w="fit-content" onClick={model.reloadAll}>
              {t("common.retry", { defaultValue: "重试" })}
            </Button>
          </Stack>
        </Alert>
      ) : null}

      {/* 顶部 KPI 指标带：icon + 大数字（固定，不随列表滚动） */}
      <OpsKpiBand
        label={t("auditWorkbench.kpiBandLabel")}
        items={[
          {
            label: t("auditWorkbench.kpiTotal"),
            value: summary ? summary.totalCount.toLocaleString("zh-CN") : "—",
            hint: t("auditWorkbench.kpiTotalHint"),
            tone: "blue",
            icon: <IconFileAnalytics size={18} />,
          },
          {
            label: t("auditWorkbench.kpiSuccess"),
            value: summary ? summary.successCount.toLocaleString("zh-CN") : "—",
            tone: "green",
            icon: <IconCircleCheck size={18} />,
          },
          {
            label: t("auditWorkbench.kpiFailure"),
            value: summary ? summary.failureCount.toLocaleString("zh-CN") : "—",
            tone: "red",
            danger: Boolean(summary && summary.failureCount > 0),
            icon: <IconCircleX size={18} />,
          },
          {
            label: t("auditWorkbench.kpiHighRisk"),
            value: summary ? summary.highRiskCount.toLocaleString("zh-CN") : "—",
            tone: "orange",
            danger: Boolean(summary && summary.highRiskCount > 0),
            icon: <IconAlertTriangle size={18} />,
          },
          {
            label: t("auditWorkbench.kpiSuccessRate"),
            value:
              !summary || summary.totalCount === 0
                ? "—"
                : `${((1 - summary.failureRate) * 100).toFixed(1)}%`,
            hint: t("auditWorkbench.kpiSuccessRateHint"),
            tone: "teal",
            icon: <IconPercentage size={18} />,
          },
          {
            label: t("auditWorkbench.kpiAverageDuration"),
            value:
              !summary || summary.totalCount === 0
                ? "—"
                : formatDuration(summary.averageDurationMs ?? 0),
            tone: "cyan",
            icon: <IconClock size={18} />,
          },
          {
            label: t("auditWorkbench.kpiSlowRequest"),
            value: summary ? (summary.slowRequestCount ?? 0).toLocaleString("zh-CN") : "—",
            hint: t("auditWorkbench.kpiSlowRequestHint"),
            tone: "yellow",
            danger: Boolean(summary && (summary.slowRequestCount ?? 0) > 0),
            icon: <IconHourglass size={18} />,
          },
          {
            label: t("auditWorkbench.kpiClientError"),
            value: summary ? (summary.clientErrorCount ?? 0).toLocaleString("zh-CN") : "—",
            hint: t("auditWorkbench.kpiClientErrorHint"),
            tone: "orange",
            icon: <IconAlertCircle size={18} />,
          },
          {
            label: t("auditWorkbench.kpiServerError"),
            value: summary ? (summary.serverErrorCount ?? 0).toLocaleString("zh-CN") : "—",
            hint: t("auditWorkbench.kpiServerErrorHint"),
            tone: "red",
            danger: Boolean(summary && (summary.serverErrorCount ?? 0) > 0),
            icon: <IconServer size={18} />,
          },
          {
            label: t("auditWorkbench.kpiClientIp"),
            value: summary ? (summary.distinctClientIpCount ?? 0).toLocaleString("zh-CN") : "—",
            hint: t("auditWorkbench.kpiClientIpHint"),
            tone: "blue",
            icon: <IconNetwork size={18} />,
          },
          {
            label: t("auditWorkbench.kpiPendingAttention"),
            value: model.pendingAttentionCount.toLocaleString("zh-CN"),
            hint: t("auditWorkbench.kpiPendingHint"),
            tone: "yellow",
            danger: model.pendingAttentionCount > 0,
            icon: <IconBell size={18} />,
          },
          {
            label: t("auditWorkbench.kpiActors"),
            value: summary ? summary.distinctActorCount.toLocaleString("zh-CN") : "—",
            hint: t("auditWorkbench.kpiActorsHint"),
            tone: "gray",
            icon: <IconUsers size={18} />,
          },
        ]}
      />

      {/* 记录列表：剩余高度内滚动，整页不滚 */}
      <Box style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
        <RecordsStream model={model} onInvestigate={investigate} onOpenAttention={openAttention} />
      </Box>

      {urlAttentionId ? (
        <AttentionDrawer
          attentionId={urlAttentionId}
          onClose={clearAttention}
          onAcknowledged={handleAcknowledged}
          onStale={handleStale}
        />
      ) : null}
    </Box>
  );
}
