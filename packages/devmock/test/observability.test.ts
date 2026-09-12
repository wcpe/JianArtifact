// FR-118 / FR-122：统一审计 Mock 的端到端契约行为。
// 先覆盖管理员边界、关注批次原子确认、快照失效和通知上限，再实现处理器。
import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { server } from "../src/node";
import {
  DEV_MOCK_ROUTE_HEADER,
  DEV_MOCK_SCENARIO_HEADER,
  resetDevMockScenario,
} from "../src/scenario";
import { observabilityStore } from "../src/observability";
import { MOCK_TOKEN, resetStore } from "../src/store";

const admin = { Authorization: `Bearer ${MOCK_TOKEN}` };
const secondAdmin = { Authorization: "Bearer mock.jwt.token:admin-2" };
const user = { Authorization: "Bearer mock.jwt.token:user" };

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  resetDevMockScenario();
  resetStore();
});
afterAll(() => server.close());

describe("统一审计观测 Mock", () => {
  it("六个新端点从同一审计夹具读取，旧审计端点保持兼容", async () => {
    const summary = await fetch("http://localhost/api/v1/observability/audit/summary", {
      headers: admin,
    });
    expect(summary.status).toBe(200);
    const summaryBody = (await summary.json()) as { snapshot: string; totalCount: number };
    expect(summaryBody.snapshot).toEqual(expect.any(String));
    expect(summaryBody.totalCount).toBeGreaterThan(0);

    const events = await fetch(
      `http://localhost/api/v1/observability/audit/events?snapshot=${encodeURIComponent(summaryBody.snapshot)}`,
      { headers: admin },
    );
    expect(events.status).toBe(200);
    const eventsBody = (await events.json()) as { items: { eventId: string }[] };
    expect(eventsBody.items.length).toBeGreaterThan(0);

    const attentions = await fetch(
      `http://localhost/api/v1/observability/audit/attentions?snapshot=${encodeURIComponent(summaryBody.snapshot)}&limit=1`,
      { headers: admin },
    );
    expect(attentions.status).toBe(200);
    const attentionsBody = (await attentions.json()) as {
      items: { attentionId: string }[];
      totalCount: number;
      nextCursor?: string;
      snapshot: string;
    };
    expect(attentionsBody).toMatchObject({ snapshot: summaryBody.snapshot });
    expect(attentionsBody.items).toHaveLength(1);
    expect(attentionsBody.totalCount).toBeGreaterThan(1);
    expect(attentionsBody.nextCursor).toEqual(expect.any(String));

    const detail = await fetch(
      `http://localhost/api/v1/observability/audit/events/${encodeURIComponent(eventsBody.items[0]!.eventId)}`,
      { headers: admin },
    );
    expect(detail.status).toBe(200);

    const notification = await fetch("http://localhost/api/v1/observability/audit/notifications", {
      headers: admin,
    });
    expect(notification.status).toBe(200);
    const notificationBody = (await notification.json()) as {
      items: { attentionId: string }[];
    };
    expect(notificationBody.items.length).toBeGreaterThan(0);

    const attention = await fetch(
      `http://localhost/api/v1/observability/audit/attention/${encodeURIComponent(notificationBody.items[0]!.attentionId)}`,
      { headers: admin },
    );
    expect(attention.status).toBe(200);

    const legacy = await fetch("http://localhost/api/v1/audit-logs?limit=1&offset=0", {
      headers: admin,
    });
    expect(legacy.status).toBe(200);
  });

  it("仅管理员可读取，响应不泄露原始诊断或凭据字段", async () => {
    for (const headers of [{}, user]) {
      const response = await fetch("http://localhost/api/v1/observability/audit/summary", {
        headers,
      });
      expect(response.status).toBe(headers === user ? 403 : 401);
    }

    const response = await fetch("http://localhost/api/v1/observability/audit/events?limit=1", {
      headers: admin,
    });
    const serialized = JSON.stringify(await response.json());
    // 仍然禁止：内部地址、明文凭据、原始诊断字段。
    for (const forbidden of ["peerUrl", '"password": "admin"', "secret", "127.0.0.1"]) {
      expect(serialized).not.toContain(forbidden);
    }
    // 排障字段（按管理员审计视图策略返回）应当存在，且凭据必须已掩码。
    for (const required of ["requestId", "userAgent", "clientIp", "bodyPreview"]) {
      expect(serialized).toContain(required);
    }
    expect(serialized).toContain("****");
  });

  it("__mock=empty 为审计读接口返回完整且可继续分页的空读模型", async () => {
    const empty = {
      ...admin,
      [DEV_MOCK_SCENARIO_HEADER]: "empty",
      [DEV_MOCK_ROUTE_HEADER]: "/audit-logs",
    };
    const summary = await fetch("http://localhost/api/v1/observability/audit/summary", {
      headers: empty,
    });
    expect(summary.status).toBe(200);
    const summaryBody = (await summary.json()) as {
      snapshot: string;
      totalCount: number;
      successCount: number;
      failureCount: number;
      highRiskCount: number;
      categoryCounts: { count: number }[];
      trend: { totalCount: number; failureCount: number; categoryCounts: { count: number }[] }[];
    };
    expect(summaryBody).toMatchObject({
      totalCount: 0,
      successCount: 0,
      failureCount: 0,
      highRiskCount: 0,
    });
    expect(summaryBody.categoryCounts.every((item) => item.count === 0)).toBe(true);
    expect(summaryBody.trend).toHaveLength(1);
    expect(summaryBody.trend[0]).toMatchObject({ totalCount: 0, failureCount: 0 });
    expect(summaryBody.trend[0]!.categoryCounts.every((item) => item.count === 0)).toBe(true);

    const events = await fetch(
      `http://localhost/api/v1/observability/audit/events?snapshot=${encodeURIComponent(summaryBody.snapshot)}`,
      { headers: empty },
    );
    expect(events.status).toBe(200);
    await expect(events.json()).resolves.toMatchObject({
      items: [],
      totalCount: 0,
      snapshot: summaryBody.snapshot,
    });

    const attention = await fetch(
      "http://localhost/api/v1/observability/audit/attention/attention-security-login",
      { headers: empty },
    );
    expect(attention.status).toBe(404);

    const notifications = await fetch("http://localhost/api/v1/observability/audit/notifications", {
      headers: empty,
    });
    await expect(notifications.json()).resolves.toEqual({
      items: [],
      total: 0,
      totalUnacknowledged: 0,
      hasMore: false,
    });
  });

  it("两个管理员并发确认同一 attentionId 时保留首个确认快照并幂等", async () => {
    const notifications = await fetch("http://localhost/api/v1/observability/audit/notifications", {
      headers: admin,
    });
    const { items } = (await notifications.json()) as { items: { attentionId: string }[] };
    const attentionId = items[0]!.attentionId;

    const acknowledge = (headers: HeadersInit) =>
      fetch("http://localhost/api/v1/observability/audit/attention-acknowledgements", {
        method: "PUT",
        headers: { "Content-Type": "application/json", ...headers },
        body: JSON.stringify({ attentionId }),
      });
    const [first, second] = await Promise.all([acknowledge(admin), acknowledge(secondAdmin)]);
    expect([first.status, second.status]).toEqual([200, 200]);
    const [firstBody, secondBody] = (await Promise.all([first.json(), second.json()])) as [
      {
        firstAcknowledgement: { acknowledgedBy: { displayName: string } };
        newlyAcknowledgedCount: number;
      },
      {
        firstAcknowledgement: { acknowledgedBy: { displayName: string } };
        newlyAcknowledgedCount: number;
      },
    ];
    expect(firstBody.firstAcknowledgement).toEqual(secondBody.firstAcknowledgement);
    expect([firstBody.newlyAcknowledgedCount, secondBody.newlyAcknowledgedCount]).toContain(0);
    expect(firstBody.firstAcknowledgement.acknowledgedBy.displayName).toBe("admin");
  });

  it("确认写入在备用节点、模拟失败和 attention 快照失效时均不改变未确认状态", async () => {
    const notification = await fetch("http://localhost/api/v1/observability/audit/notifications", {
      headers: admin,
    });
    const { items } = (await notification.json()) as { items: { attentionId: string }[] };
    const attentionId = items[0]!.attentionId;
    const body = JSON.stringify({ attentionId });

    const standby = await fetch(
      "http://localhost/api/v1/observability/audit/attention-acknowledgements",
      {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          ...admin,
          [DEV_MOCK_SCENARIO_HEADER]: "standby_read_only",
          [DEV_MOCK_ROUTE_HEADER]: "/audit-logs",
        },
        body,
      },
    );
    expect(standby.status).toBe(503);

    const failed = await fetch(
      "http://localhost/api/v1/observability/audit/attention-acknowledgements",
      {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          ...admin,
          [DEV_MOCK_SCENARIO_HEADER]: "error",
          [DEV_MOCK_ROUTE_HEADER]: "/audit-logs",
        },
        body,
      },
    );
    expect(failed.status).toBe(500);

    const staleRead = await fetch(
      "http://localhost/api/v1/observability/audit/attention/attention-stale",
      { headers: admin },
    );
    expect(staleRead.status).toBe(409);
    const staleWrite = await fetch(
      "http://localhost/api/v1/observability/audit/attention-acknowledgements",
      {
        method: "PUT",
        headers: { "Content-Type": "application/json", ...admin },
        body: JSON.stringify({ attentionId: "attention-stale" }),
      },
    );
    expect(staleWrite.status).toBe(409);

    const after = await fetch(
      `http://localhost/api/v1/observability/audit/attention/${encodeURIComponent(attentionId)}`,
      { headers: admin },
    );
    const afterBody = (await after.json()) as {
      attention: { state: string; unacknowledgedRiskEventCount: number };
    };
    expect(afterBody.attention.state).toBe("unacknowledged");
    expect(afterBody.attention.unacknowledgedRiskEventCount).toBeGreaterThan(0);
  });

  it("确认请求先遇到可重试失败后，重试成功才改变同一批次状态", async () => {
    const attentionId = "attention-security-login";
    const body = JSON.stringify({ attentionId });
    const failed = await fetch(
      "http://localhost/api/v1/observability/audit/attention-acknowledgements",
      {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          ...admin,
          [DEV_MOCK_SCENARIO_HEADER]: "error",
          [DEV_MOCK_ROUTE_HEADER]: "/audit-logs",
        },
        body,
      },
    );
    expect(failed.status).toBe(500);

    const beforeRetry = await fetch(
      `http://localhost/api/v1/observability/audit/attention/${attentionId}`,
      { headers: admin },
    );
    await expect(beforeRetry.json()).resolves.toMatchObject({
      attention: { state: "unacknowledged", unacknowledgedRiskEventCount: 1 },
    });

    const retry = await fetch(
      "http://localhost/api/v1/observability/audit/attention-acknowledgements",
      {
        method: "PUT",
        headers: { "Content-Type": "application/json", ...admin },
        body,
      },
    );
    expect(retry.status).toBe(200);
    await expect(retry.json()).resolves.toMatchObject({ newlyAcknowledgedCount: 1 });
  });

  it("快照与筛选条件不一致时返回 attention_stale，前端可要求重新读取审计", async () => {
    const summary = await fetch("http://localhost/api/v1/observability/audit/summary", {
      headers: admin,
    });
    const { snapshot } = (await summary.json()) as { snapshot: string };

    const response = await fetch(
      `http://localhost/api/v1/observability/audit/events?category=asset_change&snapshot=${encodeURIComponent(snapshot)}`,
      { headers: admin },
    );

    expect(response.status).toBe(409);
    await expect(response.json()).resolves.toMatchObject({
      error: { code: "attention_stale" },
    });
  });

  it("确认既有风险批次后，新产生的风险仍作为独立未确认批次保留", async () => {
    const acknowledgedId = "attention-security-login";
    const response = await fetch(
      "http://localhost/api/v1/observability/audit/attention-acknowledgements",
      {
        method: "PUT",
        headers: { "Content-Type": "application/json", ...admin },
        body: JSON.stringify({ attentionId: acknowledgedId }),
      },
    );
    expect(response.status).toBe(200);

    const newAttentionId = observabilityStore.appendNewRiskFixture();
    const notifications = await fetch("http://localhost/api/v1/observability/audit/notifications", {
      headers: admin,
    });
    const body = (await notifications.json()) as {
      items: { attentionId: string; state: string }[];
    };
    expect(body.items).not.toContainEqual(expect.objectContaining({ attentionId: acknowledgedId }));
    expect(body.items).toContainEqual(
      expect.objectContaining({ attentionId: newAttentionId, state: "unacknowledged" }),
    );
  });

  it("通知缺省口径返回未确认批次，limit 截断用总数和 hasMore 表示", async () => {
    const response = await fetch("http://localhost/api/v1/observability/audit/notifications", {
      headers: admin,
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      items: unknown[];
      total: number;
      totalUnacknowledged: number;
      hasMore: boolean;
      nextCursor?: string;
    };
    // 种子数据：3 个手写批次 + 3 个最近的循环批次待确认，其余已预置确认。
    expect(body.items).toHaveLength(6);
    expect(body.total).toBe(body.totalUnacknowledged);
    expect(body.total).toBe(6);
    expect(body.hasMore).toBe(false);
    expect(body.nextCursor).toBeUndefined();

    // limit 截断：hasMore + nextCursor 指向下一页。
    const paged = await fetch("http://localhost/api/v1/observability/audit/notifications?limit=2", {
      headers: admin,
    });
    const pagedBody = (await paged.json()) as {
      items: unknown[];
      hasMore: boolean;
      nextCursor?: string;
    };
    expect(pagedBody.items).toHaveLength(2);
    expect(pagedBody.hasMore).toBe(true);
    expect(pagedBody.nextCursor).toBe("2");
  });

  it("FR-117：通知中心按 status 筛选、时间排序并支持游标分页", async () => {
    const fetchNotifications = async (query = "") => {
      const response = await fetch(
        `http://localhost/api/v1/observability/audit/notifications${query}`,
        { headers: admin },
      );
      expect(response.status).toBe(200);
      return (await response.json()) as {
        items: { attentionId: string; state: string; latestOccurredAt: string }[];
        total: number;
        hasMore: boolean;
        nextCursor?: string;
        totalUnacknowledged?: number;
      };
    };

    // status=acknowledged 只含已确认批次；status=all 汇总两态且不返回页眉徽标字段。
    // 种子数据预置 18 个已确认批次（较早的复核留痕）。
    const seededAcknowledged = await fetchNotifications("?status=acknowledged");
    expect(seededAcknowledged.total).toBe(18);
    expect(seededAcknowledged.totalUnacknowledged).toBeUndefined();

    const acknowledgedId = "attention-security-login";
    const acknowledge = await fetch(
      "http://localhost/api/v1/observability/audit/attention-acknowledgements",
      {
        method: "PUT",
        headers: { "Content-Type": "application/json", ...admin },
        body: JSON.stringify({ attentionId: acknowledgedId }),
      },
    );
    expect(acknowledge.status).toBe(200);

    const acknowledged = await fetchNotifications("?status=acknowledged");
    expect(acknowledged.total).toBe(19);
    expect(acknowledged.items[0]!.state).toBe("acknowledged");

    const unacknowledged = await fetchNotifications("?status=unacknowledged");
    const all = await fetchNotifications("?status=all&limit=100");
    expect(all.total).toBe(unacknowledged.total + acknowledged.total);
    expect(all.totalUnacknowledged).toBeUndefined();

    // 显式请求时间排序：逐页 limit=1 推进，时间单调不递增，直至 hasMore=false。
    const collected: string[] = [];
    let previousTime: string | undefined;
    let cursor = "";
    for (let page = 0; page < 40; page += 1) {
      const body = await fetchNotifications(`?status=all&limit=1${cursor}`);
      if (body.items[0]) {
        if (previousTime !== undefined) {
          expect(new Date(body.items[0].latestOccurredAt).getTime()).toBeLessThanOrEqual(
            new Date(previousTime).getTime(),
          );
        }
        previousTime = body.items[0].latestOccurredAt;
        collected.push(body.items[0].attentionId);
      }
      if (!body.hasMore) {
        expect(body.nextCursor).toBeUndefined();
        break;
      }
      expect(body.nextCursor).toBeDefined();
      cursor = `&cursor=${encodeURIComponent(body.nextCursor!)}`;
    }
    expect(collected).toHaveLength(all.total);
    expect(new Set(collected).size).toBe(collected.length);

    // 非法参数映射 400。
    for (const invalid of [
      "?from=2026-08-01T00:00:00Z",
      "?status=bogus",
      "?limit=0",
      "?cursor=-1",
    ]) {
      const response = await fetch(
        `http://localhost/api/v1/observability/audit/notifications${invalid}`,
        { headers: admin },
      );
      expect(response.status).toBe(400);
    }
  });
});
