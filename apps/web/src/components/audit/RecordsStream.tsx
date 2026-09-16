// 审计日志列表：OpsSection 分区卡（标题行含时间范围）+ 两行筛选工具条
// + Mantine Table（完整时间 / 操作者邮箱 / 动作+方法路径 / 状态码 / 耗时 / 客户端 IP / 详情）。
// 全部筛选走服务端（关键字、动作、操作者邮箱、客户端 IP、请求方法、认证方式、结果、风险状态）。
import { useMediaQuery } from "@mantine/hooks";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Badge,
  Box,
  Button,
  Drawer,
  Group,
  MultiSelect,
  Pagination,
  SegmentedControl,
  Select,
  Card,
  Table,
  Text,
  TextInput,
} from "@mantine/core";
import { IconChevronDown, IconChevronUp, IconEye, IconSearch } from "@tabler/icons-react";

import { getAuditEvent } from "../../api/endpoints";
import type { AuditEvent, AuditEventDetail, AuditCategory, AuditResult } from "../../api/types";
import { OpsDetailGrid, OpsSection } from "../ops/OpsKit";
import {
  actionLabel,
  actorEmail,
  actorText,
  authSourceText,
  CATEGORY_LABEL_KEYS,
  CATEGORY_VALUES,
  formatDuration,
  formatClock,
  formatFullTime,
  requestMethod,
  resultLabelKey,
  ACTION_LABEL_KEYS,
} from "./labels";
import {
  AUDIT_PAGE_SIZE,
  AUDIT_PAGE_SIZES,
  type AuditAttentionFilter,
  type AuditWorkbenchModel,
  type AuditRange,
} from "./useAuditQuery";

const RESULT_LABEL_KEYS: Record<string, string> = {
  failure: "auditResult.failure",
  success: "auditResult.success",
  pending: "auditResult.pending",
  running: "auditResult.running",
  unknown: "auditResult.unknown",
};

const RANGE_OPTIONS = [
  { value: "1h", label: "1h" },
  { value: "24h", label: "24h" },
  { value: "7d", label: "7d" },
  { value: "30d", label: "30d" },
];

const METHOD_OPTIONS = ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"];
const AUTH_SOURCE_OPTIONS = ["jwt", "api_key", "web", "system", "anonymous"];

/** HTTP 状态码 → Badge 语义色。 */
function statusTone(statusCode: unknown): string {
  if (typeof statusCode !== "number") return "gray";
  if (statusCode >= 500) return "red";
  if (statusCode >= 400) return "orange";
  if (statusCode >= 300) return "blue";
  if (statusCode >= 200) return "green";
  return "gray";
}

/** 冻结表头：必须给不透明背景，否则滚动时下方行内容会从表头透出来。 */
const STICKY_HEADER = {
  position: "sticky",
  top: 0,
  zIndex: 2,
  background: "var(--mantine-color-body)",
  boxShadow: "inset 0 -1px 0 var(--mantine-color-default-border)",
} as const;

interface RecordsStreamProps {
  model: AuditWorkbenchModel;
  onInvestigate: (keyword: string) => void;
  onOpenAttention: (attentionId: string) => void;
}

export function RecordsStream({ model, onInvestigate, onOpenAttention }: RecordsStreamProps) {
  const { t } = useTranslation();
  const [expandedEvent, setExpandedEvent] = useState<string | null>(null);
  const [eventDetails, setEventDetails] = useState<Record<string, AuditEventDetail | "loading">>(
    {},
  );
  const [ipDraft, setIpDraft] = useState(model.filters.clientIp ?? "");
  const { records, filters, setFilters, actorFacets, emailFacets } = model;
  // 窄屏：高级筛选收起，默认只留关键字那行。
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;
  const [moreOpen, setMoreOpen] = useState(false);

  const expandEvent = async (eventId: string) => {
    if (expandedEvent === eventId) {
      setExpandedEvent(null);
      return;
    }
    setExpandedEvent(eventId);
    if (!eventDetails[eventId]) {
      setEventDetails((current) => ({ ...current, [eventId]: "loading" }));
      try {
        const detail = await getAuditEvent(eventId);
        setEventDetails((current) => ({ ...current, [eventId]: detail }));
      } catch {
        setEventDetails((current) => ({ ...current, [eventId]: "loading" }));
      }
    }
  };

  // 窄屏抽屉复用：按展开 id 反查事件对象（expandedEvent 是 id 而非对象）。
  const expandedRecord = expandedEvent
    ? records.items.find((e) => e.eventId === expandedEvent)
    : undefined;

  const actorOptions = actorFacets.map(([name]) => ({ value: name, label: name }));
  const hasFilters =
    filters.category.length > 0 ||
    filters.result.length > 0 ||
    Boolean(filters.actor) ||
    Boolean(filters.attention) ||
    Boolean(filters.method) ||
    Boolean(filters.action) ||
    Boolean(filters.actorEmail) ||
    Boolean(filters.clientIp) ||
    Boolean(filters.authSource);

  // 高级筛选控件：宽屏内联展开，窄屏收进抽屉——窄屏内联展开会把记录表顶出视口
  // （外壳 overflow:hidden，顶出去就再也够不到），抽屉则完全不改变页面布局。
  const advancedFilters = (
    <>
      <Group gap="xs" wrap="wrap" mt="xs">
        <Select
          size="xs"
          w={190}
          searchable
          clearable
          aria-label={t("auditWorkbench.filterActorEmail")}
          placeholder={t("auditWorkbench.filterActorEmail")}
          data={emailFacets.map((email) => ({ value: email, label: email }))}
          value={filters.actorEmail ?? null}
          onChange={(value) => setFilters({ ...filters, actorEmail: value ?? undefined })}
        />
        <Select
          size="xs"
          w={190}
          searchable
          clearable
          aria-label={t("auditWorkbench.filterAction")}
          placeholder={t("auditWorkbench.filterAction")}
          data={Object.keys(ACTION_LABEL_KEYS).map((action) => ({
            value: action,
            label: actionLabel(action, t),
          }))}
          value={filters.action ?? null}
          onChange={(value) => setFilters({ ...filters, action: value ?? undefined })}
        />
        <TextInput
          size="xs"
          w={150}
          aria-label={t("auditWorkbench.filterClientIp")}
          placeholder={t("auditWorkbench.filterClientIp")}
          value={ipDraft}
          onChange={(e) => setIpDraft(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              setFilters({ ...filters, clientIp: ipDraft.trim() || undefined });
            }
          }}
          onBlur={() => {
            if ((filters.clientIp ?? "") !== ipDraft.trim()) {
              setFilters({ ...filters, clientIp: ipDraft.trim() || undefined });
            }
          }}
        />
      </Group>

      <Group gap="xs" wrap="wrap" mt="xs">
        <Select
          size="xs"
          w={130}
          clearable
          allowDeselect
          aria-label={t("auditWorkbench.filterMethod")}
          placeholder={t("auditWorkbench.filterMethod")}
          data={METHOD_OPTIONS.map((method) => ({ value: method, label: method }))}
          value={filters.method ?? null}
          onChange={(value) => setFilters({ ...filters, method: value ?? undefined })}
        />
        <Select
          size="xs"
          w={140}
          clearable
          aria-label={t("auditWorkbench.filterAuthSource")}
          placeholder={t("auditWorkbench.filterAuthSource")}
          data={AUTH_SOURCE_OPTIONS.map((source) => ({
            value: source,
            label: authSourceText(source, t),
          }))}
          value={filters.authSource ?? null}
          onChange={(value) => setFilters({ ...filters, authSource: value ?? undefined })}
        />
        <MultiSelect
          size="xs"
          w={128}
          aria-label={t("auditWorkbench.filterCategory")}
          placeholder={t("auditWorkbench.filterCategory")}
          data={CATEGORY_VALUES.map((value) => ({
            value,
            label: t(CATEGORY_LABEL_KEYS[value] ?? value),
          }))}
          value={filters.category}
          clearable
          onChange={(values) => setFilters({ ...filters, category: values as AuditCategory[] })}
        />
        <MultiSelect
          size="xs"
          w={112}
          aria-label={t("auditWorkbench.filterResult")}
          placeholder={t("auditWorkbench.filterResult")}
          data={(["success", "failure", "pending", "unknown"] as AuditResult[]).map((value) => ({
            value,
            label: t(resultLabelKey(value)),
          }))}
          value={filters.result}
          clearable
          onChange={(values) => setFilters({ ...filters, result: values as AuditResult[] })}
        />
        <MultiSelect
          size="xs"
          w={128}
          aria-label={t("auditWorkbench.filterActor")}
          placeholder={t("auditWorkbench.filterActor")}
          data={actorOptions}
          value={filters.actor ? [filters.actor] : []}
          clearable
          searchable
          onChange={(values) => setFilters({ ...filters, actor: values[values.length - 1] })}
        />
        <SegmentedControl
          size="xs"
          aria-label={t("auditWorkbench.filterAttention")}
          value={filters.attention ?? "all"}
          onChange={(value) =>
            setFilters({
              ...filters,
              attention: value === "all" ? undefined : (value as AuditAttentionFilter),
            })
          }
          data={[
            { value: "all", label: t("auditWorkbench.filterAttentionAll") },
            { value: "pending", label: t("auditWorkbench.filterAttentionPending") },
            { value: "acknowledged", label: t("auditWorkbench.filterAttentionAcknowledged") },
          ]}
        />
        <Button
          size="xs"
          variant="subtle"
          color="gray"
          disabled={!hasFilters && !model.searchMode}
          onClick={() => {
            model.setSearch("");
            model.setDraft("");
            setIpDraft("");
            setFilters({ category: [], result: [] });
          }}
        >
          {t("auditWorkbench.searchClear")}
        </Button>
      </Group>
    </>
  );

  return (
    <OpsSection
      title={t("auditWorkbench.recordsTitle")}
      meta={t("auditWorkbench.totalCount", { total: model.pagination.totalCount })}
      actions={
        <SegmentedControl
          size="xs"
          aria-label={t("auditWorkbench.rangeLabel")}
          data={RANGE_OPTIONS}
          value={model.range}
          onChange={(value) => model.setRange(value as AuditRange)}
        />
      }
      bodyPadding={0}
      style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
      bodyStyle={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
    >
      {/* 筛选工具条：第一行关键字 + 动作/邮箱/IP，第二行方法与认证方式等 */}
      <Box
        px="md"
        py="xs"
        style={{
          borderBottom: "1px solid var(--mantine-color-default-border)",
          flexShrink: 0,
        }}
      >
        <Group gap="xs" wrap="wrap">
          <TextInput
            flex={1}
            miw={220}
            size="xs"
            leftSection={<IconSearch size={14} />}
            placeholder={t("auditWorkbench.searchPlaceholder")}
            value={model.draft}
            onChange={(e) => model.setDraft(e.currentTarget.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") model.setSearch(model.draft);
            }}
            aria-label={t("auditWorkbench.searchPlaceholder")}
          />
          <Button size="xs" style={{ flexShrink: 0 }} onClick={() => model.setSearch(model.draft)}>
            {t("auditWorkbench.searchAction")}
          </Button>
          {/* 窄屏：高级筛选默认收起。8 个筛选控件全展开会把 390×844 的整个视口占满，
              记录表被挤到折叠线下（实测表格完全不可见）；桌面端保持常显。 */}
          {isNarrow ? (
            <Button
              size="xs"
              variant="light"
              style={{ flexShrink: 0 }}
              aria-expanded={moreOpen}
              leftSection={moreOpen ? <IconChevronUp size={14} /> : <IconChevronDown size={14} />}
              onClick={() => setMoreOpen((open) => !open)}
            >
              {moreOpen
                ? t("auditWorkbench.filtersLess", { defaultValue: "收起筛选" })
                : t("auditWorkbench.filtersMore", { defaultValue: "更多筛选" })}
            </Button>
          ) : null}
          {isNarrow ? null : advancedFilters}
        </Group>
      </Box>

      {isNarrow ? (
        <Drawer
          opened={moreOpen}
          onClose={() => setMoreOpen(false)}
          position="bottom"
          size="lg"
          title={t("auditWorkbench.moreFiltersTitle", { defaultValue: "筛选条件" })}
        >
          {advancedFilters}
          <Button mt="md" fullWidth onClick={() => setMoreOpen(false)}>
            {t("common.confirm", { defaultValue: "完成" })}
          </Button>
        </Drawer>
      ) : null}

      <Box data-testid="audit-records-scroll" style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
        {model.searchMode ? (
          <Text size="xs" c="dimmed" px="md" pt="xs">
            {t("auditWorkbench.searchModeHint", { query: model.search })}
          </Text>
        ) : null}

        {records.loading && records.items.length === 0 ? (
          <Text size="sm" c="dimmed" py="lg" px="md">
            {t("auditWorkbench.loading")}
          </Text>
        ) : records.items.length === 0 ? (
          <Text size="sm" c="dimmed" py="lg" px="md">
            {model.searchMode
              ? t("auditWorkbench.investigateNoMatch", {
                  query: model.search,
                  loaded: model.pagination.totalCount,
                })
              : t("auditWorkbench.emptyRecords")}
          </Text>
        ) : (
          <Table layout="fixed" highlightOnHover verticalSpacing={8} horizontalSpacing="md">
            <Table.Thead>
              <Table.Tr>
                <Table.Th w={isNarrow ? 78 : 150} style={STICKY_HEADER}>
                  {t("auditWorkbench.colTime")}
                </Table.Th>
                {isNarrow ? null : (
                  <Table.Th w={190} style={STICKY_HEADER}>
                    {t("auditWorkbench.colActor")}
                  </Table.Th>
                )}
                <Table.Th style={STICKY_HEADER}>{t("auditWorkbench.colAction")}</Table.Th>
                <Table.Th w={76} style={STICKY_HEADER}>
                  {t("auditWorkbench.colStatus")}
                </Table.Th>
                {isNarrow ? null : (
                  <Table.Th w={88} style={STICKY_HEADER}>
                    {t("auditWorkbench.colDuration")}
                  </Table.Th>
                )}
                {isNarrow ? null : (
                  <Table.Th w={132} style={STICKY_HEADER}>
                    {t("auditWorkbench.colClientIp")}
                  </Table.Th>
                )}
                <Table.Th w={76} style={STICKY_HEADER}>
                  {t("auditWorkbench.colOperation")}
                </Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {records.items.map((event) => (
                <EventRow
                  key={event.eventId}
                  event={event}
                  expanded={expandedEvent === event.eventId}
                  detail={eventDetails[event.eventId]}
                  onToggle={() => void expandEvent(event.eventId)}
                  onInvestigate={onInvestigate}
                  onOpenAttention={onOpenAttention}
                />
              ))}
            </Table.Tbody>
          </Table>
        )}
      </Box>

      {/* 分页栏：固定在列表底部，不随记录滚动 */}
      <Group
        justify="space-between"
        align="center"
        px="md"
        py="xs"
        wrap="nowrap"
        style={{
          borderTop: "1px solid var(--mantine-color-default-border)",
          flexShrink: 0,
        }}
      >
        <Select
          size="xs"
          w={104}
          aria-label={t("auditWorkbench.pageSizeLabel")}
          data={AUDIT_PAGE_SIZES.map((value) => ({
            value,
            label: `${value} / ${t("auditWorkbench.pageUnit")}`,
          }))}
          value={String(model.pagination.pageSize)}
          onChange={(value) => model.pagination.setPageSize(Number(value ?? AUDIT_PAGE_SIZE))}
          allowDeselect={false}
        />
        <Pagination
          size="sm"
          total={model.pagination.totalPages}
          value={model.pagination.page}
          onChange={(next) => model.pagination.goToPage(next)}
          withEdges
        />
      </Group>

      {/* 窄屏详情抽屉：覆盖内联详情行的窄屏布局缺陷（表头 4 列与详情 7 列错位、sticky 表头遮挡、锁死视口滚不到底）。
          桌面走内联详情行（见 EventRow），此处仅在窄屏挂载，jsdom 桌面测试不受影响。 */}
      {isNarrow ? (
        <Drawer
          opened={Boolean(expandedEvent)}
          onClose={() => {
            // 复用 expandEvent 的「同 id 再点一次收起」语义：关闭抽屉即收起当前展开项，不新增状态。
            if (expandedEvent) void expandEvent(expandedEvent);
          }}
          position="bottom"
          size="lg"
          title={t("auditWorkbench.detailDrawerTitle", { defaultValue: "事件详情" })}
        >
          {expandedRecord ? (
            <EventDetail
              detail={eventDetails[expandedRecord.eventId]}
              event={expandedRecord}
              onInvestigate={onInvestigate}
              onOpenAttention={onOpenAttention}
            />
          ) : null}
        </Drawer>
      ) : null}
    </OpsSection>
  );
}

function EventRow({
  event,
  expanded,
  detail,
  onToggle,
  onInvestigate,
  onOpenAttention,
}: {
  event: AuditEvent;
  expanded: boolean;
  detail: AuditEventDetail | "loading" | undefined;
  onToggle: () => void;
  onInvestigate: (keyword: string) => void;
  onOpenAttention: (attentionId: string) => void;
}) {
  const { t } = useTranslation();
  // 窄屏裁列：只留 时间（时刻）+ 动作 + 结果 + 操作，操作者/耗时/IP 并入动作副文本。
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;
  const isHigh = event.severity === "high" || event.severity === "critical";
  const http = event.http as { method?: string; path?: string; statusCode?: number } | undefined;
  const targetText = asTextLabel(event);
  const email = actorEmail(event.actor);
  const actorName = actorText(event.actor, t);

  return (
    <>
      <Table.Tr
        onClick={onToggle}
        onKeyDown={(keyEvent) => {
          if (keyEvent.key === "Enter" || keyEvent.key === " ") {
            keyEvent.preventDefault();
            onToggle();
          }
        }}
        role="button"
        tabIndex={0}
        aria-expanded={expanded}
        aria-label={t("auditWorkbench.rowLabel", { action: actionLabel(event.action, t) })}
        title={isHigh ? t("auditWorkbench.severityHigh") : undefined}
        style={{ cursor: "pointer" }}
      >
        <Table.Td c="dimmed" style={{ fontVariantNumeric: "tabular-nums" }}>
          {/* 窄屏只留时刻：日期在分区标题的时间范围内已表达，全量时间戳会把主列挤没。 */}
          <Text size="xs">
            {isNarrow ? formatClock(event.occurredAt) : formatFullTime(event.occurredAt)}
          </Text>
        </Table.Td>
        {isNarrow ? null : (
          <Table.Td>
            <Text size="xs" truncate title={email}>
              {email}
            </Text>
            <Text size="xs" c="dimmed" truncate>
              {actorName}
            </Text>
          </Table.Td>
        )}
        <Table.Td>
          <Group gap={6} wrap="nowrap">
            <Text size="sm" truncate style={{ minWidth: 0 }}>
              {actionLabel(event.action, t)}
            </Text>
            {isHigh ? (
              <Badge size="xs" variant="light" color="red" style={{ flexShrink: 0 }}>
                {t("auditWorkbench.severityHigh")}
              </Badge>
            ) : null}
          </Group>
          <Text
            size="xs"
            c="dimmed"
            truncate
            title={http?.path ? `${requestMethod(http.method)} ${http.path}` : targetText}
          >
            {http?.path ? `${requestMethod(http.method)} ${http.path}` : targetText}
          </Text>
          {isNarrow ? (
            <Text size="xs" c="dimmed" truncate title={`${email} · ${actorName}`}>
              {email} · {formatDuration(event.durationMs)} · {event.clientIp ?? "—"}
            </Text>
          ) : null}
        </Table.Td>
        <Table.Td>
          <Badge size="sm" variant="light" color={statusTone(http?.statusCode)}>
            {http?.statusCode ?? t("auditWorkbench.statusUnknown")}
          </Badge>
        </Table.Td>
        {isNarrow ? null : (
          <Table.Td c="dimmed" style={{ fontVariantNumeric: "tabular-nums" }}>
            <Text size="xs">{formatDuration(event.durationMs)}</Text>
          </Table.Td>
        )}
        {isNarrow ? null : (
          <Table.Td c="dimmed">
            <Text size="xs" style={{ fontVariantNumeric: "tabular-nums" }}>
              {event.clientIp ?? "—"}
            </Text>
          </Table.Td>
        )}
        <Table.Td>
          <Button
            size={isNarrow ? "xs" : "compact-xs"}
            variant="subtle"
            leftSection={<IconEye size={14} />}
            onClick={(clickEvent) => {
              clickEvent.stopPropagation();
              onToggle();
            }}
          >
            {t("auditWorkbench.detailAction")}
          </Button>
        </Table.Td>
      </Table.Tr>
      {/* 窄屏详情改用抽屉（见 RecordsStream 主体），内联详情行仅在桌面渲染，避免 4 列表头与 7 列详情错位 + sticky 表头遮挡 + 锁死视口滚不到底。 */}
      {expanded && !isNarrow ? (
        <Table.Tr>
          <Table.Td colSpan={7} p={0}>
            <EventDetail
              detail={detail}
              event={event}
              onInvestigate={onInvestigate}
              onOpenAttention={onOpenAttention}
            />
          </Table.Td>
        </Table.Tr>
      ) : null}
    </>
  );
}

/** 目标文本：优先路由模板，其次目标 label。 */
function asTextLabel(event: AuditEvent): string {
  const http = event.http as { path?: string } | undefined;
  if (http?.path) return http.path;
  const target = event.target as { label?: string; repository?: string } | undefined;
  return target?.label ?? target?.repository ?? "—";
}

function EventDetail({
  detail,
  event,
  onInvestigate,
  onOpenAttention,
}: {
  detail: AuditEventDetail | "loading" | undefined;
  event: AuditEvent;
  onInvestigate: (keyword: string) => void;
  onOpenAttention: (attentionId: string) => void;
}) {
  const { t } = useTranslation();
  const detailData = detail && detail !== "loading" ? detail : null;
  const attentionId = event.attention?.attentionId;
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
  const actor = event.actor as { displayName?: string; authSource?: string } | undefined;
  const email = actorEmail(event.actor);
  const details = (detailData?.details ?? {}) as {
    resultSummary?: string;
    errorClass?: string;
    affectedCount?: number;
  };

  return (
    <Box p="sm" m="sm" style={{ background: "var(--mantine-color-gray-0)", borderRadius: 8 }}>
      {/* 头部：状态码 + 动作 + 方法 + 路径 */}
      <Group gap="xs" wrap="wrap" mb="xs">
        <Badge size="sm" variant="light" color={statusTone(http?.statusCode)}>
          {http?.statusCode ?? "—"} {t(RESULT_LABEL_KEYS[event.result] ?? event.result)}
        </Badge>
        <Text size="sm" fw={600}>
          {actionLabel(event.action, t)}
        </Text>
        <Badge size="xs" variant="default">
          {requestMethod(http?.method)}
        </Badge>
        <Text size="xs" style={{ fontFamily: "monospace", overflowWrap: "anywhere" }}>
          {http?.path ?? "—"}
        </Text>
      </Group>

      {/* 字段网格 */}
      <OpsDetailGrid
        items={[
          {
            label: t("auditWorkbench.detailTime"),
            value: formatFullTime(event.occurredAt),
          },
          { label: t("auditWorkbench.colDuration"), value: formatDuration(event.durationMs) },
          {
            label: t("auditWorkbench.detailRequestId"),
            value: <span style={{ fontFamily: "monospace" }}>{http?.requestId ?? "—"}</span>,
          },
          {
            label: t("auditWorkbench.detailActor"),
            value: `${email} / ${actorText(event.actor, t)}`,
          },
          {
            label: t("auditWorkbench.filterAuthSource"),
            value: authSourceText(actor?.authSource, t),
          },
          { label: t("auditWorkbench.detailToken"), value: http?.tokenPreview ?? "—" },
          { label: t("auditWorkbench.colClientIp"), value: event.clientIp ?? "—" },
          { label: t("auditWorkbench.detailUserAgent"), value: http?.userAgent ?? "—" },
        ]}
      />

      {/* 结果摘要 */}
      <Box mt="xs">
        {detailData ? (
          <Text size="xs" style={{ lineHeight: 1.9 }}>
            {t("auditWorkbench.detailResultSummary", {
              summary: details.resultSummary ?? "—",
              errorClass: details.errorClass ?? "—",
              affected: details.affectedCount ?? 0,
            })}
          </Text>
        ) : (
          <Text size="xs" c="dimmed">
            {t("auditWorkbench.detailLoading")}
          </Text>
        )}
      </Box>

      {/* 请求体（已脱敏） */}
      {http?.bodyPreview ? (
        <Box mt="xs">
          <Text size="xs" c="dimmed" mb={4}>
            {t("auditWorkbench.detailRequestBody")}
          </Text>
          <Card withBorder radius="sm" padding="xs">
            <Text
              size="xs"
              style={{
                fontFamily: "monospace",
                whiteSpace: "pre-wrap",
                overflowWrap: "anywhere",
              }}
            >
              {http.bodyPreview}
            </Text>
          </Card>
        </Box>
      ) : null}

      <Group gap="xs" mt="sm">
        {attentionId ? (
          <Button
            size="compact-xs"
            variant="light"
            onClick={() => onOpenAttention(attentionId)}
            leftSection={<IconEye size={14} />}
          >
            {t("auditWorkbench.riskOpen")}
          </Button>
        ) : null}
        <Button
          size="compact-xs"
          variant="subtle"
          onClick={() => onInvestigate(asTextLabel(event))}
          leftSection={<IconSearch size={14} />}
        >
          {t("auditWorkbench.detailInvestigate")}
        </Button>
      </Group>
    </Box>
  );
}
