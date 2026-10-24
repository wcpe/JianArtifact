// 维护与清理页的前端守卫。
//
// 布局按备份页范式：固定视口 + Tab 切换 + 模态框，不做向下平铺。
// 重点不在"渲染出来"，而在**防护真的生效**：
//   - 未输入确认词时，确认按钮必须 disabled（第三层防护，写成装饰就等于没有）；
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
        // 字段名与真实接口一致（small-camel，后端已加 json tag）。
        stats: { entries: 1, bytes: 0 },
      }),
    ),
    // 字段名与真实接口一致：后端返回 { jobs: [...] } 而非 { items: [...] }。
    http.get("*/api/v1/maintenance/jobs", () =>
      HttpResponse.json({
        jobs: [
          {
            name: "blob-gc",
            intervalSeconds: 86400,
            running: false,
            runs: 3,
            failures: 0,
            lastStartedAt: "2026-10-23T10:00:00Z",
            lastFinishedAt: "2026-10-23T10:00:01Z",
            lastError: null,
          },
        ],
      }),
    ),
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

describe("维护与清理页：固定布局与 Tab", () => {
  it("按 Tab 分区，不再把四个区块平铺在页面上", async () => {
    useMaintenanceHandlers();
    renderPage();

    // 四个 Tab 都在。
    expect(await screen.findByRole("tab", { name: "存储体检" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "回收站" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "清理任务" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "维护作业" })).toBeTruthy();

    // 默认 Tab 是体检：显示事实条（空分片目录 21），且**不**同时渲染回收站表格。
    await waitFor(() => expect(screen.getByText("21")).toBeTruthy());
    expect(screen.queryByText("/data/blobs/0a/ea")).toBeNull();
  });

  it("切换到回收站 Tab 才加载回收站内容", async () => {
    useMaintenanceHandlers();
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole("tab", { name: "回收站" }));
    // keepMounted={false}：切过去才挂载，才发请求。
    await waitFor(() => expect(screen.getByText("/data/blobs/0a/ea")).toBeTruthy());
    // 占用显示为数字而不是 NaN。
    expect(document.body.textContent ?? "").not.toMatch(/NAN/i);
  });
});

describe("维护与清理页：三层防护", () => {
  it("预览走模态框，且未输入确认词时确认按钮不可用", async () => {
    useMaintenanceHandlers();
    const user = userEvent.setup();
    renderPage();

    // 第一层：点清理按钮 → 弹预览模态框（不再是页面内展开一块）。
    await user.click(await screen.findByRole("button", { name: /空分片目录/ }));
    const previewDialog = await screen.findByRole("dialog");
    expect(within(previewDialog).getByText(/已选 2 项/)).toBeTruthy();

    // 进入第二层：二次确认弹窗。
    await user.click(within(previewDialog).getByRole("button", { name: "移入回收站" }));
    await waitFor(() => expect(screen.getAllByRole("dialog").length).toBeGreaterThan(0));
    const confirmDialog = screen.getAllByRole("dialog").at(-1) as HTMLElement;
    const submit = within(confirmDialog).getByRole("button", { name: "清理" }) as HTMLButtonElement;

    // 第三层：未输入确认词 → disabled（真的门，不是提示文字）。
    expect(submit.disabled).toBe(true);
    await user.type(within(confirmDialog).getByLabelText("确认词"), "清理");
    await waitFor(() => expect(submit.disabled).toBe(false));
  });

  it("取消预览不发清理请求", async () => {
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

    await user.click(await screen.findByRole("button", { name: /空分片目录/ }));
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

    await user.click(await screen.findByRole("tab", { name: "回收站" }));
    const restoreBtn = await screen.findByRole("button", { name: "恢复" });
    await user.click(restoreBtn);

    await waitFor(() => expect(restoredIds).toEqual([1]));
    // 恢复不弹确认词输入框：反向操作不应再加三道关。
    expect(screen.queryByLabelText("确认词")).toBeNull();
  });
});

describe("维护与清理页：接口形状不符时不白屏", () => {
  // 回归守卫：作业接口返回 { jobs: [...] } 而不是 { items: [...] }。
  // 早期页面读 res.items.length，线上直接白屏（Cannot read properties of
  // undefined (reading 'length')），而当时的 mock 也写成 items 所以测试全绿。
  it("作业接口缺字段时其余 Tab 仍可用", async () => {
    useMaintenanceHandlers();
    server.use(http.get("*/api/v1/maintenance/jobs", () => HttpResponse.json({})));
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByText("存储体检")).toBeTruthy();
    await user.click(screen.getByRole("tab", { name: "维护作业" }));
    // 作业区块显示空态而不是崩掉。
    expect(await screen.findByText("本实例未注册任何维护作业")).toBeTruthy();
  });
});
