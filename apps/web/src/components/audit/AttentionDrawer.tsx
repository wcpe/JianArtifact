// 风险批次详情抽屉（FR-118）：完整成员分页加载 + 二次确认防误触。
// 深链 attentionId、attention_stale 处理与确认流语义与旧实现保持一致。
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Alert,
  Badge,
  Box,
  Button,
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
            <Button size="xs" variant="light" onClick={state.reload}>
              {t("common.retry", { defaultValue: "重试" })}
            </Button>
          </Box>
        </Alert>
      ) : null}
      {detail ? (
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
            <Text>{asText(detail.attention.target)}</Text>
            <Text size="sm" c="dimmed">
              {detail.attention.summary}
            </Text>
            <Text size="xs" c="dimmed">
              {t("auditWorkbench.drawerMeta", {
                affected: detail.attention.affectedCount,
                total: detail.totalCount,
              })}
            </Text>
          </Stack>
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
                <Button loading={acknowledging} onClick={() => setConfirmOpen((open) => !open)}>
                  {t("auditWorkbench.confirmTitle")}
                </Button>
              </Popover.Target>
              <Popover.Dropdown>
                <Stack gap="xs">
                  <Text size="xs">{t("auditWorkbench.confirmPopover")}</Text>
                  <Group gap="xs">
                    <Button size="xs" onClick={() => void acknowledge()}>
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
              <Box
                key={event.eventId}
                style={{
                  borderTop: "1px solid var(--mantine-color-default-border)",
                  paddingTop: 8,
                }}
              >
                <Group gap="xs" wrap="wrap">
                  <Badge color={resultColor(event.result)} variant="light" size="sm">
                    {t(resultLabelKey(event.result))}
                  </Badge>
                  <Text size="sm" fw={600}>
                    {event.action}
                  </Text>
                  <Text size="xs" c="dimmed">
                    {actorText(event.actor, t)} · {formatTime(event.occurredAt)}
                  </Text>
                </Group>
                <Text size="xs" c="dimmed" mt={2}>
                  {asText(event.target)}
                </Text>
                {event.summary ? (
                  <Text size="xs" mt={2}>
                    {event.summary}
                  </Text>
                ) : null}
              </Box>
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
