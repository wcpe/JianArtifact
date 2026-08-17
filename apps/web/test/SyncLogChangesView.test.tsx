// 同步详情回归测试：无变更记录时仍保留分类 Tab 与分页入口。
import { screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  getClusterSyncLogs: vi.fn().mockResolvedValue({ items: [], total: 0 }),
  getClusterSyncLogChanges: vi.fn().mockResolvedValue({ items: [], total: 0 }),
}));

vi.mock("../src/api/endpoints", () => api);

import { SyncLogChangesView } from "../src/components/repo/SyncLogChangesView";
import { renderWithProviders } from "./harness";

describe("同步历史详情", () => {
  it("无变更记录时保留分类 Tab 和分页", async () => {
    renderWithProviders(<SyncLogChangesView logId={1} hasChanges={false} />, { authenticated: true });

    expect(await screen.findByText("全部")).toBeTruthy();
    expect(await screen.findByText("该次同步无变更记录")).toBeTruthy();
    expect(screen.getByRole("button", { name: "1" })).toBeTruthy();
    expect(api.getClusterSyncLogChanges).not.toHaveBeenCalled();
  });
});
