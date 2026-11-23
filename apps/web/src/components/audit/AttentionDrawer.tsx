// 风险批次详情抽屉：完整成员分页加载 + 二次确认防误触。
// 深链 attentionId、attention_stale 处理与确认流语义与旧实现保持一致。
import { useEffect, useRef, useState } from "react";
import { IconCheck, IconRefresh } from "@tabler/icons-react";
import { useTranslation } from "react-i18next";
import {
  Alert,
  Badge,
  Box,
  Button,
  Card,
  Code,
  Drawer,
  Group,
  Loader,
  Popover,
  Stack,
  Text,
} from "@mantine/core";

import { ForbiddenState } from "@jianartifact/ui";

import { acknowledgeAuditAttention, getAuditAttention } from "../../api/endpoints";
import { ApiError } from "../../api/client";
import type { AuditEvent } from "../../api/types";
import { REFRESH_EVENT, useAsync } from "../../hooks/useAsync";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { AUDIT_PAGE_SIZE } from "./useAuditQuery";
import {
  actionLabel,
  actorText,
  asText,
  formatDuration,
  formatTime,
  resultColor,
  resultLabelKey,
  severityColor,
  severityLabelKey,
} from "./labels";

export interface AttentionDrawerProps {
  attentionId: string;
  onClose: () => void;
  onAcknowledged: () => void;
  onStale: () => void;
}
/** 批次概览的聚合结果：谁做的、做了什么、影响范围、时间跨度。 */
interface BatchOverviewData {
  /** 主要操作者（按事件数降序），最多保留两个用于「等 N 人」。 */
  actors: string[];
  actorCount: number;
  /** 去重后的动作（i18n 后的可读名称），最多保留三个。 */
  actions: string[];
  actionCount: number;
  /** 失败事件数：批次的核心风险信号，>0 时高亮。 */
  failureCount: number;
  /** 时间跨度：首末事件时间。 */
  firstAt: string;
  latestAt: string;
}

/**
 * 汇总批次成员，回答「谁做了什么」。
 *
 * 此前抽屉顶部同时展示 affectedCount 与 totalCount，而服务端
 * `AffectedCount = len(group.events)`，两者永远是同一个数字，读者只看到重复。
 * 这里改为按操作者 / 动作去重聚合，把批次真正想表达的信息显性化。
 */
function summarizeBatch(
  events: AuditEvent[],
  attention: { firstOccurredAt?: string; latestOccurredAt?: string },
  t: (key: string, options?: Record<string, unknown>) => string,
): BatchOverviewData {
  const actorCounter = new Map<string, number>();
  const actionCounter = new Map<string, number>();
  let failureCount = 0;

  for (const event of events) {
    const actor = actorText(event.actor, t);
    actorCounter.set(actor, (actorCounter.get(actor) ?? 0) + 1);
    const action = actionLabel(event.action, t);
    actionCounter.set(action, (actionCounter.get(action) ?? 0) + 1);
    if (event.result === "failure") failureCount += 1;
  }

  const byCountDesc = (counter: Map<string, number>) =>
    [...counter.entries()].sort((a, b) => b[1] - a[1]).map(([name]) => name);

  const actorNames = byCountDesc(actorCounter);
  const actionNames = byCountDesc(actionCounter);
  const times = events.map((event) => event.occurredAt).filter(Boolean);
  const sortedTimes = [...times].sort();

  return {
    actors: actorNames.slice(0, 2),
    actorCount: actorNames.length,
    actions: actionNames.slice(0, 3),
    actionCount: actionNames.length,
    failureCount,
    firstAt: attention.firstOccurredAt ?? sortedTimes[0] ?? "",
    latestAt: attention.latestOccurredAt ?? sortedTimes[sortedTimes.length - 1] ?? "",
  };
}

/**
 * 批次概览卡：把「谁做了什么」放在最前面，让读者不必逐条读记录就能判断批次性质。
 * 只展示结论性指标；IP、UA、请求头等排障上下文收在每条记录的展开区里。
 */
function BatchOverview({
  overview,
  totalCount,
}: {
  overview: BatchOverviewData;
  totalCount: number;
}) {
  const { t } = useTranslation();
  // 单人单动作是批次最常见形态，此时直接给结论句，避免堆一排只有一项的标签。
  const actorSummary =
    overview.actorCount === 1
      ? overview.actors[0]
      : t("auditWorkbench.drawerActorMultiple", {
          first: overview.actors.join(" / "),
          count: overview.actorCount,
        });
  const actionSummary =
    overview.actionCount === 1
      ? overview.actions[0]
      : t("auditWorkbench.drawerActionMultiple", {
          first: overview.actions.join(" / "),
          count: overview.actionCount,
        });

  return (
    <Card withBorder radius="sm" padding="sm" component="section" aria-label={t("auditWorkbench.drawerOverview")}>
      <Text size="xs" c="dimmed" fw={600} mb={6}>
        {t("auditWorkbench.drawerOverview")}
      </Text>
      <Stack gap={6}>
        <Group gap={6} wrap="wrap" align="baseline">
          <Text size="xs" c="dimmed" w={64} style={{ flexShrink: 0 }}>
            {t("auditWorkbench.drawerWho")}
          </Text>
          <Text size="sm" fw={600}>{actorSummary}</Text>
        </Group>
        <Group gap={6} wrap="wrap" align="baseline">
          <Text size="xs" c="dimmed" w={64} style={{ flexShrink: 0 }}>
            {t("auditWorkbench.drawerWhat")}
          </Text>
          <Text size="sm" fw={600}>{actionSummary}</Text>
        </Group>
        <Group gap={6} wrap="wrap" align="baseline">
          <Text size="xs" c="dimmed" w={64} style={{ flexShrink: 0 }}>
            {t("auditWorkbench.drawerScale")}
          </Text>
          <Text size="sm">
            {t("auditWorkbench.drawerScaleValue", { total: totalCount })}
            {overview.failureCount > 0 ? (
              <Text span size="sm" c="red" fw={600} ml={6}>
                {t("auditWorkbench.drawerScaleFailure", { count: overview.failureCount })}
              </Text>
            ) : null}
          </Text>
        </Group>
        <Group gap={6} wrap="wrap" align="baseline">
          <Text size="xs" c="dimmed" w={64} style={{ flexShrink: 0 }}>
            {t("auditWorkbench.drawerWhen")}
          </Text>
          <Text size="sm">
            {formatTime(overview.firstAt)} — {formatTime(overview.latestAt)}
          </Text>
        </Group>
      </Stack>
    </Card>
  );
}

/**
 * 批次成员行：默认只显示「谁 · 做了什么 · 结果 · 时间」四要素，
 * 请求 ID / IP / UA / 请求头 / 请求体等排障上下文按需展开。
 *
 * 此前这些技术字段在成员卡片里直接平铺，一次设置变更也会撑出十几行
 * 重复的对话框、UA 与请求体，用户反馈「一堆数据铺开有点迷糊」。
 */
function BatchMemberRow({ event }: { event: AuditEvent }) {
  const { t } = useTranslation();
  const [opened, setOpened] = useState(false);
  const http = event.http as
    | {
        method?: string;
        path?: string;
        statusCode?: number;
        requestId?: string;
        userAgent?: string;
        tokenPreview?: string;
        bodyPreview?: string;
      }
    | undefined;
  // 目标文本对设置类事件是键名、对制品事件是路径，二者都是「改了什么」的主体。
  const target = asText(event.target);
  const hasContext = Boolean(
    http?.requestId ||
      http?.userAgent ||
      http?.tokenPreview ||
      http?.bodyPreview ||
      event.clientIp ||
      event.durationMs ||
      event.durationServerMs,
  );

  return (
    <Box
      style={{
        borderTop: "1px solid var(--mantine-color-default-border)",
        paddingTop: 8,
      }}
    >
      <Group gap="xs" wrap="wrap" justify="space-between" align="flex-start">
        <Box style={{ flex: "1 1 240px", minWidth: 0 }}>
          <Group gap="xs" wrap="wrap">
            <Badge color={resultColor(event.result)} variant="light" size="sm">
              {t(resultLabelKey(event.result))}
            </Badge>
            <Text size="sm" fw={600}>
              {actorText(event.actor, t)}
            </Text>
            <Text size="xs" c="dimmed">
              {formatTime(event.occurredAt)}
            </Text>
          </Group>
          {/* 目标（改了什么）放在主行加粗；动作作为前置标签退到副行——
              排查时要先认目标，再判断这个动作对不对。 */}
          <Text size="sm" mt={2} fw={600} style={{ overflowWrap: "anywhere" }}>
            {target && target !== "—" ? target : actionLabel(event.action, t)}
          </Text>
          <Text size="xs" c="dimmed" mt={1}>
            {actionLabel(event.action, t)}
          </Text>
          {event.summary ? (
            <Text size="xs" mt={2} style={{ lineHeight: 1.6 }}>
              {event.summary}
            </Text>
          ) : null}
        </Box>
        {hasContext ? (
          <Button
            size="compact-xs"
            variant="subtle"
            onClick={() => setOpened((value) => !value)}
            aria-expanded={opened}
          >
            {opened ? t("auditWorkbench.drawerHideContext") : t("auditWorkbench.drawerShowContext")}
          </Button>
        ) : null}
      </Group>
      {opened && hasContext ? (
        <Box mt="xs" pl="xs" style={{ borderLeft: "2px solid var(--mantine-color-default-border)" }}>
          <Stack gap={2}>
            {http?.method || http?.path ? (
              <Text size="xs" c="dimmed" style={{ overflowWrap: "anywhere" }}>
                {t("auditWorkbench.drawerContextRequest", {
                  method: http?.method?.toUpperCase() ?? "—",
                  path: http?.path ?? "—",
                  status: http?.statusCode ?? "—",
                })}
              </Text>
            ) : null}
            {event.clientIp ? (
              <Text size="xs" c="dimmed">{t("auditWorkbench.drawerContextIp", { ip: event.clientIp })}</Text>
            ) : null}
            {event.durationMs || event.durationServerMs ? (
              <Text size="xs" c="dimmed">
                {t("auditWorkbench.drawerContextDuration", {
                  total: formatDuration(event.durationMs),
                  server: formatDuration(event.durationServerMs),
                })}
              </Text>
            ) : null}
            {http?.requestId ? (
              <Text size="xs" c="dimmed" style={{ overflowWrap: "anywhere" }}>
                {t("auditWorkbench.drawerContextRequestId", { id: http.requestId })}
              </Text>
            ) : null}
            {http?.tokenPreview ? (
              <Text size="xs" c="dimmed">{t("auditWorkbench.drawerContextToken", { token: http.tokenPreview })}</Text>
            ) : null}
            {http?.userAgent ? (
              <Text size="xs" c="dimmed" style={{ overflowWrap: "anywhere" }}>
                {t("auditWorkbench.drawerContextUserAgent", { ua: http.userAgent })}
              </Text>
            ) : null}
            {http?.bodyPreview ? (
              <Box mt={2}>
                <Text size="xs" c="dimmed" mb={2}>{t("auditWorkbench.drawerContextBody")}</Text>
                <Code block style={{ fontSize: 11, whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>
                  {http.bodyPreview}
                </Code>
              </Box>
            ) : null}
          </Stack>
        </Box>
      ) : null}
    </Box>
  );
}


export function AttentionDrawer({
  attentionId,
  onClose,
  onAcknowledged,
  onStale,
}: AttentionDrawerProps) {
  const { t } = useTranslation();
  const state = useAsync(() => getAuditAttention(attentionId), [attentionId]);
  const [acknowledging, setAcknowledging] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [items, setItems] = useState<AuditEvent[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>();
  const [loadingMore, setLoadingMore] = useState(false);

  const staleFiredRef = useRef(false);
  useEffect(() => {
    if (
      state.error instanceof ApiError &&
      state.error.code === "attention_stale" &&
      !staleFiredRef.current
    ) {
      staleFiredRef.current = true;
      onStale();
    }
  }, [onStale, state.error]);

  useEffect(() => {
    if (!state.data) return;
    setItems(state.data.items);
    setNextCursor(state.data.nextCursor);
  }, [state.data]);

  const detail = state.data;
  // 批次概览：把「谁做了什么」压成可扫读的结论，替代此前重复的计数与技术字段铺开。
  const overview = detail ? summarizeBatch(detail.items, detail.attention, t) : null;

  const acknowledge = async () => {
    setAcknowledging(true);
    try {
      const result = await acknowledgeAuditAttention(attentionId);
      notifySuccess(t("auditWorkbench.acknowledgedToast", { count: result.totalRiskEventCount }));
      setConfirmOpen(false);
      onAcknowledged();
    } catch (error) {
      if (error instanceof ApiError && error.code === "attention_stale") onStale();
      else notifyError(error);
    } finally {
      setAcknowledging(false);
    }
  };

  const loadMore = async () => {
    if (!nextCursor || loadingMore) return;
    setLoadingMore(true);
    try {
      const page = await getAuditAttention(attentionId, {
        cursor: nextCursor,
        limit: AUDIT_PAGE_SIZE,
      });
      setItems((current) => {
        const seen = new Set(current.map((item) => item.eventId));
        return [...current, ...page.items.filter((item) => !seen.has(item.eventId))];
      });
      setNextCursor(page.nextCursor);
    } catch (error) {
      notifyError(error);
    } finally {
      setLoadingMore(false);
    }
  };

  return (
    <Drawer
      opened
      onClose={onClose}
      title={t("auditWorkbench.drawerTitle")}
      position="right"
      size="lg"
    >
      {state.forbidden ? <ForbiddenState message={t("auditWorkbench.drawerForbidden")} /> : null}
      {state.loading ? (
        <Group justify="center" py="lg">
          <Loader size="sm" />
        </Group>
      ) : null}
      {state.error &&
      !(state.error instanceof ApiError && state.error.code === "attention_stale") ? (
        <Alert color="red" title={t("auditWorkbench.drawerLoadError")}>
          <Box mt="sm">
            <Button
              size="xs"
              variant="light"
              onClick={state.reload}
              leftSection={<IconRefresh size={14} />}
            >
              {t("common.retry", { defaultValue: "重试" })}
            </Button>
          </Box>
        </Alert>
      ) : null}
      {detail && overview ? (
        <Stack gap="md">
          <Stack gap={4}>
            <Group gap="xs">
              <Badge color={resultColor(detail.attention.result)}>
                {t(resultLabelKey(detail.attention.result))}
              </Badge>
              <Badge color={detail.attention.state === "unacknowledged" ? "orange" : "blue"}>
                {detail.attention.state === "unacknowledged"
                  ? t("notifications.stateUnacknowledged")
                  : t("notifications.stateAcknowledged")}
              </Badge>
              {severityLabelKey(detail.attention.severity) ? (
                <Badge color={severityColor(detail.attention.severity)}>
                  {t(severityLabelKey(detail.attention.severity)!)}
                </Badge>
              ) : null}
            </Group>
            <Text fw={700}>{actionLabel(detail.attention.action, t)}</Text>
          </Stack>
          <BatchOverview overview={overview} totalCount={detail.totalCount} />
          {detail.attention.state === "unacknowledged" ? (
            <Popover
              opened={confirmOpen}
              onChange={setConfirmOpen}
              width={240}
              position="top"
              withArrow
              shadow="md"
            >
              <Popover.Target>
                <Button
                  loading={acknowledging}
                  onClick={() => setConfirmOpen((open) => !open)}
                  leftSection={<IconCheck size={16} />}
                >
                  {t("auditWorkbench.confirmTitle")}
                </Button>
              </Popover.Target>
              <Popover.Dropdown>
                <Stack gap="xs">
                  <Text size="xs">{t("auditWorkbench.confirmPopover")}</Text>
                  <Group gap="xs">
                    <Button
                      size="xs"
                      leftSection={<IconCheck size={14} />}
                      onClick={() => void acknowledge()}
                    >
                      {t("auditWorkbench.confirmYes")}
                    </Button>
                    <Button size="xs" variant="default" onClick={() => setConfirmOpen(false)}>
                      {t("auditWorkbench.confirmNo")}
                    </Button>
                  </Group>
                </Stack>
              </Popover.Dropdown>
            </Popover>
          ) : null}
          <Stack gap="xs">
            <Text fw={600}>{t("auditWorkbench.drawerMembers", { total: detail.totalCount })}</Text>
            {items.map((event) => (
              <BatchMemberRow key={event.eventId} event={event} />
            ))}
            {nextCursor ? (
              <Button variant="default" loading={loadingMore} onClick={() => void loadMore()}>
                {t("auditWorkbench.drawerLoadMore")}
              </Button>
            ) : null}
          </Stack>
        </Stack>
      ) : null}
    </Drawer>
  );
}

/** 确认成功后的全局联动（刷新页眉通知等）。 */
export function dispatchGlobalRefresh() {
  window.dispatchEvent(new CustomEvent(REFRESH_EVENT));
}
