// 同步详情回归测试：无变更记录时仍保留分类 Tab 与分页入口。
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError } from "../src/api/client";

const api = vi.hoisted(() => ({
  getClusterSyncLogs: vi.fn().mockResolvedValue({ items: [], total: 0 }),
  getClusterSyncLogChanges: vi.fn().mockResolvedValue({ items: [], total: 0 }),
}));

vi.mock("../src/api/endpoints", () => api);

import { SyncLogChangesView } from "../src/components/repo/SyncLogChangesView";
import { renderWithProviders } from "./harness";

describe("同步历史详情", () => {
  beforeEach(() => {
    api.getClusterSyncLogs.mockReset().mockResolvedValue({ items: [], total: 0 });
    api.getClusterSyncLogChanges.mockReset().mockResolvedValue({ items: [], total: 0 });
  });

  it("无变更记录时保留分类 Tab 和分页", async () => {
    renderWithProviders(<SyncLogChangesView logId={1} hasChanges={false} />, {
      authenticated: true,
    });

    expect(await screen.findByText("全部")).toBeTruthy();
    expect(await screen.findByText("该次同步无变更记录")).toBeTruthy();
    expect(screen.getByRole("button", { name: "1" })).toBeTruthy();
    expect(api.getClusterSyncLogChanges).not.toHaveBeenCalled();
  });

  it("展示摘要与变更，并按正向页码请求下一页", async () => {
    const user = userEvent.setup();
    api.getClusterSyncLogs.mockResolvedValue({
      items: [
        {
          id: 7,
          peerUrl: "https://peer.example.com",
          startedAt: "2026-08-17T01:00:00Z",
          success: true,
          fromSeq: 10,
          toSeq: 111,
          changes: 101,
          applied: 100,
          failed: 1,
          blobs: 3,
          entityCounts: '{"asset":101}',
        },
      ],
      total: 1,
    });
    api.getClusterSyncLogChanges.mockResolvedValue({
      items: [
        {
          seq: 11,
          nodeId: "node-a",
          op: "put",
          entityType: "asset",
          entityKey: "maven-releases:com/example/app.jar",
          data: "{}",
          ts: "2026-08-17T01:00:01Z",
        },
      ],
      total: 101,
    });

    renderWithProviders(<SyncLogChangesView logId={7} />, { authenticated: true });

    expect(await screen.findByText("maven-releases:com/example/app.jar")).toBeTruthy();
    expect(screen.getByText(/变更: 101/)).toBeTruthy();
    expect(screen.getByText(/失败: 1/)).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "2" }));

    await waitFor(() =>
      expect(api.getClusterSyncLogChanges).toHaveBeenLastCalledWith(7, 100, 100, undefined),
    );
  });

  it("切到无结果分类后保留分类入口并可恢复全部结果", async () => {
    const user = userEvent.setup();
    const assetChange = {
      seq: 11,
      nodeId: "node-a",
      op: "put",
      entityType: "asset",
      entityKey: "maven-releases:com/example/app.jar",
      data: "{}",
      ts: "2026-08-17T01:00:01Z",
    };
    api.getClusterSyncLogChanges.mockImplementation(
      (_logId: number, _limit: number, _offset: number, entityType?: string) =>
        Promise.resolve(
          entityType === "repository"
            ? { items: [], total: 0 }
            : { items: [assetChange], total: 1 },
        ),
    );

    renderWithProviders(<SyncLogChangesView logId={7} />, { authenticated: true });
    expect(await screen.findByText(assetChange.entityKey)).toBeTruthy();

    await user.click(screen.getByRole("radio", { name: "仓库" }));
    expect(await screen.findByText("该次同步无变更记录")).toBeTruthy();
    expect(screen.getByRole("radio", { name: "全部" })).toBeTruthy();
    await waitFor(() =>
      expect(api.getClusterSyncLogChanges).toHaveBeenLastCalledWith(7, 100, 0, "repository"),
    );

    await user.click(screen.getByRole("radio", { name: "全部" }));
    expect(await screen.findByText(assetChange.entityKey)).toBeTruthy();
    expect(api.getClusterSyncLogChanges).toHaveBeenLastCalledWith(7, 100, 0, undefined);
  });

  it("详情接口返回 403 时展示无权限状态", async () => {
    api.getClusterSyncLogChanges.mockRejectedValue(new ApiError("forbidden", "权限不足", 403));

    renderWithProviders(<SyncLogChangesView logId={7} />, { authenticated: true });

    expect(await screen.findByText("无访问权限")).toBeTruthy();
  });
});
