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
});
