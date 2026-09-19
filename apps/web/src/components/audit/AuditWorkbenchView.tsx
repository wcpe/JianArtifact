// 审计工作台（单列表布局）：顶部 KPI 卡片指标 + OpsSection 记录表（工具行含筛选）。
// 页面标题由页眉面包屑承担；外层锁定视口高度（calc(100dvh - 页眉 - 内容边距)）、
// 列表在剩余高度内滚动；风险批次改为「风险状态」筛选维度，详情走行内展开与抽屉深链。
import { useCallback, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Alert, Box, Button, Group, Stack, Text } from "@mantine/core";
import { useMediaQuery } from "@mantine/hooks";
import {
  IconAlertCircle,
  IconAlertTriangle,
  IconBell,
  IconCircleCheck,
  IconCircleX,
  IconChevronDown,
  IconChevronUp,
  IconClock,
  IconFileAnalytics,
  IconHourglass,
  IconNetwork,
  IconPercentage,
  IconRefresh,
  IconServer,
  IconUsers,
} from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

import { ForbiddenState } from "@jianartifact/ui";

import { ApiError } from "../../api/client";
import { currentLocaleTag } from "../../i18n/current";
import { PageShell } from "../../app/PageShell";
import { OpsKpiBand } from "../ops/OpsKit";
import type { OpsKpiItem } from "../ops/OpsKit";
import { formatDuration } from "./labels";
import { AttentionDrawer, dispatchGlobalRefresh } from "./AttentionDrawer";
import { RecordsStream } from "./RecordsStream";
import { useAuditQuery } from "./useAuditQuery";

// 窄屏主指标在 kpiItems 里的下标（总数 / 失败 / 高危 / 成功率 / 平均耗时 / 待确认批次）。
const NARROW_KPI_INDEXES = [0, 2, 3, 4, 5, 10];

export function AuditWorkbenchView() {
  const { t } = useTranslation();
  const [searchParams, setSearchParams] = useSearchParams();
  const model = useAuditQuery();
  const urlAttentionId = searchParams.get("attentionId");
  // 深链批次已变化的本地提示（snapshotStale 只覆盖列表读取错误，抽屉 onStale 需独立记录）。
  const [staleNotice, setStaleNotice] = useState(false);
  // 窄屏（< 48em）：12 项 KPI 用独立卡片要占 6 行、首屏看不到记录列表，
  // 改用紧凑横带（共享 OpsKpiBand 的 strip 变体）把高度压掉一多半。
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;
  // 窄屏 KPI 带的"更多指标"展开态。
  const [kpiExpanded, setKpiExpanded] = useState(false);

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

  // 12 个指标在窄屏要占 4 行（近半屏），而记录表才是这页的主体。
  // 窄屏只渲染 6 个主指标（总数/失败/高危/成功率/平均耗时/待确认批次），其余用「更多指标」展开；
  // 宽屏（cards 变体）保持 12 个全展示。索引对应下方 kpiItems 的顺序。
  const kpiItems: OpsKpiItem[] = [
    {
      label: t("auditWorkbench.kpiTotal"),
      value: summary ? summary.totalCount.toLocaleString(currentLocaleTag()) : "—",
      hint: t("auditWorkbench.kpiTotalHint"),
      tone: "blue",
      icon: <IconFileAnalytics size={18} />,
    },
    {
      label: t("auditWorkbench.kpiSuccess"),
      value: summary ? summary.successCount.toLocaleString(currentLocaleTag()) : "—",
      tone: "green",
      icon: <IconCircleCheck size={18} />,
    },
    {
      label: t("auditWorkbench.kpiFailure"),
      value: summary ? summary.failureCount.toLocaleString(currentLocaleTag()) : "—",
      tone: "red",
      danger: Boolean(summary && summary.failureCount > 0),
      icon: <IconCircleX size={18} />,
    },
    {
      label: t("auditWorkbench.kpiHighRisk"),
      value: summary ? summary.highRiskCount.toLocaleString(currentLocaleTag()) : "—",
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
        !summary || summary.totalCount === 0 ? "—" : formatDuration(summary.averageDurationMs ?? 0),
      tone: "cyan",
      icon: <IconClock size={18} />,
    },
    {
      label: t("auditWorkbench.kpiSlowRequest"),
      value: summary ? (summary.slowRequestCount ?? 0).toLocaleString(currentLocaleTag()) : "—",
      hint: t("auditWorkbench.kpiSlowRequestHint"),
      tone: "yellow",
      danger: Boolean(summary && (summary.slowRequestCount ?? 0) > 0),
      icon: <IconHourglass size={18} />,
    },
    {
      label: t("auditWorkbench.kpiClientError"),
      value: summary ? (summary.clientErrorCount ?? 0).toLocaleString(currentLocaleTag()) : "—",
      hint: t("auditWorkbench.kpiClientErrorHint"),
      tone: "orange",
      icon: <IconAlertCircle size={18} />,
    },
    {
      label: t("auditWorkbench.kpiServerError"),
      value: summary ? (summary.serverErrorCount ?? 0).toLocaleString(currentLocaleTag()) : "—",
      hint: t("auditWorkbench.kpiServerErrorHint"),
      tone: "red",
      danger: Boolean(summary && (summary.serverErrorCount ?? 0) > 0),
      icon: <IconServer size={18} />,
    },
    {
      label: t("auditWorkbench.kpiClientIp"),
      value: summary ? (summary.distinctClientIpCount ?? 0).toLocaleString(currentLocaleTag()) : "—",
      hint: t("auditWorkbench.kpiClientIpHint"),
      tone: "blue",
      icon: <IconNetwork size={18} />,
    },
    {
      label: t("auditWorkbench.kpiPendingAttention"),
      value: model.pendingAttentionCount.toLocaleString(currentLocaleTag()),
      hint: t("auditWorkbench.kpiPendingHint"),
      tone: "yellow",
      danger: model.pendingAttentionCount > 0,
      icon: <IconBell size={18} />,
    },
    {
      label: t("auditWorkbench.kpiActors"),
      value: summary ? summary.distinctActorCount.toLocaleString(currentLocaleTag()) : "—",
      hint: t("auditWorkbench.kpiActorsHint"),
      tone: "gray",
      icon: <IconUsers size={18} />,
    },
  ];

  const narrowKpiItems = NARROW_KPI_INDEXES.map((index) => kpiItems[index]).filter(
    (item): item is OpsKpiItem => Boolean(item),
  );
  const visibleKpiItems = isNarrow && !kpiExpanded ? narrowKpiItems : kpiItems;

  return (
    <PageShell testId="audit-workbench" gap={20}>
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
            <Button
              size="xs"
              variant="light"
              onClick={dismissStaleAndReload}
              leftSection={<IconRefresh size={14} />}
            >
              {t("auditWorkbench.staleAction")}
            </Button>
          </Group>
        </Alert>
      ) : null}
      {model.summary.error && !model.summary.refreshError && !summary ? (
        // 「聚合范围过大」是**可自解**的错误（后端 409 audit_query_too_large），不能和
        // 真·不可用混为一谈：原来一律显示「当前节点审计暂时不可用」，用户既不知道原因，
        // 也无从下手（重试必然还是同一结果）。这里给出原因 + 一键切到最小窗口。
        model.summary.error instanceof ApiError &&
        model.summary.error.code === "audit_query_too_large" ? (
          <Alert
            color="yellow"
            title={t("auditWorkbench.tooLargeTitle")}
            icon={<IconAlertTriangle size={16} />}
          >
            <Stack gap="sm">
              <Text size="sm">{model.summary.error.message}</Text>
              <Button
                size="xs"
                variant="light"
                w="fit-content"
                leftSection={<IconClock size={14} />}
                onClick={() => model.setRange("1h")}
              >
                {t("auditWorkbench.tooLargeAction")}
              </Button>
            </Stack>
          </Alert>
        ) : (
          <Alert
            color="red"
            title={t("auditWorkbench.loadErrorTitle")}
            icon={<IconAlertTriangle size={16} />}
          >
            <Stack gap="sm">
              <Text size="sm">{model.summary.error.message}</Text>
              <Button
                size="xs"
                variant="light"
                w="fit-content"
                onClick={model.reloadAll}
                leftSection={<IconRefresh size={14} />}
              >
                {t("common.retry", { defaultValue: "重试" })}
              </Button>
            </Stack>
          </Alert>
        )
      ) : null}

      {/* 顶部 KPI 指标带：icon + 大数字（固定，不随列表滚动）；窄屏切紧凑横带 */}
      <OpsKpiBand
        label={t("auditWorkbench.kpiBandLabel")}
        variant={isNarrow ? "strip" : "cards"}
        // 窄屏 3 列：12 个指标从 6 行压到 4 行（约省 140px）。2 列时 KPI 带独占半屏，
        // 记录表只剩 199px——本来就没多少地方给数据。
        cols={isNarrow ? { base: 3, xs: 3 } : { base: 2, xs: 3, sm: 4, lg: 6 }}
        items={visibleKpiItems}
        actions={
          isNarrow ? (
            <Button
              size="compact-xs"
              variant="subtle"
              leftSection={
                kpiExpanded ? <IconChevronUp size={14} /> : <IconChevronDown size={14} />
              }
              onClick={() => setKpiExpanded((open) => !open)}
            >
              {kpiExpanded
                ? t("auditWorkbench.kpiLess", { defaultValue: "收起指标" })
                : t("auditWorkbench.kpiMore", { defaultValue: "更多指标" })}
            </Button>
          ) : undefined
        }
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
    </PageShell>
  );
}
