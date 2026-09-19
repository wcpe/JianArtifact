import type {
  AuditPreviewCategory,
  AuditPreviewEvent,
  PreviewRange,
} from "../../mocks/observabilityPreview";
import { AUDIT_RESULT } from "../../mocks/observabilityPreview";
import { parseUtc } from "../../lib/timeFormat";

export interface AuditPreviewGroup {
  groupKey: string;
  bucketKey: string;
  bucketLabel: string;
  events: AuditPreviewEvent[];
  eventCount: number;
  latest: AuditPreviewEvent;
  categories: AuditPreviewCategory[];
  results: AuditPreviewEvent["result"][];
  successCount: number;
  failureCount: number;
  affectedCount: number;
}

export interface AuditPreviewTimeBucket {
  key: string;
  label: string;
  groups: AuditPreviewGroup[];
  eventCount: number;
}

function eventGroupKey(event: AuditPreviewEvent) {
  return event.operationId ? `operation:${event.operationId}` : `event:${event.source}:${event.id}`;
}

/**
 * 分桶键与标签按**浏览器本地时区**计算。
 * 后端传的是 UTC，若直接 `toISOString()` 取前 13 位就会按 UTC 天/小时分桶——UTC+8 下
 * 本地 09:30 的记录会被标成「01:00–01:59」，分桶与标签双错，跨日时还会归错到前一天。
 */
function timeBucket(occurredAt: string, range: PreviewRange) {
  const at = parseUtc(occurredAt);
  if (!at) {
    return { key: occurredAt, label: occurredAt };
  }
  const pad = (input: number) => String(input).padStart(2, "0");
  const date = `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;

  if (range === "24h") {
    const hour = pad(at.getHours());
    return { key: `${date}T${hour}`, label: `${date} ${hour}:00–${hour}:59` };
  }

  return { key: date, label: date };
}

export function newestFirstAuditEvents(events: AuditPreviewEvent[]) {
  return [...events].sort((left, right) => right.occurredAt.localeCompare(left.occurredAt));
}

function isAttentionGroup(group: AuditPreviewGroup) {
  return group.failureCount > 0 || group.events.some((event) => event.risk);
}

/** 失败批次优先，其次是仅高风险批次；同一优先级内按批次最新记录排序。 */
export function prioritizeAuditGroups(groups: AuditPreviewGroup[]) {
  return groups.filter(isAttentionGroup).sort((left, right) => {
    const leftPriority = left.failureCount > 0 ? 1 : 0;
    const rightPriority = right.failureCount > 0 ? 1 : 0;
    return (
      rightPriority - leftPriority || right.latest.occurredAt.localeCompare(left.latest.occurredAt)
    );
  });
}

export function groupAuditEvents(
  events: AuditPreviewEvent[],
  range: PreviewRange,
): AuditPreviewGroup[] {
  const grouped = new Map<
    string,
    { groupKey: string; bucketKey: string; bucketLabel: string; events: AuditPreviewEvent[] }
  >();

  events.forEach((event) => {
    const groupKey = eventGroupKey(event);
    const bucket = timeBucket(event.occurredAt, range);
    const key = `${groupKey}\u0000${bucket.key}`;
    const group = grouped.get(key) ?? {
      groupKey,
      bucketKey: bucket.key,
      bucketLabel: bucket.label,
      events: [],
    };
    group.events.push(event);
    grouped.set(key, group);
  });

  return [...grouped.values()]
    .flatMap((group) => {
      const orderedEvents = newestFirstAuditEvents(group.events);
      const latest = orderedEvents[0];
      if (!latest) {
        return [];
      }

      return [
        {
          groupKey: group.groupKey,
          bucketKey: group.bucketKey,
          bucketLabel: group.bucketLabel,
          events: orderedEvents,
          eventCount: orderedEvents.length,
          latest,
          categories: [...new Set(orderedEvents.map((event) => event.category))],
          results: [...new Set(orderedEvents.map((event) => event.result))],
          successCount: orderedEvents.filter(
            (event) => event.result !== AUDIT_RESULT.failure,
          ).length,
          failureCount: orderedEvents.filter(
            (event) => event.result === AUDIT_RESULT.failure,
          ).length,
          affectedCount: orderedEvents.reduce(
            (total, event) => total + (event.affectedCount ?? 0),
            0,
          ),
        },
      ];
    })
    .sort((left, right) => right.latest.occurredAt.localeCompare(left.latest.occurredAt));
}

export function bucketAuditGroups(groups: AuditPreviewGroup[]): AuditPreviewTimeBucket[] {
  const buckets = new Map<string, AuditPreviewTimeBucket>();

  groups.forEach((group) => {
    const bucket = buckets.get(group.bucketKey) ?? {
      key: group.bucketKey,
      label: group.bucketLabel,
      groups: [],
      eventCount: 0,
    };
    bucket.groups.push(group);
    bucket.eventCount += group.eventCount;
    buckets.set(group.bucketKey, bucket);
  });

  return [...buckets.values()].sort((left, right) => right.key.localeCompare(left.key));
}
