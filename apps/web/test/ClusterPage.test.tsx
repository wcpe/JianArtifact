// 集群页回归测试：同步历史摘要可见，详情使用模态框打开并可关闭返回。
import { screen, waitFor, waitForElementToBeRemoved } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  getClusterStatus: vi.fn().mockResolvedValue({
    nodeId: "node-a",
    peerUrl: "https://peer.example.com",
    tokenSet: true,
    enabled: true,
    watermark: 9,
    hasWatermark: true,
  }),
  getClusterSyncLogs: vi.fn().mockResolvedValue({
    items: [
      {
        id: 7,
        peerUrl: "https://peer.example.com",
        startedAt: "2026-08-17T01:00:00Z",
        success: false,
        fromSeq: 10,
        toSeq: 20,
        changes: 10,
        applied: 9,
        failed: 1,
        blobs: 2,
        entityCounts: '{"asset":8,"repository":2}',
        errorText: "一个变更应用失败",
      },
    ],
    total: 1,
  }),
  getClusterSyncLogChanges: vi.fn().mockResolvedValue({ items: [], total: 0 }),
  triggerClusterSync: vi.fn(),
}));

vi.mock("../src/api/endpoints", () => api);

import { ClusterPage } from "../src/pages/ClusterPage";
import { renderWithProviders } from "./harness";

describe("集群同步历史", () => {
  beforeEach(() => {
    api.getClusterSyncLogs.mockClear();
  });

  it("展示失败摘要并在模态框打开详情", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ClusterPage />, { route: "/cluster#history", authenticated: true });

    expect(await screen.findByText("制品8 · 仓库2")).toBeTruthy();
    expect(screen.getByText("一个变更应用失败")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "详情" }));

    expect(await screen.findByRole("dialog")).toBeTruthy();
    expect(screen.getByText("同步历史详情")).toBeTruthy();
    await user.keyboard("{Escape}");
    await waitForElementToBeRemoved(() => screen.queryByRole("dialog"));
    expect(screen.getByText("制品8 · 仓库2")).toBeTruthy();
  });

  it("同步历史摘要按正向页码请求下一页", async () => {
    const user = userEvent.setup();
    api.getClusterSyncLogs.mockResolvedValueOnce({
      items: [
        {
          id: 8,
          peerUrl: "https://peer.example.com",
          startedAt: "2026-08-17T02:00:00Z",
          success: true,
          fromSeq: 20,
          toSeq: 21,
          changes: 1,
          applied: 1,
          failed: 0,
          blobs: 0,
          entityCounts: '{"asset":1}',
        },
      ],
      total: 21,
    });
    renderWithProviders(<ClusterPage />, { route: "/cluster#history", authenticated: true });

    await user.click(await screen.findByRole("button", { name: "2" }));
    await waitFor(() => expect(api.getClusterSyncLogs).toHaveBeenLastCalledWith(20, 20));
  });
});
