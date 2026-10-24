// 维护与清理页的前端守卫。
//
// 布局按备份页范式：固定视口 + Tab 切换 + 模态框，不做向下平铺。
// 重点不在"渲染出来"，而在**防护真的生效**：
//   - 未输入确认词时，确认按钮必须 disabled（第三层防护，写成装饰就等于没有）；
//   - 取消不发请求；
//   - 恢复是反向操作，不加确认词（否则误删后取回还要过三道关）。
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";

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

  it("三张清单表的列齐全（释放字节 / 耗时 / 失败原因 / 是否运行中 / 剩余保留天数）", async () => {
    // 这几列是规格点名要的，缺了不影响任何交互，所以只能靠表头断言守住——
    // 删掉任一列（或漏加 i18n 键导致表头渲染成键名）本用例转红。
    useMaintenanceHandlers();
    server.use(
      http.get("*/api/v1/maintenance/clean-tasks", () =>
        HttpResponse.json({
          items: [
            {
              ID: 9,
              Kind: "empty_dirs",
              Status: "succeeded",
              TotalItems: 3,
              ProcessedItems: 3,
              FreedBytes: 2048,
              ErrorMessage: "磁盘只读",
              CreatedAt: "2026-10-23 10:00:00",
              StartedAt: "2026-10-23 10:00:00",
              FinishedAt: "2026-10-23 10:00:02",
            },
          ],
          total: 1,
          page: 1,
          pageSize: 20,
        }),
      ),
    );
    const user = userEvent.setup();
    renderPage();

    // 清理任务：kind / 状态 / 释放字节 / 耗时 / 失败原因。
    await user.click(await screen.findByRole("tab", { name: "清理任务" }));
    const tasks = await screen.findByTestId("clean-tasks-section");
    for (const header of ["类别", "状态", "释放字节", "耗时", "失败原因"]) {
      expect(within(tasks).getByRole("columnheader", { name: header })).toBeTruthy();
    }
    // 值真的按列渲染：2 KB 释放、2 秒耗时、失败原因不再挤在时间列里。
    expect(within(tasks).getByText("2.0 KB")).toBeTruthy();
    expect(within(tasks).getByText("2.00 s")).toBeTruthy();
    expect(within(tasks).getByText("磁盘只读")).toBeTruthy();

    // 维护作业：间隔 / 最近执行 / 失败次数 / 是否运行中。
    await user.click(screen.getByRole("tab", { name: "维护作业" }));
    const jobs = await screen.findByTestId("jobs-section");
    for (const header of ["作业", "间隔", "最近执行", "失败次数", "是否运行中"]) {
      expect(within(jobs).getByRole("columnheader", { name: header })).toBeTruthy();
    }
    // running=false 的作业显示「空闲」（不猜成在跑）。
    expect(within(jobs).getByText("空闲")).toBeTruthy();

    // 回收站：kind / 原始路径 / 大小 / 移入时间 / 剩余保留天数。
    await user.click(screen.getByRole("tab", { name: "回收站" }));
    const trash = await screen.findByTestId("trash-section");
    for (const header of ["类别", "原路径", "大小", "时间", "剩余保留天数"]) {
      expect(within(trash).getByRole("columnheader", { name: header })).toBeTruthy();
    }

    // 列变多之后**不能**把最小宽度去掉：没有它时窄屏会把原路径压成一字一行，
    // 那不属于横向溢出所以任何溢出检查都发现不了。最小宽度在 → 窄屏改在容器内横向滚动。
    // 不锁具体数值：Mantine 把 `miw={720}` 落成 `calc(45rem * var(--mantine-scale))`，
    // 断言换成"px 字面量"会随主题缩放漂移，这里只要求最小宽度确实存在。
    const minWidth = window.getComputedStyle(within(trash).getByRole("table")).minWidth;
    expect(minWidth).not.toBe("");
    expect(minWidth).not.toBe("auto");
    expect(minWidth).not.toBe("0px");
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
    // 打开即可输入：焦点已经在确认词输入框上，不需要先点一下。
    expect(document.activeElement).toBe(within(confirmDialog).getByLabelText("确认词"));
    await user.type(within(confirmDialog).getByLabelText("确认词"), "清理");
    await waitFor(() => expect(submit.disabled).toBe(false));
  });

  it("确认提交后按钮进入 pending：连点只发一次请求", async () => {
    // 请求在途时确认按钮若仍可点，手抖连点会重复发出同一批不可逆请求
    // （第二次通常撞 409 或对已移走的路径再删一次）。这里让请求永不返回，
    // 断言按钮在此期间 disabled，且只发出一次请求。
    useMaintenanceHandlers();
    let executed = 0;
    server.use(
      http.post("*/api/v1/maintenance/cleanup", async () => {
        executed++;
        // 永不落定：模拟慢请求，把按钮卡在 pending 态。
        await new Promise(() => {});
        return HttpResponse.json({ taskId: 1, status: "queued" });
      }),
    );
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole("button", { name: /空分片目录/ }));
    const previewDialog = await screen.findByRole("dialog");
    await user.click(within(previewDialog).getByRole("button", { name: "移入回收站" }));
    const confirmDialog = await screen.findByRole("dialog");
    const submit = within(confirmDialog).getByRole("button", { name: "清理" }) as HTMLButtonElement;
    await user.type(within(confirmDialog).getByLabelText("确认词"), "清理");
    await waitFor(() => expect(submit.disabled).toBe(false));

    await user.click(submit);
    await waitFor(() => expect(submit.disabled).toBe(true));
    await user.click(submit);
    expect(executed).toBe(1);
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

describe("维护与清理页：刷新失败不清空列表", () => {
  // 两组用例守同一件事：网络抖动**不能**把已经拿到的数据抹掉。
  // 旧实现是 `.catch(() => setTasks([]))` / `.catch(() => setEntries([]))`，
  // 清理任务那侧后果更重——列表被清空 → hasRunning 变 false → 轮询定时器被卸载 →
  // 进行中的任务就此从界面消失，用户以为清理任务丢了（实际还在跑）。
  const RUNNING_TASK = {
    ID: 7,
    Kind: "empty_dirs",
    Status: "running",
    TotalItems: 21,
    ProcessedItems: 5,
    FreedBytes: 0,
    ErrorMessage: null,
    CreatedAt: "2026-10-23 10:00:00",
    StartedAt: "2026-10-23 10:00:01",
    FinishedAt: null,
  };

  it("清理任务轮询失败时保留列表并提示，且轮询不中断", async () => {
    // shouldAdvanceTime：让 findBy* 的真实时间等待不被假定时器冻死。
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let failing = false;
      let calls = 0;
      server.use(
        http.get("*/api/v1/maintenance/clean-tasks", () => {
          calls++;
          return failing
            ? new HttpResponse(null, { status: 500 })
            : HttpResponse.json({ items: [RUNNING_TASK], total: 1, page: 1, pageSize: 20 });
        }),
      );
      // 直连 ?tab=tasks，省掉一次点击（假定时器下不必再掺 userEvent）。
      renderWithProviders(<MaintenancePage />, {
        route: "/maintenance?tab=tasks",
        authenticated: true,
      });

      expect(await screen.findByText("empty_dirs")).toBeTruthy();

      // 后续轮询开始失败。
      failing = true;
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1500);
      });

      // 数据仍在：这是本用例的核心——清空列表的实现会在这里转红。
      expect(screen.getByText("empty_dirs")).toBeTruthy();
      expect(document.querySelector('[data-task-status="running"]')).toBeTruthy();
      // 失败如实提示，不静默吞掉。
      expect(await screen.findByText(/仍在展示上次数据/)).toBeTruthy();

      // 轮询没有因为一次失败而停：再推一个间隔，请求数继续增长。
      const before = calls;
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1500);
      });
      expect(calls).toBeGreaterThan(before);
    } finally {
      vi.useRealTimers();
    }
  });

  it("回收站回拉失败时保留条目，不显示成「回收站是空的」", async () => {
    // 回收站没有「刷新」按钮，回拉发生在操作之后的 load()（这里走恢复）。
    // 若 catch 里仍写 setEntries([])，恢复成功后一次失败的列表刷新会把整张表抹成空态，
    // 用户会以为条目凭空消失——本用例在那种实现下转红。
    let failing = false;
    server.use(
      http.get("*/api/v1/maintenance/trash", () => {
        if (failing) return new HttpResponse(null, { status: 500 });
        return HttpResponse.json({
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
        });
      }),
      http.post("*/api/v1/maintenance/trash/restore", () => HttpResponse.json({ restored: 1 })),
    );
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole("tab", { name: "回收站" }));
    expect(await screen.findByText("/data/blobs/0a/ea")).toBeTruthy();

    // 恢复成功后回拉，此时列表接口开始失败。
    failing = true;
    await user.click(screen.getByRole("button", { name: "恢复" }));

    expect(await screen.findByText(/仍在展示上次数据/)).toBeTruthy();
    expect(screen.getByText("/data/blobs/0a/ea")).toBeTruthy();
    expect(screen.queryByText("回收站是空的")).toBeNull();
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
