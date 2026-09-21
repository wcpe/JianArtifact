// 审计工作台数据层（单列表分页 + 顶部 KPI）：筛选/时间范围/关键字/页码状态 +
// summary / events / 待确认批次计数三路读取 + 可见时静默刷新。
// 事件列表走服务端分页（offset 直达任意页），关键字与风险状态同样由服务端过滤，
// 保证「筛选 → 分页 → 统计」三端口径一致。
import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useSearchParams } from "react-router-dom";

import {
  getAuditAttentionNotifications,
  getAuditSummary,
  listAuditEvents,
  type AuditObservabilityQuery,
} from "../../api/endpoints";
import type {
  AuditCategory,
  AuditEvent,
  AuditObservabilitySummary,
  AuditResult,
} from "../../api/types";
import { useAsync, useVisibleRefresh } from "../../hooks/useAsync";
import { actorText, parseTime } from "./labels";

/** 默认每页事件数（可切换 20 / 50 / 100）。 */
export const AUDIT_PAGE_SIZE = 20;
export const AUDIT_PAGE_SIZES = ["20", "50", "100"];

// 1h 档是"聚合范围过大"时的兜底选择：事件量按量级增长，24h 窗口在活跃节点上
// 也可能触到后端聚合上限（返回 audit_query_too_large），此时一键切到最小窗口仍能看数据。
export type AuditRange = "1h" | "24h" | "7d" | "30d";
/** 风险状态筛选：待处理（未确认）/ 已确认。 */
export type AuditAttentionFilter = "pending" | "acknowledged";

export interface AuditFilters {
  category: AuditCategory[];
  result: AuditResult[];
  actor?: string;
  attention?: AuditAttentionFilter;
  /** HTTP 请求方法。 */
  method?: string;
  /** 操作名称（精确）。 */
  action?: string;
  /** 操作者邮箱（精确）。 */
  actorEmail?: string;
  /** 客户端 IP 前缀。 */
  clientIp?: string;
  /** 认证方式。 */
  authSource?: string;
}

const RANGE_MS: Record<AuditRange, number> = {
  "1h": 60 * 60_000,
  "24h": 24 * 60 * 60_000,
  "7d": 7 * 24 * 60 * 60_000,
  "30d": 30 * 24 * 60 * 60_000,
};

const RANGES: AuditRange[] = ["1h", "24h", "7d", "30d"];
const ATTENTIONS: AuditAttentionFilter[] = ["pending", "acknowledged"];

/** 从 URL 恢复筛选与分页状态（深链 / 刷新不丢视图）。 */
function readUrlState(params: URLSearchParams) {
  const rangeParam = params.get("range");
  const attentionParam = params.get("attention");
  const pageParam = Number(params.get("page"));
  const sizeParam = Number(params.get("pageSize"));
  return {
    range: RANGES.includes(rangeParam as AuditRange) ? (rangeParam as AuditRange) : "24h",
    attention: ATTENTIONS.includes(attentionParam as AuditAttentionFilter)
      ? (attentionParam as AuditAttentionFilter)
      : undefined,
    q: params.get("q") ?? "",
    page: Number.isFinite(pageParam) && pageParam > 0 ? Math.floor(pageParam) : 1,
    pageSize: AUDIT_PAGE_SIZES.includes(String(sizeParam)) ? sizeParam : AUDIT_PAGE_SIZE,
    category: params.getAll("category") as AuditCategory[],
    result: params.getAll("result") as AuditResult[],
    actor: params.get("actor") ?? undefined,
    method: params.get("method") ?? undefined,
    action: params.get("action") ?? undefined,
    actorEmail: params.get("actorEmail") ?? undefined,
    clientIp: params.get("clientIp") ?? undefined,
    authSource: params.get("authSource") ?? undefined,
  };
}

export function useAuditQuery() {
  const { t } = useTranslation();
  const [searchParams, setSearchParams] = useSearchParams();
  const initial = useMemo(() => readUrlState(searchParams), []);
  const [filters, setFilters] = useState<AuditFilters>({
    category: initial.category,
    result: initial.result,
    actor: initial.actor,
    attention: initial.attention,
    method: initial.method,
    action: initial.action,
    actorEmail: initial.actorEmail,
    clientIp: initial.clientIp,
    authSource: initial.authSource,
  });
  const [range, setRange] = useState<AuditRange>(initial.range);
  const [draft, setDraft] = useState(initial.q);
  const [search, setSearch] = useState(initial.q);
  const [page, setPage] = useState(initial.page);
  const [pageSize, setPageSize] = useState(initial.pageSize);

  const query: AuditObservabilityQuery = useMemo(() => {
    const to = new Date();
    return {
      from: new Date(to.getTime() - RANGE_MS[range]).toISOString(),
      to: to.toISOString(),
      category: filters.category.length > 0 ? filters.category : undefined,
      result: filters.result.length > 0 ? filters.result : undefined,
      actor: filters.actor,
      attention: filters.attention,
      method: filters.method,
      action: filters.action,
      actorEmail: filters.actorEmail,
      clientIp: filters.clientIp,
      authSource: filters.authSource,
      q: search.trim() || undefined,
    };
  }, [filters, range, search]);
  const queryKey = JSON.stringify(query);

  // 筛选条件变化时回到第一页（分页状态不进 queryKey，避免翻页触发重置）。
  useEffect(() => {
    setPage(1);
  }, [queryKey]);

  // 视图状态写入 URL：刷新与分享链接都能还原（replace 避免污染历史栈）。
  useEffect(() => {
    const next = new URLSearchParams(searchParams);
    const attentionId = next.get("attentionId");
    next.delete("range");
    next.delete("q");
    next.delete("attention");
    next.delete("page");
    next.delete("pageSize");
    next.delete("category");
    next.delete("result");
    next.delete("actor");
    next.delete("method");
    next.delete("action");
    next.delete("actorEmail");
    next.delete("clientIp");
    next.delete("authSource");
    if (range !== "24h") next.set("range", range);
    if (search.trim()) next.set("q", search.trim());
    if (filters.attention) next.set("attention", filters.attention);
    if (page > 1) next.set("page", String(page));
    if (pageSize !== AUDIT_PAGE_SIZE) next.set("pageSize", String(pageSize));
    for (const value of filters.category) next.append("category", value);
    for (const value of filters.result) next.append("result", value);
    if (filters.actor) next.set("actor", filters.actor);
    if (filters.method) next.set("method", filters.method);
    if (filters.action) next.set("action", filters.action);
    if (filters.actorEmail) next.set("actorEmail", filters.actorEmail);
    if (filters.clientIp) next.set("clientIp", filters.clientIp);
    if (filters.authSource) next.set("authSource", filters.authSource);
    if (attentionId) next.set("attentionId", attentionId);
    const current = searchParams.toString();
    if (next.toString() !== current) setSearchParams(next, { replace: true });
  }, [filters, page, pageSize, range, search, searchParams, setSearchParams]);

  const summary = useAsync<AuditObservabilitySummary>(() => getAuditSummary(query), [queryKey], {
    cacheKey: `audit:wb:summary:${queryKey}`,
  });
  const events = useAsync(
    () =>
      listAuditEvents({
        ...query,
        offset: (page - 1) * pageSize,
        limit: pageSize,
      }),
    [queryKey, page, pageSize],
    { cacheKey: `audit:wb:events:${queryKey}:${page}:${pageSize}` },
  );
  // 待确认批次数（页眉同口径：缺省请求 = 未确认批次）。
  const pendingNotifications = useAsync(() => getAuditAttentionNotifications(), [], {
    cacheKey: "audit:wb:pending-notifications",
  });

  // 首屏返回精确总数；翻页返回 -1（后端不再重复全量计数，属 30 天卡死修复）——
  // 保留首屏已知值，避免翻页后分页器与「共 N 条」被 -1 覆盖。
  const [knownTotal, setKnownTotal] = useState<number | null>(null);
  useEffect(() => {
    const value = events.data?.totalCount;
    if (value !== undefined && value >= 0) setKnownTotal(value);
  }, [events.data]);
  const totalCount = knownTotal ?? 0;
  const totalPages = Math.max(1, Math.ceil(totalCount / pageSize));

  /** offset 直达目标页：无需顺序回放游标。 */
  const goToPage = useCallback(
    (target: number) => {
      if (target < 1 || target > totalPages) return;
      setPage(target);
    },
    [totalPages],
  );

  const changePageSize = useCallback((next: number) => {
    setPageSize(next);
    setPage(1);
  }, []);

  const reloadAll = useCallback(() => {
    summary.reload();
    events.reload();
    pendingNotifications.reload();
  }, [events.reload, pendingNotifications.reload, summary.reload]);
  // 仅页面可见时静默刷新（默认 60s），替代旧实现 5s×3 接口的无条件轮询。
  useVisibleRefresh(reloadAll);

  const items = events.data?.items ?? [];
  const searchMode = search.trim().length > 0;

  /** 操作者邮箱候选（当前页观测值，供邮箱下拉使用）。 */
  const emailFacets = useMemo(() => {
    const seen = new Set<string>();
    for (const event of items) {
      const email = (event.actor as { email?: string }).email;
      if (email) seen.add(email);
    }
    return [...seen].sort();
  }, [items]);

  const actorFacets = useMemo(() => {
    const counts = new Map<string, number>();
    for (const event of items) {
      const name = actorText(event.actor, t);
      counts.set(name, (counts.get(name) ?? 0) + 1);
    }
    return [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 6);
  }, [items, t]);

  // 快照过期（attention_stale）：事件列表读取报出即提示整页刷新。
  const snapshotStale = events.error?.code === "attention_stale";
  /** 待确认批次数（页眉同一口径）。 */
  const pendingAttentionCount = pendingNotifications.data?.total ?? 0;
  /** 当前页最近一次事件时间（KPI 提示）。 */
  const latestEventAt = useMemo(
    () =>
      [...items].sort((a, b) => parseTime(b.occurredAt) - parseTime(a.occurredAt))[0]?.occurredAt,
    [items],
  );

  return {
    filters,
    setFilters,
    range,
    setRange,
    draft,
    setDraft,
    search,
    setSearch,
    searchMode,
    query,
    queryKey,
    summary,
    snapshotStale,
    pendingAttentionCount,
    latestEventAt,
    records: {
      items,
      loading: events.loading,
    },
    pagination: {
      page,
      pageSize,
      totalPages,
      totalCount,
      hasExactTotal: knownTotal !== null,
      goToPage,
      setPageSize: changePageSize,
      loading: false,
    },
    actorFacets,
    emailFacets,
    reloadAll,
  };
}

export type AuditWorkbenchModel = ReturnType<typeof useAuditQuery>;
export type { AuditEvent };
