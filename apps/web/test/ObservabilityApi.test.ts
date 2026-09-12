// FR-118：页面只能通过 typed API 客户端读取和确认服务端签发的审计批次。
import { afterEach, describe, expect, it } from "vitest";

import { ApiError, setToken } from "../src/api/client";
import {
  acknowledgeAuditAttention,
  getAuditAttention,
  getAuditAttentionNotifications,
  getAuditEvent,
  getAuditSummary,
  listAuditEvents,
} from "../src/api/endpoints";

afterEach(() => setToken(null));

describe("统一审计 API 客户端", () => {
  it("使用服务端快照读取事件、详情和有限通知预览", async () => {
    setToken("mock.jwt.token");
    const summary = await getAuditSummary({ category: ["asset_change", "replication"] });
    const events = await listAuditEvents({
      category: ["asset_change", "replication"],
      snapshot: summary.snapshot,
      limit: 1,
    });
    expect(events.snapshot).toBe(summary.snapshot);
    expect(events.items).toHaveLength(1);
    await expect(getAuditEvent(events.items[0]!.eventId)).resolves.toEqual(
      expect.objectContaining({
        event: expect.objectContaining({ eventId: events.items[0]!.eventId }),
      }),
    );

    const notifications = await getAuditAttentionNotifications();
    expect(notifications.items.length).toBeLessThanOrEqual(20);
    expect(notifications.totalUnacknowledged).toBeGreaterThanOrEqual(notifications.items.length);
  });

  it("确认仅提交 attentionId，重复确认返回同一首个确认快照", async () => {
    setToken("mock.jwt.token");
    const notifications = await getAuditAttentionNotifications();
    const attentionId = notifications.items[0]!.attentionId;
    const first = await acknowledgeAuditAttention(attentionId);
    const repeated = await acknowledgeAuditAttention(attentionId);
    expect(repeated.firstAcknowledgement).toEqual(first.firstAcknowledgement);
    expect(repeated.newlyAcknowledgedCount).toBe(0);
    await expect(getAuditAttention(attentionId)).resolves.toEqual(
      expect.objectContaining({ attention: expect.objectContaining({ state: "acknowledged" }) }),
    );
  });

  it("快照不可重建时保留服务端 attention_stale 错误码", async () => {
    setToken("mock.jwt.token");
    await expect(getAuditAttention("attention-stale")).rejects.toMatchObject<ApiError>({
      code: "attention_stale",
      status: 409,
    });
  });
});
