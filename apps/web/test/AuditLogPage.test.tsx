// FR-118：真实审计读模型的页面状态、批次确认和 attention_stale 回归。
import { HttpResponse, http } from "msw";
import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { AuditLogPage } from "../src/pages/AuditLogPage";
import { AppRoutes } from "../src/app/router";
import { server } from "@jianartifact/devmock/node";
import {
  releaseDevMockPendingRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { renderWithProviders } from "./harness";

const emptySummary = {
  snapshot: "empty-snapshot",
  snapshotAt: "2026-08-27T00:00:00.000Z",
  totalCount: 0,
  successCount: 0,
  failureCount: 0,
  failureRate: 0,
  distinctActorCount: 0,
  highRiskCount: 0,
  categoryCounts: [
    { category: "management_change", count: 0 },
    { category: "asset_change", count: 0 },
    { category: "security_event", count: 0 },
    { category: "replication", count: 0 },
  ],
  trend: [],
};

describe("当前节点审计中心（方案 A · 双栏工作台）", () => {
  it("始终保留筛选器与搜索框，清除筛选按钮常驻", async () => {
    const user = userEvent.setup();
    renderWithProviders(<AuditLogPage />, { route: "/audit-logs", authenticated: true });

    expect((await screen.findAllByLabelText("类别")).length).toBeGreaterThan(0);
    expect(screen.getAllByLabelText("结果")[0]).toBeTruthy();
    expect(screen.getAllByLabelText("操作者").length).toBeGreaterThan(0);
    expect(
      screen.getByLabelText("路径 / 动作 / 操作者邮箱"),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "清除筛选" })).toBeTruthy();

    await user.type(
      screen.getByLabelText("路径 / 动作 / 操作者邮箱"),
      "不存在的仓库",
    );
    expect(
      screen.getByLabelText("路径 / 动作 / 操作者邮箱"),
    ).toBeTruthy();
  });

  it("展示真实统一事件流的 KPI、筛选与列表（无 Tab 切换）", async () => {
    renderWithProviders(<AuditLogPage />, { route: "/audit-logs", authenticated: true });

    // 顶部 KPI 卡片 + OpsSection 记录表（页面标题由页眉面包屑承担，页面内不重复）。
    expect(await screen.findByText("审计事件")).toBeTruthy();
    expect(screen.getByText("待确认批次")).toBeTruthy();
    expect(await screen.findByText("审计记录")).toBeTruthy();

    // 记录流的筛选器常驻：风险状态 / 类别 / 结果 / 操作者。
    expect(screen.getByLabelText("风险状态")).toBeTruthy();
    expect(screen.getAllByLabelText("类别")[0]).toBeTruthy();
    expect(screen.getAllByLabelText("结果")[0]).toBeTruthy();
    expect(screen.getAllByLabelText("操作者").length).toBeGreaterThan(0);
  });

  it("通过 URL attentionId 打开批次详情，确认需二次确认后刷新当前节点数据", async () => {
    const user = userEvent.setup();
    renderWithProviders(<AuditLogPage />, {
      route: "/audit-logs?attentionId=attention-asset-delete",
      authenticated: true,
    });

    const dialog = await screen.findByRole("dialog", { name: "风险批次详情" });
    expect(dialog.textContent).toContain("制品删除事务未完成，未提交任何变更");

    // 防误触：第一次点击只出现确认气泡。
    await user.click(screen.getByRole("button", { name: "确认已处理" }));
    await screen.findByText("确认该批次全部风险记录已处理？确认后写入确认人与时间。");
    await user.click(screen.getByRole("button", { name: "确认", exact: true }));
    await screen.findByText(/已确认处理 \d+ 条风险记录/);
    await waitFor(() => expect(screen.queryByText("确认已处理")).toBeNull());
  });

  it("风险批次详情可继续加载完整成员", async () => {
    const user = userEvent.setup();
    server.use(
      http.get("*/api/v1/observability/audit/attention/attention-asset-delete", ({ request }) => {
        const cursor = new URL(request.url).searchParams.get("cursor");
        return HttpResponse.json(
          attentionPage(
            cursor === "next" ? ["第二个成员"] : ["第一个成员"],
            cursor ? undefined : "next",
          ),
        );
      }),
    );
    renderWithProviders(<AuditLogPage />, {
      route: "/audit-logs?attentionId=attention-asset-delete",
      authenticated: true,
    });

    expect(await screen.findByText("第一个成员")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "继续加载批次成员" }));
    expect(await screen.findByText("第二个成员")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "继续加载批次成员" })).toBeNull();
  });

  it("确认失败后保留风险抽屉与错误提示", async () => {
    const user = userEvent.setup();
    server.use(
      http.put("*/api/v1/observability/audit/attention-acknowledgements", () =>
        HttpResponse.json(
          { error: { code: "temporary_failure", message: "确认暂时失败，请重试" } },
          { status: 503 },
        ),
      ),
    );
    renderWithProviders(<AppRoutes />, {
      route: "/audit-logs?attentionId=attention-asset-delete",
      authenticated: true,
    });

    // 页眉通知入口已退役（统一收敛到审计日志）。
    expect(screen.queryByRole("button", { name: /风险通知/ })).toBeNull();
    expect(await screen.findByRole("dialog", { name: "风险批次详情" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "确认已处理" }));
    await screen.findByText("确认该批次全部风险记录已处理？确认后写入确认人与时间。");
    await user.click(screen.getByRole("button", { name: "确认", exact: true }));

    expect(await screen.findByText("确认暂时失败，请重试")).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "风险批次详情" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "确认已处理" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /风险通知/ })).toBeNull();
  });

  it("将 attention_stale 明确提示为刷新审计，而不是伪装为不存在", async () => {
    const user = userEvent.setup();
    renderWithProviders(<AuditLogPage />, {
      route: "/audit-logs?attentionId=attention-stale",
      authenticated: true,
    });

    expect(await screen.findByText("审计快照已变化，请刷新审计后重新读取。")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "刷新审计" }));
    await waitFor(() =>
      expect(screen.queryByText("审计快照已变化，请刷新审计后重新读取。")).toBeNull(),
    );
  });

  it("读取事件页遇到 attention_stale 时提示重新读取审计快照", async () => {
    server.use(
      http.get("*/api/v1/observability/audit/events", () =>
        HttpResponse.json(
          { error: { code: "attention_stale", message: "审计快照与当前筛选条件不匹配" } },
          { status: 409 },
        ),
      ),
    );

    renderWithProviders(<AuditLogPage />, { route: "/audit-logs", authenticated: true });
    expect(await screen.findByText("审计快照已变化，请刷新审计后重新读取。")).toBeTruthy();
    expect(screen.getByRole("button", { name: "刷新审计" })).toBeTruthy();
  });

  it("展示加载、失败和空态，不以预览数据替代真实结果", async () => {
    window.history.replaceState({}, "", "/audit-logs?__mock=loading");
    const { unmount } = renderWithProviders(<AuditLogPage />, {
      route: "/audit-logs",
      authenticated: true,
    });
    await waitForDevMockPendingRequest();
    expect(screen.getByText("正在加载当前节点审计…")).toBeTruthy();
    await act(async () => {
      releaseDevMockPendingRequests();
    });
    // 方案 A 数据层仅在页面可见时低频刷新（不再有 5s 轮询），一次释放后首屏数据即可达。
    expect(await screen.findByText("审计记录")).toBeTruthy();
    unmount();

    window.history.replaceState({}, "", "/audit-logs?__mock=error");
    renderWithProviders(<AuditLogPage />, {
      route: "/audit-logs",
      authenticated: true,
    });
    expect(await screen.findByText("当前节点审计暂时不可用")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
  });

  it("在服务端返回零事件时展示空态", async () => {
    server.use(
      http.get("*/api/v1/observability/audit/summary", () => HttpResponse.json(emptySummary)),
      http.get("*/api/v1/observability/audit/events", () =>
        HttpResponse.json({
          items: [],
          totalCount: 0,
          snapshot: emptySummary.snapshot,
          snapshotAt: emptySummary.snapshotAt,
        }),
      ),
      http.get("*/api/v1/observability/audit/attentions", () =>
        HttpResponse.json({
          items: [],
          totalCount: 0,
          snapshot: emptySummary.snapshot,
          snapshotAt: emptySummary.snapshotAt,
        }),
      ),
    );

    renderWithProviders(<AuditLogPage />, { route: "/audit-logs", authenticated: true });
    expect(await screen.findByText("当前筛选条件下没有审计事件")).toBeTruthy();
    expect(screen.getAllByLabelText("类别")[0]).toBeTruthy();
    expect(screen.getByRole("button", { name: "清除筛选" })).toBeTruthy();
  });
});

function attentionPage(summaries: string[], nextCursor?: string) {
  return {
    attention: {
      attentionId: "attention-asset-delete",
      state: "unacknowledged",
      categoryCounts: [{ category: "asset_change", count: 2 }],
      severity: "high",
      firstOccurredAt: "2026-08-30T10:00:00.000Z",
      latestOccurredAt: "2026-08-30T10:01:00.000Z",
      result: "failure",
      action: "asset.delete",
      target: { kind: "artifact", label: "待处理制品" },
      summary: "风险批次",
      successCount: 0,
      failureCount: 2,
      affectedCount: 2,
      unacknowledgedRiskEventCount: 2,
      riskEventCount: 2,
      acknowledgedRiskEventCount: 0,
    },
    items: summaries.map((summary, index) => ({
      eventId: `attention-member-${summary}`,
      occurredAt: `2026-08-30T10:0${index}:00.000Z`,
      action: "asset.delete",
      actor: { displayName: "admin", subjectType: "user", userId: 1, authSource: "web" },
      category: "asset_change",
      result: "failure",
      severity: "high",
      summary,
      target: { kind: "artifact", label: "待处理制品" },
    })),
    totalCount: 2,
    ...(nextCursor ? { nextCursor } : {}),
  };
}
