import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  getAuditLogs: vi.fn(),
  getReplicationApplyLogs: vi.fn(),
}));

vi.mock("../src/api/endpoints", () => api);

import { AuditLogPage } from "../src/pages/AuditLogPage";
import { renderWithProviders } from "./harness";

const replicationItem = {
  sourceNode: "node-a",
  sourceSeq: 7,
  peerUrl: "http://peer",
  entityType: "repository",
  entityKey: "central",
  op: "put",
  result: "applied",
  detail: "已应用",
  lastError: "",
  lastErrorAt: "",
  firstSeenAt: "2026-08-17T00:00:00Z",
  lastSeenAt: "2026-08-17T00:00:00Z",
  attemptCount: 2,
};

const managementItem = {
  id: 1,
  ts: "2026-08-17T00:00:00Z",
  actor: "admin",
  action: "asset.put",
  entityKey: "k1",
  repo: "central",
  result: "ok",
  ip: "127.0.0.1",
};

// FR-101 分页布局回归：分页条须与表格滚动区同处纵向 flex 容器，
// 否则表格会全部展开把分页条挤出固定高度可视区，导致用户看不到分页条。
// 从表格向上找到第一个 overflowY:auto 的滚动容器（只包裹表格）。
function scrollContainerOf(el: HTMLElement): HTMLElement | null {
  let node = el.parentElement;
  while (node) {
    if (getComputedStyle(node).overflowY === "auto") return node;
    node = node.parentElement;
  }
  return null;
}

// 断言分页条位于表格滚动区之外、且与滚动区共处纵向 flex 容器（稳定显示在表格下方）。
function expectPaginationBelowScrollTable() {
  const pagination = document.querySelector(".mantine-Pagination-root");
  const scrollContainer = scrollContainerOf(screen.getByRole("table"));
  expect(scrollContainer, "表格应位于内部滚动容器中").not.toBeNull();
  expect(scrollContainer!.contains(pagination), "分页条不应被滚动容器包裹/裁切").toBe(false);
  // 滚动容器的直接父级（AsyncBoundary 容器）必须是纵向 flex：滚动区占满余高、分页条固定在底部。
  const flexParent = scrollContainer!.parentElement!;
  expect(getComputedStyle(flexParent).display, "分页条所在容器应为 flex").toBe("flex");
  expect(getComputedStyle(flexParent).flexDirection, "分页条所在容器应为纵向排列").toBe("column");
  expect(flexParent.contains(pagination), "分页条应与滚动区同处该 flex 容器").toBe(true);
}

beforeEach(() => {
  api.getAuditLogs.mockReset().mockResolvedValue({ items: [], total: 0 });
  api.getReplicationApplyLogs.mockReset().mockResolvedValue({ items: [], total: 0 });
});

describe("审计日志页", () => {
  it("保留管理操作页签并切换到复制应用", async () => {
    const user = userEvent.setup();
    renderWithProviders(<AuditLogPage />, { authenticated: true });
    expect(screen.getByRole("tab", { name: "管理操作" })).toBeTruthy();
    await user.click(screen.getByRole("tab", { name: "复制应用" }));
    expect(await screen.findByText("暂无复制应用记录")).toBeTruthy();
    expect(api.getReplicationApplyLogs).toHaveBeenCalledWith({ limit: 50, offset: 0 });
  });

  it("提交真实结果、实体和来源节点筛选", async () => {
    const user = userEvent.setup();
    renderWithProviders(<AuditLogPage />, { authenticated: true });
    await user.click(screen.getByRole("tab", { name: "复制应用" }));
    await screen.findByText("暂无复制应用记录");
    await user.click(screen.getByRole("textbox", { name: "结果" }));
    await user.click(screen.getByRole("option", { name: "应用失败" }));
    await user.type(screen.getByLabelText("实体"), "asset");
    await user.type(screen.getByLabelText("来源节点"), "node-a");
    await user.click(screen.getByRole("button", { name: "筛选" }));
    await waitFor(() =>
      expect(api.getReplicationApplyLogs).toHaveBeenLastCalledWith({
        result: "failed",
        entityType: "asset",
        sourceNode: "node-a",
        limit: 50,
        offset: 0,
      }),
    );
  });

  it("按真实复制结果显示徽章颜色", async () => {
    const user = userEvent.setup();
    const results = [
      "applied",
      "lww_skipped",
      "metadata_applied_pending_blob",
      "pending_parent",
      "blob_failed",
      "skipped_permanent",
      "failed",
    ];
    api.getReplicationApplyLogs.mockResolvedValue({
      items: results.map((result, index) => ({ ...replicationItem, sourceSeq: index + 1, result })),
      total: results.length,
    });
    renderWithProviders(<AuditLogPage />, { authenticated: true });
    await user.click(screen.getByRole("tab", { name: "复制应用" }));
    await screen.findByText("failed");

    const badgeStyle = (result: string) =>
      screen.getByText(result).closest("[class*='mantine-Badge-root']")?.getAttribute("style") ??
      "";
    expect(badgeStyle("applied")).toContain(
      "--badge-color: var(--mantine-color-green-light-color)",
    );
    expect(badgeStyle("lww_skipped")).toContain(
      "--badge-color: var(--mantine-color-green-light-color)",
    );
    expect(badgeStyle("metadata_applied_pending_blob")).toContain(
      "--badge-color: var(--mantine-color-yellow-light-color)",
    );
    expect(badgeStyle("pending_parent")).toContain(
      "--badge-color: var(--mantine-color-yellow-light-color)",
    );
    expect(badgeStyle("blob_failed")).toContain(
      "--badge-color: var(--mantine-color-red-light-color)",
    );
    expect(badgeStyle("skipped_permanent")).toContain(
      "--badge-color: var(--mantine-color-red-light-color)",
    );
    expect(badgeStyle("failed")).toContain("--badge-color: var(--mantine-color-red-light-color)");
  });

  it("复制记录为空时显示专用空态", async () => {
    const user = userEvent.setup();
    renderWithProviders(<AuditLogPage />, { authenticated: true });
    await user.click(screen.getByRole("tab", { name: "复制应用" }));
    expect(await screen.findByText("暂无复制应用记录")).toBeTruthy();
    expect(screen.queryByRole("columnheader", { name: "来源节点" })).toBeNull();
  });

  it("分页请求使用 limit 和 offset", async () => {
    const user = userEvent.setup();
    api.getReplicationApplyLogs.mockResolvedValue({ items: [replicationItem], total: 51 });
    renderWithProviders(<AuditLogPage />, { authenticated: true });
    await user.click(screen.getByRole("tab", { name: "复制应用" }));
    await screen.findByText("node-a");
    await user.click(screen.getByRole("button", { name: "2" }));
    await waitFor(() =>
      expect(api.getReplicationApplyLogs).toHaveBeenLastCalledWith({
        limit: 50,
        offset: 50,
      }),
    );
  });

  it("管理操作：分页条固定在表格滚动区下方且可交互（FR-101）", async () => {
    const user = userEvent.setup();
    api.getAuditLogs.mockResolvedValue({
      items: Array.from({ length: 50 }, (_, i) => ({
        ...managementItem,
        id: i + 1,
        entityKey: `k${i + 1}`,
      })),
      total: 51,
    });
    renderWithProviders(<AuditLogPage />, { authenticated: true });
    await waitFor(() => expect(document.querySelector(".mantine-Pagination-root")).not.toBeNull());
    expectPaginationBelowScrollTable();
    // 分页条可交互：点击第 2 页以 offset=50 重新请求
    await user.click(screen.getByRole("button", { name: "2" }));
    await waitFor(() =>
      expect(api.getAuditLogs).toHaveBeenLastCalledWith(
        expect.objectContaining({ limit: 50, offset: 50 }),
      ),
    );
  });

  it("复制应用：分页条固定在表格滚动区下方且可交互（FR-101）", async () => {
    const user = userEvent.setup();
    api.getReplicationApplyLogs.mockResolvedValue({
      items: Array.from({ length: 50 }, (_, i) => ({ ...replicationItem, sourceSeq: i + 1 })),
      total: 51,
    });
    renderWithProviders(<AuditLogPage />, { authenticated: true });
    await user.click(screen.getByRole("tab", { name: "复制应用" }));
    await waitFor(() => expect(document.querySelector(".mantine-Pagination-root")).not.toBeNull());
    expectPaginationBelowScrollTable();
  });
});

describe("审计日志时间筛选", () => {
  it("提交并重置起止时间", async () => {
    const user = userEvent.setup();
    renderWithProviders(<AuditLogPage />, { authenticated: true });
    await screen.findByText("暂无审计记录");
    await user.type(screen.getByLabelText("开始时间"), "2026-08-01");
    await user.type(screen.getByLabelText("结束时间"), "2026-08-17");
    await user.click(screen.getByRole("button", { name: "筛选" }));
    await waitFor(() =>
      expect(api.getAuditLogs).toHaveBeenLastCalledWith({
        actor: undefined,
        action: undefined,
        repo: undefined,
        from: "2026-08-01T00:00:00.000Z",
        to: "2026-08-17T23:59:59.999Z",
        limit: 50,
        offset: 0,
      }),
    );
    await user.click(screen.getByRole("button", { name: "重置" }));
    await waitFor(() =>
      expect(api.getAuditLogs).toHaveBeenLastCalledWith({
        actor: undefined,
        action: undefined,
        repo: undefined,
        from: undefined,
        to: undefined,
        limit: 50,
        offset: 0,
      }),
    );
  });
});
