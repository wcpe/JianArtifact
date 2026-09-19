import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  getMigration: vi.fn(),
  getMigrationReport: vi.fn(),
}));

vi.mock("../src/api/endpoints", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../src/api/endpoints")>()),
  getMigration: api.getMigration,
  getMigrationReport: api.getMigrationReport,
}));

import { MigrationDetailPage } from "../src/pages/MigrationDetailPage";
import { formatUtcToLocal } from "../src/lib/timeFormat";
import { renderWithProviders } from "./harness";

const task = {
  id: 1,
  status: "planned",
  sourceType: "online_rest",
  conflictPolicy: "skip",
  createdAt: "2026-08-26T00:00:00Z",
  updatedAt: "2026-08-26T00:00:00Z",
};

function renderDetail() {
  return renderWithProviders(
    <Routes>
      <Route path="/migrations/:id" element={<MigrationDetailPage />} />
    </Routes>,
    { route: "/migrations/1", authenticated: true },
  );
}

beforeEach(() => {
  api.getMigration.mockReset();
  api.getMigrationReport.mockReset();
});

describe("迁移详情读取状态", () => {
  it("任务读取失败进入可重试错误态，不会永久停留在加载中", async () => {
    api.getMigration.mockRejectedValueOnce(new Error("迁移任务读取失败"));
    api.getMigrationReport.mockRejectedValueOnce(new Error("报告读取失败"));
    renderDetail();

    expect((await screen.findByTestId("state-error")).textContent).toContain("迁移任务读取失败");
    expect(screen.queryByTestId("state-loading")).toBeNull();

    api.getMigration.mockResolvedValueOnce(task);
    api.getMigrationReport.mockResolvedValueOnce({
      taskId: 1,
      status: "planned",
      sourceType: "online_rest",
      conflictPolicy: "skip",
      totals: { copied: 0, skipped: 0, failed: 0 },
    });
    await userEvent.setup().click(screen.getByRole("button", { name: "重试" }));

    expect(await screen.findByText("详情 #1")).toBeTruthy();
  });

  it("生命周期时间按浏览器本地时区展示，不直接把后端 UTC 原串渲染给用户", async () => {
    api.getMigration.mockResolvedValueOnce(task);
    api.getMigrationReport.mockResolvedValueOnce({
      taskId: 1,
      status: "planned",
      sourceType: "online_rest",
      conflictPolicy: "skip",
      totals: { copied: 0, skipped: 0, failed: 0 },
    });
    renderDetail();

    await screen.findByText("详情 #1");
    // 后端给的是带 Z 的 RFC3339，直接渲染会让用户看到 UTC；必须过 lib/timeFormat 的入口。
    expect(screen.getAllByText(formatUtcToLocal(task.createdAt)).length).toBeGreaterThan(0);
    expect(screen.queryAllByText(task.createdAt)).toHaveLength(0);
  });
});
