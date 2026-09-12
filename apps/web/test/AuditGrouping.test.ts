// 审计聚合规则：稳定操作标识和时间桶共同决定关联范围。
import { describe, expect, it } from "vitest";

import {
  bucketAuditGroups,
  groupAuditEvents,
  prioritizeAuditGroups,
} from "../src/components/observability/auditGrouping";
import type { AuditPreviewEvent } from "../src/mocks/observabilityPreview";

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
      ["operation:op-1", "2026-08-26T15", 1],
      ["operation:op-1", "2026-08-26T14", 1],
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
      "2026-08-26T15",
      "2026-08-26T14",
    ]);
    expect(bucketAuditGroups(groupAuditEvents(events, "7d")).map((bucket) => bucket.key)).toEqual([
      "2026-08-26",
    ]);
    expect(bucketAuditGroups(groupAuditEvents(events, "30d")).map((bucket) => bucket.key)).toEqual([
      "2026-08-26",
    ]);
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
