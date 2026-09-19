// 审计聚合规则：稳定操作标识和时间桶共同决定关联范围。
// 分桶按**宿主本地时区**（后端传 UTC，展示跟随浏览器），故期望值一律用本地 getter 拼装，
// 保证任意时区下 CI 稳定；另附「非 UTC 宿主下不得按 UTC 分桶」的显式回归。
import { describe, expect, it } from "vitest";

import {
  bucketAuditGroups,
  groupAuditEvents,
  prioritizeAuditGroups,
} from "../src/components/observability/auditGrouping";
import type { AuditPreviewEvent } from "../src/mocks/observabilityPreview";

const pad = (input: number) => String(input).padStart(2, "0");

/** 用宿主本地时区拼出分桶键（与实现口径一致）。 */
function localBucketKey(iso: string, granularity: "hour" | "day"): string {
  const at = new Date(iso);
  const date = `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
  return granularity === "hour" ? `${date}T${pad(at.getHours())}` : date;
}

/** 用宿主本地时区拼出分桶标签里的日期部分。 */
function localDate(iso: string): string {
  const at = new Date(iso);
  return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
}

function event(overrides: Partial<AuditPreviewEvent> = {}): AuditPreviewEvent {
  return {
    id: "audit-1",
    source: "audit",
    occurredAt: "2026-08-26T14:10:00Z",
    timestamp: "今天 14:10",
    category: "制品变更",
    actor: "release-bot",
    authSource: "协议账户",
    action: "删除制品版本",
    target: "maven-releases / com.example:demo:1.4.2",
    result: "成功",
    ...overrides,
  };
}

describe("审计分组", () => {
  it("按稳定 operationId 关联记录，并保留其中每一条原始记录", () => {
    const groups = groupAuditEvents(
      [
        event({ id: "audit-1", operationId: "op-1" }),
        event({
          id: "replication-1",
          source: "replication",
          occurredAt: "2026-08-26T14:08:00Z",
          category: "同步复制",
          actor: "系统复制",
          action: "应用制品删除操作",
          result: "已应用",
          operationId: "op-1",
        }),
      ],
      "24h",
    );

    expect(groups).toHaveLength(1);
    expect(groups[0]).toMatchObject({ groupKey: "operation:op-1", eventCount: 2 });
    expect(groups[0].events.map((item) => item.id)).toEqual(["audit-1", "replication-1"]);
  });

  it("同一 operationId 跨时间桶时必须拆分，不能并入最新时间桶", () => {
    const groups = groupAuditEvents(
      [
        event({ id: "audit-14", operationId: "op-1", occurredAt: "2026-08-26T14:10:00Z" }),
        event({ id: "audit-15", operationId: "op-1", occurredAt: "2026-08-26T15:10:00Z" }),
      ],
      "24h",
    );

    expect(groups).toHaveLength(2);
    expect(groups.map((group) => [group.groupKey, group.bucketKey, group.eventCount])).toEqual([
      ["operation:op-1", localBucketKey("2026-08-26T15:10:00Z", "hour"), 1],
      ["operation:op-1", localBucketKey("2026-08-26T14:10:00Z", "hour"), 1],
    ]);
  });

  it("没有 operationId 时即使操作者、动作和对象相同也必须保持单条分组", () => {
    const groups = groupAuditEvents(
      [event({ id: "audit-1" }), event({ id: "audit-2", occurredAt: "2026-08-26T14:09:00Z" })],
      "24h",
    );

    expect(groups.map((group) => group.groupKey)).toEqual([
      "event:audit:audit-1",
      "event:audit:audit-2",
    ]);
    expect(groups.every((group) => group.eventCount === 1)).toBe(true);
  });

  it("按范围使用小时或自然日时间桶展示分组", () => {
    const events = [
      event({ id: "audit-1", occurredAt: "2026-08-26T14:10:00Z" }),
      event({ id: "audit-2", occurredAt: "2026-08-26T15:10:00Z" }),
    ];

    expect(bucketAuditGroups(groupAuditEvents(events, "24h")).map((bucket) => bucket.key)).toEqual([
      localBucketKey("2026-08-26T15:10:00Z", "hour"),
      localBucketKey("2026-08-26T14:10:00Z", "hour"),
    ]);
    expect(bucketAuditGroups(groupAuditEvents(events, "7d")).map((bucket) => bucket.key)).toEqual([
      localBucketKey("2026-08-26T14:10:00Z", "day"),
    ]);
    expect(bucketAuditGroups(groupAuditEvents(events, "30d")).map((bucket) => bucket.key)).toEqual([
      localBucketKey("2026-08-26T14:10:00Z", "day"),
    ]);
  });

  it("分桶键与标签按宿主本地时区计算，不按 UTC（跨日不归错桶）", () => {
    // UTC 20:00 在东八区已是次日 04:00：按 UTC 分桶会落在 26 日，本地口径必须落在 27 日。
    const [bucket] = bucketAuditGroups(
      groupAuditEvents([event({ id: "audit-1", occurredAt: "2026-08-26T20:00:00Z" })], "24h"),
    );

    expect(bucket.key).toBe(localBucketKey("2026-08-26T20:00:00Z", "hour"));
    expect(bucket.label).toContain(localDate("2026-08-26T20:00:00Z"));

    const offsetMinutes = -new Date("2026-08-26T20:00:00Z").getTimezoneOffset();
    if (offsetMinutes !== 0) {
      // 宿主非 UTC 时，按 UTC 分桶的实现会留下 "2026-08-26T20"，这条断言专门拦它。
      expect(bucket.key).not.toBe("2026-08-26T20");
    }
  });

  it("聚合批次统计成功、失败和显式影响数，已应用也计入成功", () => {
    const groups = groupAuditEvents(
      [
        event({ id: "audit-1", operationId: "op-1", affectedCount: 12 }),
        event({
          id: "replication-1",
          source: "replication",
          occurredAt: "2026-08-26T14:08:00Z",
          category: "同步复制",
          actor: "系统复制",
          action: "应用制品删除操作",
          result: "已应用",
          operationId: "op-1",
          affectedCount: 2,
        }),
        event({
          id: "audit-2",
          occurredAt: "2026-08-26T14:07:00Z",
          operationId: "op-1",
          result: "失败",
        }),
      ],
      "24h",
    );

    expect(groups[0]).toMatchObject({ successCount: 2, failureCount: 1, affectedCount: 14 });
  });

  it("关注批次将包含失败记录的操作组排在仅高风险组之前", () => {
    const groups = groupAuditEvents(
      [
        event({
          id: "risk",
          operationId: "op-risk",
          occurredAt: "2026-08-26T14:12:00Z",
          risk: true,
        }),
        event({
          id: "failure-start",
          operationId: "op-failure",
          occurredAt: "2026-08-26T14:10:00Z",
        }),
        event({
          id: "failure",
          operationId: "op-failure",
          occurredAt: "2026-08-26T14:11:00Z",
          result: "失败",
        }),
      ],
      "24h",
    );

    expect(prioritizeAuditGroups(groups).map((group) => group.groupKey)).toEqual([
      "operation:op-failure",
      "operation:op-risk",
    ]);
  });
});
