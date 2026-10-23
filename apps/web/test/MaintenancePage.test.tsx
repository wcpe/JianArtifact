// 维护与清理页的前端守卫（FR-160）。
//
// 重点不在"渲染出来"，而在**防护真的生效**：
//   - 未输入确认词时，确认按钮必须 disabled（这是第三层防护，写成装饰就等于没有）；
//   - 取消不发请求；
//   - 恢复是反向操作，不加确认词（否则误删后取回还要过三道关）。
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";
import { MaintenancePage } from "../src/pages/MaintenancePage";
import { renderWithProviders } from "./harness";

const HEALTH = {
  orphanBlobs: 0,
  orphanBlobBytes: 0,
  quarantinedBlobs: 0,
  quarantinedBytes: 0,
  emptyDirs: 21,
  preRestoreDirs: 3,
  preRestoreBytes: 15,
  backupUploadsBytes: 10_020_101,
  backupImportsBytes: 0,
  backupDrillBytes: 0,
  totalBytes: 17_924_795,
};

function useMaintenanceHandlers() {
  server.use(
    http.get("*/api/v1/maintenance/storage-health", () => HttpResponse.json(HEALTH)),
    http.get("*/api/v1/maintenance/clean-tasks", () =>
      HttpResponse.json({ items: [], total: 0, page: 1, pageSize: 20 }),
    ),
    http.get("*/api/v1/maintenance/trash", () =>
      HttpResponse.json({
        items: [
          {
            ID: 1,
            TaskID: 1,
            Kind: "empty_dirs",
            OriginalPath: "/data/blobs/0a/ea",
            TrashPath: "/data/trash/1/1",
            IsDir: true,
            Bytes: 0,
            Status: "staged",
            CreatedAt: "2026-10-23 10:00:00",
            ResolvedAt: null,
          },
        ],
        total: 1,
        page: 1,
        pageSize: 50,
        stats: { entries: 1, bytes: 0 },
      }),
    ),
    http.get("*/api/v1/maintenance/jobs", () => HttpResponse.json({ items: [] })),
    http.post("*/api/v1/maintenance/cleanup/preview", () =>
      HttpResponse.json({
        kind: "empty_dirs",
        items: [
          { path: "blobs/0a/ea", isDir: true, bytes: 0 },
          { path: "blobs/23/5c", isDir: true, bytes: 0 },
        ],
        itemCount: 2,
        freedBytes: 0,
      }),
    ),
  );
}

function renderPage() {
  return renderWithProviders(<MaintenancePage />, { route: "/maintenance", authenticated: true });
}

describe("维护与清理页", () => {
  it("体检数字与作业区块正常渲染", async () => {
    useMaintenanceHandlers();
    renderPage();
    expect(await screen.findByText("存储体检")).toBeTruthy();
    // 空目录数来自体检响应。
    await waitFor(() => expect(screen.getByText("21")).toBeTruthy());
    expect(screen.getByText("维护作业")).toBeTruthy();
  });

  it("预览后未输入确认词时，确认按钮必须不可用", async () => {
    useMaintenanceHandlers();
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole("button", { name: /空分片目录 · 预览清理/ }));
    // 预览清单出现，默认全选。
    const preview = await screen.findByText(/清理预览：empty_dirs/);
    expect(preview).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "移入回收站" }));
    const dialog = await screen.findByRole("dialog");
    const submit = within(dialog).getByRole("button", { name: "清理" }) as HTMLButtonElement;
    // 未输入确认词 → 必须 disabled（第三层防护真的生效，而不是提示文字）。
    expect(submit.disabled).toBe(true);

    await user.type(within(dialog).getByLabelText("确认词"), "清理");
    await waitFor(() => expect(submit.disabled).toBe(false));
  });

  it("取消不会发出清理请求", async () => {
    useMaintenanceHandlers();
    let executed = 0;
    server.use(
      http.post("*/api/v1/maintenance/cleanup", () => {
        executed++;
        return HttpResponse.json({ taskId: 1, status: "succeeded" });
      }),
    );
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole("button", { name: /空分片目录 · 预览清理/ }));
    await user.click(await screen.findByRole("button", { name: "移入回收站" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "取消" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(executed).toBe(0);
  });

  it("恢复只需一次点击，不要求确认词", async () => {
    useMaintenanceHandlers();
    let restoredIds: number[] | null = null;
    server.use(
      http.post("*/api/v1/maintenance/trash/restore", async ({ request }) => {
        const body = (await request.json()) as { ids: number[] };
        restoredIds = body.ids;
        return HttpResponse.json({ restored: 1 });
      }),
    );
    const user = userEvent.setup();
    renderPage();

    const restoreBtn = await screen.findByRole("button", { name: "恢复" });
    await user.click(restoreBtn);

    await waitFor(() => expect(restoredIds).toEqual([1]));
    // 恢复不弹确认词输入框：反向操作不应再加三道关。
    expect(screen.queryByLabelText("确认词")).toBeNull();
  });
});
