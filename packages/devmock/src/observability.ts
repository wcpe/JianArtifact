// FR-118：开发态统一审计读模型。
// 该模块只保存脱敏后的不可变事件夹具与本节点确认状态，供六个观测端点共享。
import type { components } from "./schema.gen";

type Schemas = components["schemas"];

export type AuditCategory = Schemas["AuditCategory"];
export type AuditResult = Schemas["AuditResult"];
export type AuditSeverity = Schemas["AuditSeverity"];
export type AuditEvent = Schemas["AuditEvent"];
export type AuditEventDetail = Schemas["AuditEventDetail"];
export type AuditAttention = Schemas["AuditAttention"];
export type AuditAttentionDetail = Schemas["AuditAttentionDetail"];
export type AuditAttentionNotificationList = Schemas["AuditAttentionNotificationList"];
export type AuditObservabilitySummary = Schemas["AuditObservabilitySummary"];
export type AuditEventPage = Schemas["AuditEventPage"];
export type AuditAttentionPage = Schemas["AuditAttentionPage"];
export type AuditAcknowledgement = Schemas["AuditAcknowledgement"];
export type AcknowledgeAuditAttentionResponse = Schemas["AcknowledgeAuditAttentionResponse"];

/** 通知中心查询参数（FR-117）：缺省为页眉口径（最近 24 小时未确认预览）。 */
export interface NotificationQuery {
  from?: string;
  to?: string;
  status?: Schemas["AuditNotificationStatus"];
  limit?: number;
  cursor?: number;
}

export interface AuditQuery {
  from?: string;
  to?: string;
  categories: AuditCategory[];
  results: AuditResult[];
  actor?: string;
  repository?: string;
  /** 关键字：匹配事件 ID / 动作 / 摘要 / 目标 / 操作者。 */
  q?: string;
  /** 风险状态：pending（未确认批次）/ acknowledged（已确认批次）。 */
  attention?: "pending" | "acknowledged";
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

/**
 * 审计事件的存储形状 = 契约 AuditEvent 的**平铺字段**（便于按时间/分类/结果检索）
 * + 完整 `event`（含 occurredAt）+ 脱敏 `details`。
 *
 * 平铺部分刻意不含 occurredAt（事件时间只落在 event 上，避免两处时间漂移）与
 * attention（确认状态由 attentionId 与批次读模型单独维护）。
 */
interface StoredAuditEvent extends Omit<AuditEvent, "occurredAt" | "attention"> {
  event: Omit<AuditEvent, "attention">;
  details: Schemas["AuditEventSafeDetails"];
  attentionId?: string;
}

interface Snapshot {
  ids: string[];
  query: string;
  snapshotAt: string;
}

interface ObservabilityState {
  now: Date;
  events: StoredAuditEvent[];
  acknowledgements: Map<string, AuditAcknowledgement>;
  snapshots: Map<string, Snapshot>;
  nextSnapshot: number;
}

const categories: AuditCategory[] = [
  "management_change",
  "asset_change",
  "security_event",
  "replication",
];

const severityRank: Record<AuditSeverity, number> = { normal: 0, high: 1, critical: 2 };

function eventAt(now: Date, minutesAgo: number): string {
  return new Date(now.getTime() - minutesAgo * 60_000).toISOString();
}

function storedEvent(
  now: Date,
  input: Omit<StoredAuditEvent, "event"> & {
    eventId: string;
    minutesAgo: number;
    category: AuditCategory;
    severity: AuditSeverity;
    result: AuditResult;
    action: string;
    target: Schemas["AuditTarget"];
    actor: Schemas["AuditActorSnapshot"];
    summary: string;
    operationId?: string;
    http?: Schemas["AuditHttpContext"];
    durationMs?: number;
    clientIp?: string;
  },
): StoredAuditEvent {
  const { minutesAgo, ...event } = input;
  return { ...input, event: { ...event, occurredAt: eventAt(now, minutesAgo) } };
}

function seedEvents(now: Date): StoredAuditEvent[] {
  const events: StoredAuditEvent[] = [
    storedEvent(now, {
      eventId: "audit-security-login-1",
      minutesAgo: 4,
      category: "security_event",
      severity: "critical",
      result: "failure",
      action: "auth.login_rejected",
      target: { kind: "user", label: "管理员登录" },
      actor: { displayName: "anonymous", subjectType: "anonymous", authSource: "web" },
      summary: "管理登录被安全策略拒绝",
      http: {
        method: "POST",
        path: "/api/v1/auth/login",
        statusCode: 401,
        requestId: "c1f0a4e2-7b31-4d55-9a10-2f7c8e1b0a33",
        userAgent:
          "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36 Edg/152.0.0.0",
        bodyPreview: `{
  "username": "admin",
  "password": "****",
  "remember": true
}`,
      },
      durationMs: 96,
      clientIp: "203.0.113.7",
      attentionId: "attention-security-login",
      details: {
        resultSummary: "连续失败的管理登录已被拦截",
        errorClass: "authentication_rejected",
        affectedCount: 1,
      },
    }),
    storedEvent(now, {
      eventId: "audit-asset-delete-1",
      minutesAgo: 8,
      category: "asset_change",
      severity: "high",
      result: "failure",
      action: "asset.delete",
      target: { kind: "artifact", label: "maven-releases：应用制品", repository: "maven-releases" },
      actor: {
        displayName: "admin",
        subjectType: "user",
        userId: 1,
        authSource: "jwt",
        email: "admin@sub2api.local",
      },
      summary: "制品删除事务未完成，未提交任何变更",
      http: {
        method: "DELETE",
        path: "/api/v1/artifacts/{id}",
        statusCode: 500,
        requestId: "8d42b7c9-1e05-4f28-b3aa-59d6c0f41e77",
        userAgent:
          "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36 Edg/152.0.0.0",
        tokenPreview: "Bearer eyJhbG****1dnM",
        bodyPreview: `{
  "reason": "cleanup",
  "force": true
}`,
      },
      durationMs: 1240,
      clientIp: "10.12.3.44",
      operationId: "op-audit-delete-1",
      attentionId: "attention-asset-delete",
      details: {
        resultSummary: "原子删除在预检阶段终止",
        errorClass: "transaction_rejected",
        affectedCount: 2,
      },
    }),
    storedEvent(now, {
      eventId: "audit-replication-apply-1",
      minutesAgo: 11,
      category: "replication",
      severity: "critical",
      result: "failure",
      action: "replication.apply",
      target: { kind: "cluster", label: "主备同步批次" },
      actor: { displayName: "同步服务", subjectType: "replication", authSource: "replication" },
      summary: "复制批次应用失败，等待重试",
      http: {
        method: "POST",
        path: "/api/v1/replication/apply",
        statusCode: 502,
        requestId: "5b90cc17-4a6e-42d1-8f30-7c2ad9e5b1f4",
        userAgent: "jianartifact-node/0.2.0 (replication-worker)",
        bodyPreview: `{
  "batch_id": "repl-batch-118",
  "mode": "incremental"
}`,
      },
      durationMs: 3310,
      clientIp: "192.168.1.27",
      operationId: "op-audit-replication-1",
      attentionId: "attention-replication-apply",
      details: {
        resultSummary: "复制批次未达到完成边界",
        errorClass: "replication_apply_failed",
        affectedCount: 4,
      },
    }),
    storedEvent(now, {
      eventId: "audit-management-setting-1",
      minutesAgo: 18,
      category: "management_change",
      severity: "normal",
      result: "success",
      action: "setting.update",
      target: { kind: "setting", label: "匿名访问设置" },
      actor: {
        displayName: "admin",
        subjectType: "user",
        userId: 1,
        authSource: "jwt",
        email: "admin@sub2api.local",
      },
      summary: "基础设置已更新",
      http: {
        method: "PUT",
        path: "/api/v1/settings",
        statusCode: 200,
        requestId: "a7e31d08-6c92-4b57-91fd-0e5a3c8b2d61",
        userAgent:
          "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36 Edg/152.0.0.0",
        tokenPreview: "Bearer eyJhbG****1dnM",
        bodyPreview: `{
  "anonymous_access": false,
  "public_url": "https://repo.example.com"
}`,
      },
      durationMs: 74,
      clientIp: "10.12.3.44",
      details: { resultSummary: "设置已保存", affectedCount: 1 },
    }),
  ];
  // 循环生成的演示事件保持真实分布：多数成功、失败为少数；
  // 每个高风险操作都形成风险批次（attentionId），成功批次是「待复核留痕」。
  for (let index = 0; index < 21; index += 1) {
    const failed = index % 5 === 0;
    events.push(
      storedEvent(now, {
        eventId: `audit-notification-${index + 1}`,
        minutesAgo: 30 + index,
        category: index % 2 === 0 ? "asset_change" : "replication",
        severity: failed ? (index % 10 === 0 ? "critical" : "high") : "normal",
        result: failed ? "failure" : "success",
        action: index % 2 === 0 ? "asset.operation" : "replication.apply",
        target:
          index % 2 === 0
            ? {
                kind: "artifact",
                label: `raw-hosted：风险制品 ${index + 1}`,
                repository: "raw-hosted",
              }
            : { kind: "cluster", label: `同步批次 ${index + 1}` },
        actor:
          index % 2 === 0
            ? {
                displayName: "admin",
                subjectType: "user",
                userId: 1,
                authSource: "jwt",
                email: "admin@sub2api.local",
              }
            : { displayName: "同步服务", subjectType: "replication", authSource: "system" },
        summary: failed
          ? index % 2 === 0
            ? "制品操作未通过预检，需要管理员确认"
            : "同步批次应用失败，等待重试"
          : index % 2 === 0
            ? "制品操作已完成，需管理员复核留痕"
            : "同步批次已应用，需管理员复核留痕",
        http: {
          method: index % 3 === 0 ? "DELETE" : index % 3 === 1 ? "PUT" : "POST",
          path:
            index % 2 === 0
              ? `/api/v1/artifacts/${index + 1}`
              : `/api/v1/replication/batches/${index + 1}`,
          statusCode: failed ? (index % 2 === 0 ? 500 : 502) : index % 3 === 0 ? 204 : 200,
          requestId: `f25ae17c-692d-452d-867d-6c17da0b${String(4000 + index).slice(-4)}`,
          userAgent:
            index % 2 === 0
              ? "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36 Edg/152.0.0.0"
              : "jianartifact-node/0.2.0 (replication-worker)",
          ...(index % 2 === 0 ? { tokenPreview: "Bearer eyJhbG****1dnM" } : {}),
          bodyPreview:
            index % 2 === 0
              ? `{
  "account_ids": [
    ${index + 5},
    ${index + 3},
    2,
    1
  ],
  "force": true
}`
              : `{
  "batch_id": "repl-batch-${100 + index}",
  "mode": "incremental"
}`,
        },
        durationMs: 90 + ((index * 173) % 1400),
        clientIp: ["203.0.113.7", "10.12.3.44", "192.168.1.8"][index % 3]!,
        attentionId: `attention-notification-${index + 1}`,
        details: {
          resultSummary: failed ? "风险操作保留完整审计记录" : "操作已按当前节点策略完成",
          errorClass: failed ? "mock_attention" : "none",
          affectedCount: 1,
        },
      }),
    );
  }
  return events;
}

function seed(): ObservabilityState {
  const now = new Date();
  const state: ObservabilityState = {
    now,
    events: seedEvents(now),
    acknowledgements: new Map(),
    snapshots: new Map(),
    nextSnapshot: 0,
  };
  // 演示数据预置确认状态：只有最近的风险批次保持待确认，
  // 较早的批次视为管理员已复核（与真实运维环境的积压形态一致）。
  const firstAckAt = new Date(now.getTime() - 20 * 60_000).toISOString();
  for (let index = 3; index < 21; index += 1) {
    state.acknowledgements.set(`attention-notification-${index + 1}`, {
      acknowledgedBy: {
        displayName: "admin",
        subjectType: "user",
        userId: 1,
        authSource: "web",
      },
      acknowledgedAt: new Date(
        Math.min(now.getTime() - (index + 5) * 60_000, new Date(firstAckAt).getTime()),
      ).toISOString(),
    });
  }
  return state;
}

let state = seed();

function queryKey(query: AuditQuery): string {
  return JSON.stringify({
    from: query.from ?? "",
    to: query.to ?? "",
    q: query.q ?? "",
    attention: query.attention ?? "",
    method: query.method ?? "",
    action: query.action ?? "",
    clientIp: query.clientIp ?? "",
    actorEmail: query.actorEmail ?? "",
    authSource: query.authSource ?? "",
    categories: [...query.categories].sort(),
    results: [...query.results].sort(),
    actor: query.actor ?? "",
    repository: query.repository ?? "",
  });
}

function matchesQuery(item: StoredAuditEvent, query: AuditQuery): boolean {
  const event = item.event;
  if (query.q) {
    const needle = query.q.trim().toLowerCase();
    const haystack = [
      event.eventId,
      event.action,
      event.summary ?? "",
      event.target.label,
      event.target.repository ?? "",
      event.actor.displayName,
    ]
      .join("\n")
      .toLowerCase();
    if (!haystack.includes(needle)) return false;
  }
  if (query.method && event.http?.method !== query.method) return false;
  if (query.action && event.action !== query.action) return false;
  if (query.authSource && event.actor.authSource !== query.authSource) return false;
  if (query.actorEmail && (event.actor.email ?? "") !== query.actorEmail) return false;
  if (query.clientIp && !(event.clientIp ?? "").startsWith(query.clientIp)) return false;
  if (query.attention) {
    if (!item.attentionId) return false;
    const confirmed = Boolean(acknowledgement(item.attentionId));
    if (query.attention === "pending" && confirmed) return false;
    if (query.attention === "acknowledged" && !confirmed) return false;
  }
  return (
    (!query.from || event.occurredAt >= query.from) &&
    (!query.to || event.occurredAt < query.to) &&
    (query.categories.length === 0 || query.categories.includes(event.category)) &&
    (query.results.length === 0 || query.results.includes(event.result)) &&
    (!query.actor || event.actor.displayName === query.actor) &&
    (!query.repository || event.target.repository === query.repository)
  );
}

function eventsFor(query: AuditQuery): StoredAuditEvent[] {
  const from = query.from ?? new Date(state.now.getTime() - 24 * 60 * 60_000).toISOString();
  const to = query.to ?? state.now.toISOString();
  return state.events
    .filter((item) => matchesQuery(item, { ...query, from, to }))
    .sort((left, right) => right.event.occurredAt.localeCompare(left.event.occurredAt));
}

function page<T>(
  items: T[],
  offset: number,
  limit: number,
  prefix: string,
): { items: T[]; nextCursor?: string } {
  const sliced = items.slice(offset, offset + limit);
  const nextOffset = offset + sliced.length;
  return {
    items: sliced,
    ...(nextOffset < items.length ? { nextCursor: `${prefix}${nextOffset}` } : {}),
  };
}

function acknowledgement(id: string): AuditAcknowledgement | undefined {
  return state.acknowledgements.get(id);
}

function projectEvent(item: StoredAuditEvent): AuditEvent {
  if (!item.attentionId) {
    return { ...item.event };
  }
  const confirmed = acknowledgement(item.attentionId);
  return {
    ...item.event,
    attention: {
      attentionId: item.attentionId,
      state: confirmed ? "acknowledged" : "unacknowledged",
      ...(confirmed ? { acknowledgement: confirmed } : {}),
    },
  };
}

function attentionItems(attentionId: string): StoredAuditEvent[] {
  return state.events.filter((item) => item.attentionId === attentionId);
}

function attentionPreviewForMembers(
  attentionId: string,
  members: StoredAuditEvent[],
): AuditAttention | undefined {
  if (members.length === 0) return undefined;
  const confirmed = acknowledgement(attentionId);
  const projected = members.map(projectEvent);
  const categoryCounts = categories
    .map((category) => ({
      category,
      count: projected.filter((item) => item.category === category).length,
    }))
    .filter((item) => item.count > 0);
  const failures = projected.filter((item) => item.result === "failure");
  const successes = projected.filter((item) => item.result === "success");
  const primary = [...projected].sort((left, right) => {
    const failureDifference =
      Number(right.result === "failure") - Number(left.result === "failure");
    if (failureDifference !== 0) return failureDifference;
    const severityDifference = severityRank[right.severity] - severityRank[left.severity];
    if (severityDifference !== 0) return severityDifference;
    return right.occurredAt.localeCompare(left.occurredAt);
  })[0]!;
  const severity = projected.some((item) => item.severity === "critical")
    ? "critical"
    : projected.some((item) => item.severity === "high")
      ? "high"
      : "normal";
  const result = new Set(projected.map((item) => item.result));
  const aggregateResult = result.size === 1 ? projected[0]!.result : "mixed";
  const occurredAt = projected.map((item) => item.occurredAt).sort();
  return {
    attentionId,
    state: confirmed ? "acknowledged" : "unacknowledged",
    categoryCounts,
    severity,
    firstOccurredAt: occurredAt[0]!,
    latestOccurredAt: occurredAt.at(-1)!,
    result: aggregateResult,
    action: primary.action,
    target: primary.target,
    summary: primary.summary,
    successCount: successes.length,
    failureCount: failures.length,
    affectedCount: members.reduce((total, item) => total + item.details.affectedCount, 0),
    unacknowledgedRiskEventCount: confirmed ? 0 : members.length,
    ...(primary.operationId ? { operationId: primary.operationId } : {}),
    ...(confirmed ? { firstAcknowledgement: confirmed } : {}),
    riskEventCount: members.length,
    acknowledgedRiskEventCount: confirmed ? members.length : 0,
  };
}

function attentionPreview(attentionId: string): AuditAttention | undefined {
  return attentionPreviewForMembers(attentionId, attentionItems(attentionId));
}

function newSnapshot(
  query: AuditQuery,
  items: StoredAuditEvent[],
): { snapshot: string; snapshotAt: string } {
  const snapshot = `audit-snapshot-${++state.nextSnapshot}`;
  const snapshotAt = state.now.toISOString();
  state.snapshots.set(snapshot, {
    ids: items.map((item) => item.event.eventId),
    query: queryKey(query),
    snapshotAt,
  });
  return { snapshot, snapshotAt };
}

function categoryCounts(items: StoredAuditEvent[]): Schemas["AuditCategoryCount"][] {
  return categories.map((category) => ({
    category,
    count: items.filter((item) => item.event.category === category).length,
  }));
}

function trend(items: StoredAuditEvent[], from: string, to: string): Schemas["AuditTrendPoint"][] {
  return [
    {
      from,
      to,
      totalCount: items.length,
      failureCount: items.filter((item) => item.event.result === "failure").length,
      categoryCounts: categoryCounts(items),
    },
  ];
}

/** 慢请求阈值（毫秒），与 KPI「慢请求」口径一致。 */
const SLOW_REQUEST_MS = 1000;

function summaryFor(query: AuditQuery, items: StoredAuditEvent[]): AuditObservabilitySummary {
  const { snapshot, snapshotAt } = newSnapshot(query, items);
  const failures = items.filter((item) => item.event.result === "failure");
  const successes = items.filter((item) => item.event.result === "success");
  const finished = failures.length + successes.length;
  const durations = items
    .map((item) => item.event.durationMs)
    .filter((value): value is number => typeof value === "number");
  const statuses = items
    .map((item) => item.event.http?.statusCode)
    .filter((value): value is number => typeof value === "number");
  const from = query.from ?? new Date(state.now.getTime() - 24 * 60 * 60_000).toISOString();
  const to = query.to ?? state.now.toISOString();
  return {
    snapshot,
    snapshotAt,
    totalCount: items.length,
    successCount: successes.length,
    failureCount: failures.length,
    failureRate: finished === 0 ? 0 : failures.length / finished,
    distinctActorCount: new Set(
      items
        .filter((item) => item.event.actor.subjectType === "user")
        .map((item) => item.event.actor.userId ?? item.event.actor.displayName),
    ).size,
    highRiskCount: items.filter((item) => item.event.severity !== "normal").length,
    distinctClientIpCount: new Set(
      items.map((item) => item.event.clientIp).filter((value): value is string => Boolean(value)),
    ).size,
    averageDurationMs:
      durations.length === 0
        ? 0
        : Math.round(durations.reduce((total, value) => total + value, 0) / durations.length),
    slowRequestCount: durations.filter((value) => value >= SLOW_REQUEST_MS).length,
    clientErrorCount: statuses.filter((code) => code >= 400 && code < 500).length,
    serverErrorCount: statuses.filter((code) => code >= 500).length,
    categoryCounts: categoryCounts(items),
    trend: trend(items, from, to),
  };
}

function riskAttentionPreviews(): AuditAttention[] {
  return [...new Set(state.events.flatMap((item) => (item.attentionId ? [item.attentionId] : [])))]
    .map(attentionPreview)
    .filter((item): item is AuditAttention => item !== undefined);
}

function notificationOrder(left: AuditAttention, right: AuditAttention): number {
  const failureDifference = Number(right.failureCount > 0) - Number(left.failureCount > 0);
  if (failureDifference !== 0) return failureDifference;
  const severityDifference = severityRank[right.severity] - severityRank[left.severity];
  if (severityDifference !== 0) return severityDifference;
  return right.latestOccurredAt.localeCompare(left.latestOccurredAt);
}

function notificationTimeOrder(left: AuditAttention, right: AuditAttention): number {
  const timeDifference = right.latestOccurredAt.localeCompare(left.latestOccurredAt);
  if (timeDifference !== 0) return timeDifference;
  return right.attentionId.localeCompare(left.attentionId);
}

export function resetObservabilityStore(): void {
  state = seed();
}

export const observabilityStore = {
  summary(query: AuditQuery): AuditObservabilitySummary {
    return summaryFor(query, eventsFor(query));
  },

  emptySummary(query: AuditQuery): AuditObservabilitySummary {
    return summaryFor(query, []);
  },

  events(
    query: AuditQuery,
    snapshot: string | undefined,
    offset: number,
    limit: number,
  ): AuditEventPage | "stale" {
    const current = eventsFor(query);
    const resolved = snapshot ? state.snapshots.get(snapshot) : undefined;
    if (snapshot && (!resolved || resolved.query !== queryKey(query))) return "stale";
    const source = resolved
      ? resolved.ids
          .map((id) => state.events.find((item) => item.event.eventId === id))
          .filter((item): item is StoredAuditEvent => item !== undefined)
      : current;
    const created = snapshot ? undefined : newSnapshot(query, current);
    const boundary = resolved ?? state.snapshots.get(created!.snapshot)!;
    const result = page(source.map(projectEvent), offset, limit, "audit-cursor-");
    return {
      items: result.items,
      totalCount: source.length,
      snapshot: snapshot ?? created!.snapshot,
      snapshotAt: boundary.snapshotAt,
      ...(result.nextCursor ? { nextCursor: result.nextCursor } : {}),
    };
  },

  emptyEvents(query: AuditQuery, snapshot: string | undefined): AuditEventPage | "stale" {
    const resolved = snapshot ? state.snapshots.get(snapshot) : undefined;
    if (snapshot && (!resolved || resolved.query !== queryKey(query))) return "stale";
    const created = snapshot ? undefined : newSnapshot(query, []);
    const boundary = resolved ?? state.snapshots.get(created!.snapshot)!;
    return {
      items: [],
      totalCount: 0,
      snapshot: snapshot ?? created!.snapshot,
      snapshotAt: boundary.snapshotAt,
    };
  },

  attentions(
    query: AuditQuery,
    snapshot: string | undefined,
    offset: number,
    limit: number,
  ): AuditAttentionPage | "stale" {
    const resolved = snapshot ? state.snapshots.get(snapshot) : undefined;
    if (snapshot && (!resolved || resolved.query !== queryKey(query))) return "stale";
    const current = eventsFor(query);
    const source = resolved
      ? resolved.ids
          .map((id) => state.events.find((item) => item.event.eventId === id))
          .filter((item): item is StoredAuditEvent => item !== undefined)
      : current;
    const created = snapshot ? undefined : newSnapshot(query, current);
    const boundary = resolved ?? state.snapshots.get(created!.snapshot)!;
    const groups = new Map<string, StoredAuditEvent[]>();
    source.forEach((item) => {
      if (!item.attentionId) return;
      const members = groups.get(item.attentionId) ?? [];
      members.push(item);
      groups.set(item.attentionId, members);
    });
    const items = [...groups.entries()]
      .map(([attentionId, members]) => attentionPreviewForMembers(attentionId, members))
      .filter((item): item is AuditAttention => item !== undefined)
      .sort(notificationOrder);
    const result = page(items, offset, limit, "audit-attention-cursor-");
    return {
      items: result.items,
      totalCount: items.length,
      snapshot: snapshot ?? created!.snapshot,
      snapshotAt: boundary.snapshotAt,
      ...(result.nextCursor ? { nextCursor: result.nextCursor } : {}),
    };
  },

  eventDetail(eventId: string): AuditEventDetail | undefined {
    const item = state.events.find((candidate) => candidate.event.eventId === eventId);
    return item ? { event: projectEvent(item), details: { ...item.details } } : undefined;
  },

  attention(
    attentionId: string,
    offset: number,
    limit: number,
  ): AuditAttentionDetail | "stale" | undefined {
    if (attentionId === "attention-stale") return "stale";
    const attention = attentionPreview(attentionId);
    if (!attention) return undefined;
    const result = page(
      attentionItems(attentionId).map(projectEvent),
      offset,
      limit,
      "attention-cursor-",
    );
    return {
      attention,
      items: result.items,
      totalCount: attention.riskEventCount,
      ...(result.nextCursor ? { nextCursor: result.nextCursor } : {}),
    };
  },

  acknowledge(
    attentionId: string,
    actor: Schemas["AuditActorSnapshot"],
  ): AcknowledgeAuditAttentionResponse | "stale" {
    if (attentionId === "attention-stale" || attentionItems(attentionId).length === 0)
      return "stale";
    const existing = acknowledgement(attentionId);
    const firstAcknowledgement = existing ?? {
      acknowledgedBy: actor,
      acknowledgedAt: new Date().toISOString(),
    };
    if (!existing) state.acknowledgements.set(attentionId, firstAcknowledgement);
    const totalRiskEventCount = attentionItems(attentionId).length;
    return {
      attentionId,
      firstAcknowledgement,
      totalRiskEventCount,
      newlyAcknowledgedCount: existing ? 0 : totalRiskEventCount,
      unacknowledgedRiskEventCount: 0,
    };
  },

  notifications(query: NotificationQuery = {}): AuditAttentionNotificationList {
    // FR-117：缺省请求保持页眉口径（最近 24 小时未确认、失败优先、至多 20 条）；
    // 显式筛选时按 status 口径分页并按最新时间排序。
    const explicit =
      query.from !== undefined ||
      query.to !== undefined ||
      query.status !== undefined ||
      query.limit !== undefined ||
      query.cursor !== undefined;
    const from = query.from ?? new Date(state.now.getTime() - 24 * 60 * 60_000).toISOString();
    const to = query.to ?? state.now.toISOString();
    const status = query.status ?? "unacknowledged";
    const limit = query.limit ?? 20;
    const offset = query.cursor ?? 0;
    const matched = riskAttentionPreviews()
      .filter((item) => item.latestOccurredAt >= from && item.latestOccurredAt < to)
      .filter((item) => (status === "all" ? true : item.state === status));
    const items = explicit
      ? [...matched].sort((left, right) => notificationTimeOrder(left, right))
      : [...matched].sort(notificationOrder);
    const total = items.length;
    const start = explicit ? Math.min(offset, total) : 0;
    const end = explicit ? Math.min(start + limit, total) : Math.min(total, 20);
    const page = items.slice(start, end);
    const hasMore = end < total;
    return {
      items: page,
      total,
      hasMore,
      ...(hasMore ? { nextCursor: String(end) } : {}),
      ...(explicit ? {} : { totalUnacknowledged: total }),
    };
  },

  emptyNotifications(query: NotificationQuery = {}): AuditAttentionNotificationList {
    const explicit =
      query.from !== undefined ||
      query.to !== undefined ||
      query.status !== undefined ||
      query.limit !== undefined ||
      query.cursor !== undefined;
    return {
      items: [],
      total: 0,
      hasMore: false,
      ...(explicit ? {} : { totalUnacknowledged: 0 }),
    };
  },

  appendNewRiskFixture(): string {
    const attentionId = `attention-new-risk-${state.events.length + 1}`;
    state.events.push(
      storedEvent(state.now, {
        eventId: `audit-new-risk-${state.events.length + 1}`,
        minutesAgo: 0.01,
        category: "security_event",
        severity: "critical",
        result: "failure",
        action: "security.policy_rejected",
        target: { kind: "setting", label: "发布安全策略" },
        actor: { displayName: "系统", subjectType: "system", authSource: "internal" },
        summary: "新风险操作等待管理员确认",
        attentionId,
        details: {
          resultSummary: "新事件不属于此前已确认批次",
          errorClass: "mock_new_risk",
          affectedCount: 1,
        },
      }),
    );
    return attentionId;
  },
};
