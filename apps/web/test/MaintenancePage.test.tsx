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
import i18n from "../src/i18n";
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

    // 外层 Tab 都在。回收站是唯一入口（内含「制品」与「存储清理」两个分区）。
    expect(await screen.findByRole("tab", { name: "存储体检" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "回收站" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Maven 包清理" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Maven 版本清理" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "清理任务" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "维护作业" })).toBeTruthy();
    // 合并后不应再有第二个「资产回收站」入口。
    expect(screen.queryByRole("tab", { name: "资产回收站" })).toBeNull();

    // 默认 Tab 是体检：显示事实条（空分片目录 21），且**不**同时渲染回收站表格。
    await waitFor(() => expect(screen.getByText("21")).toBeTruthy());
    expect(screen.queryByText("/data/blobs/0a/ea")).toBeNull();
  });

  it("体检页的「最近清理活动」列出最近几轮结果，且能跳到清理任务 Tab", async () => {
    // 这块是为了让体检页回答「上次清是什么时候、清掉了没有」——
    // 只给 KPI 数字时用户无法判断"该不该现在再清一次"。
    useMaintenanceHandlers();
    server.use(
      http.get("*/api/v1/maintenance/clean-tasks", () =>
        HttpResponse.json({
          items: [
            {
              ID: 11,
              Kind: "empty_dirs",
              Status: "succeeded",
              TotalItems: 21,
              ProcessedItems: 21,
              FreedBytes: 0,
              ErrorMessage: null,
              CreatedAt: "2026-10-23 10:00:00",
              StartedAt: "2026-10-23 10:00:00",
              FinishedAt: "2026-10-23 10:00:01",
            },
            {
              ID: 10,
              Kind: "stale_uploads",
              Status: "failed",
              TotalItems: 5,
              ProcessedItems: 2,
              FreedBytes: 0,
              ErrorMessage: "磁盘只读",
              CreatedAt: "2026-10-23 09:00:00",
              StartedAt: "2026-10-23 09:00:00",
              FinishedAt: "2026-10-23 09:00:01",
            },
          ],
          total: 2,
          page: 1,
          pageSize: 4,
        }),
      ),
    );
    const user = userEvent.setup();
    renderPage();

    const panel = await screen.findByTestId("recent-cleanup-panel");
    // 类别走 i18n 映射（后端 kind 是 snake_case，键名是 camelCase，拼不出来）。
    await waitFor(() => expect(within(panel).getByText("空分片目录")).toBeTruthy());
    expect(within(panel).getByText("中断的分片上传")).toBeTruthy();
    // 成功与失败各自的状态徽章都在——失败必须可见，否则"清过了"会被误读成"清干净了"。
    expect(within(panel).getByText("succeeded")).toBeTruthy();
    expect(within(panel).getByText("failed")).toBeTruthy();

    // 「查看全部」跳到清理任务 Tab。
    await user.click(within(panel).getByRole("button", { name: /查看全部/ }));
    await waitFor(() =>
      expect(screen.getByRole("tab", { name: "清理任务" }).getAttribute("aria-selected")).toBe(
        "true",
      ),
    );
  });

  it("清理任务 Tab 分页：请求按页发出，切页后拉第 2 页", async () => {
    // 早先固定 `listCleanTasks(1, 20)`，第 21 条起在界面上不存在。
    // 分页后还有个连带约束：翻到非第 1 页时必须**继续轮询**——否则第 1 页上
    // 正在跑的任务会被永久漏掉（`hasRunning` 只看当前页）。
    const all = Array.from({ length: 45 }, (_, i) => ({
      ID: 100 - i,
      Kind: "empty_dirs",
      Status: "succeeded",
      TotalItems: 1,
      ProcessedItems: 1,
      FreedBytes: 1024,
      ErrorMessage: null,
      CreatedAt: "2026-10-23 10:00:00",
      StartedAt: "2026-10-23 10:00:00",
      FinishedAt: "2026-10-23 10:00:01",
    }));
    const requested: number[] = [];
    useMaintenanceHandlers();
    server.use(
      http.get("*/api/v1/maintenance/clean-tasks", ({ request }) => {
        const url = new URL(request.url);
        const page = Number(url.searchParams.get("page") ?? "1");
        const pageSize = Number(url.searchParams.get("pageSize") ?? "20");
        requested.push(page);
        const start = (page - 1) * pageSize;
        return HttpResponse.json({
          items: all.slice(start, start + pageSize),
          total: all.length,
          page,
          pageSize,
        });
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<MaintenancePage />, {
      route: "/maintenance?tab=tasks",
      authenticated: true,
    });

    const section = await screen.findByTestId("clean-tasks-section");
    await waitFor(() => expect(requested).toContain(1));
    const pager = within(section).getByTestId("clean-tasks-pagination");
    await user.click(within(pager).getByRole("button", { name: "2" }));
    await waitFor(() => expect(requested).toContain(2));
  });

  it("六个 KPI 卡的 testid 各不相同（按 id 定位能取到对应那一项）", async () => {
    // 回归守卫：早先 testid 由「有无清理动作」推导（`kpi-${kind ?? "total"}`），
    // 六个指标里三个都落在同一个 `kpi-total` 上——按 id 定位拿到的永远是第一张卡，
    // 而断言照样能通过，所以只有"逐个 id 唯一且数值对得上"才拦得住。
    useMaintenanceHandlers();
    renderPage();

    const expected: Array<[string, string]> = [
      ["kpi-orphan-blobs", "0"],
      ["kpi-quarantined", "0"],
      ["kpi-empty-dirs", "21"],
      ["kpi-pre-restore", "3"],
      ["kpi-stale-uploads", "9.6 MB"],
      ["kpi-total", "17.1 MB"],
    ];
    for (const [testId, value] of expected) {
      const card = await screen.findByTestId(testId);
      expect(card.textContent ?? "").toContain(value);
    }
    // id 集合大小必须等于卡片数：重复 id 会让 queryAllByTestId 返回多张卡。
    expect(screen.queryAllByTestId("kpi-total")).toHaveLength(1);
  });

  it("可回收占比只累加有字节数的类别，空分片目录不计入", async () => {
    // 口径守卫：空分片目录是空目录、占 0 字节，若被当成一笔可回收空间累加，
    // 分子会凭空变大。
    //
    // 断言直接看**分子文案**而不是只看百分比：这里总量约 17.9MB、分子约 10MB，
    // 空分片目录的条数（21）折进去只让占比变化 0.0001%，四舍五入后仍是 56%——
    // 只断言百分比的话，口径写错也照样通过（实测：故意把 emptyDirs 加进分子，
    // 百分比断言仍绿）。分子是格式化后的 "9.6 MB"，把 21 字节加进去不会改变它，
    // 因此还要单独断言「明细里没有空分片目录这一行」，两条合起来才真正锁住口径。
    useMaintenanceHandlers();
    renderPage();

    const panel = await screen.findByTestId("reclaim-panel");
    // 手算：孤儿 0 + 隔离 0 + 回滚备份 15 B + 中断上传 10,020,101 B ≈ 9.6 MB
    expect(within(panel).getByText(/可回收 9\.6 MB/)).toBeTruthy();
    // 明细里四个有字节数的类别都在，空分片目录不出现。
    for (const key of ["orphan", "quarantine", "preRestore", "staleUploads"]) {
      expect(panel.querySelector(`[data-reclaim-category="${key}"]`)).toBeTruthy();
    }
    expect(panel.querySelector('[data-reclaim-category="emptyDirs"]')).toBeNull();
    // 空分片目录的条数绝不能出现在明细里（它只该出现在上方 KPI 卡）。
    expect(within(panel).queryByText(/21 项/)).toBeNull();
  });

  it("切换到回收站 Tab 才加载回收站内容", async () => {
    useMaintenanceHandlers();
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole("tab", { name: "回收站" }));
    // 回收站内含两个分区；存储清理条目在第二个分区里。
    await user.click(await screen.findByRole("tab", { name: /存储清理/ }));
    // keepMounted={false}：切过去才挂载，才发请求。
    await waitFor(() => expect(screen.getByText("/data/blobs/0a/ea")).toBeTruthy());
    // 占用显示为数字而不是 NaN。
    expect(document.body.textContent ?? "").not.toMatch(/NAN/i);
  });

  it("资产回收站分页：请求按页发出，计数给出当前页范围与全量", async () => {
    // 回归守卫：早先固定 `listAssetTrash(1, 100)` 且不渲染任何分页控件，
    // 第 101 条起在界面上根本不存在，而顶部仍显示真实总数。这里构造 45 条
    // （每页 20 → 3 页），断言三件事：① 请求带上了正确的 page/pageSize；
    // ② 出现了分页控件；③ 计数文案同时含当前页范围与总数（只给总数会误导）。
    const all = Array.from({ length: 45 }, (_, i) => ({
      id: i + 1,
      repositoryId: 1,
      repositoryName: "maven-hosted",
      path: `com/example/demo/1.0.${i}/demo-1.0.${i}.jar`,
      scope: "whole_version" as const,
      size: 1024,
      trashedAt: "2026-10-23 10:00:00",
    }));
    const requested: Array<{ page: number; pageSize: number }> = [];
    useMaintenanceHandlers();
    // handler 注册顺序即优先级：自定义分页 handler 必须在 `useMaintenanceHandlers`
    // 之后注册，否则会被它内置的固定响应覆盖，分页参数也就永远看不到。
    server.use(
      http.get("*/api/v1/maintenance/asset-trash", ({ request }) => {
        const url = new URL(request.url);
        const page = Number(url.searchParams.get("page") ?? "1");
        const pageSize = Number(url.searchParams.get("pageSize") ?? "20");
        requested.push({ page, pageSize });
        const start = (page - 1) * pageSize;
        return HttpResponse.json({
          items: all.slice(start, start + pageSize),
          total: all.length,
          page,
          pageSize,
          stats: { entries: all.length, bytes: all.length * 1024 },
        });
      }),
    );
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole("tab", { name: "回收站" }));
    const section = await screen.findByTestId("asset-trash-section");
    await waitFor(() => expect(within(section).getByText(/第 1–20 项/)).toBeTruthy());
    // 请求参数：page / pageSize 都真的传出去了（后端就是靠这两个参数分页的）。
    expect(requested[0]).toEqual({ page: 1, pageSize: 20 });

    // 分页控件存在，且切页后发出第 2 页请求。
    const pager = within(section).getByTestId("asset-trash-pagination");
    const page2 = within(pager).getByRole("button", { name: "2" });
    await user.click(page2);
    await waitFor(() => expect(requested.some((r) => r.page === 2)).toBe(true));
    await waitFor(() => expect(within(section).getByText(/第 21–40 项/)).toBeTruthy());
  });

  it("存储清理分页：全量计数与列表条数不再混淆，「清空」按全量计数", async () => {
    // 早先固定 `listTrash(1, 50)` 而顶部用 `stats.entries`（全表计数）：
    // 真实 60 条时显示「共 60 项」却只列 50 行，而「清空回收站」会真的删光 60 条。
    const all = Array.from({ length: 60 }, (_, i) => ({
      ID: i + 1,
      TaskID: 1,
      Kind: "empty_dirs",
      OriginalPath: `/data/blobs/aa/${i}`,
      TrashPath: `/data/trash/1/${i}`,
      IsDir: true,
      Bytes: 0,
      Status: "staged",
      CreatedAt: "2026-10-23 10:00:00",
      ResolvedAt: null,
    }));
    useMaintenanceHandlers();
    // 同上一用例：自定义 handler 必须在 `useMaintenanceHandlers` 之后注册。
    server.use(
      http.get("*/api/v1/maintenance/trash", ({ request }) => {
        const url = new URL(request.url);
        const page = Number(url.searchParams.get("page") ?? "1");
        const pageSize = Number(url.searchParams.get("pageSize") ?? "20");
        const start = (page - 1) * pageSize;
        return HttpResponse.json({
          items: all.slice(start, start + pageSize),
          total: all.length,
          page,
          pageSize,
          stats: { entries: all.length, bytes: 0 },
        });
      }),
    );
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole("tab", { name: "回收站" }));
    await user.click(await screen.findByRole("tab", { name: /存储清理/ }));
    const section = await screen.findByTestId("storage-trash-section");
    await waitFor(() => expect(within(section).getByText(/第 1–20 项/)).toBeTruthy());
    expect(within(section).getByTestId("storage-trash-pagination")).toBeTruthy();

    // 「清空回收站」的确认文案必须给出**全量** 60 条，而不是当前页 20 条。
    await user.click(within(section).getByRole("button", { name: "清空回收站" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent ?? "").toContain("60");
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
    await user.click(screen.getByRole("tab", { name: /存储清理/ }));
    const trash = await screen.findByTestId("storage-trash-section");
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

  it("确认词跟随界面语言：英文界面输英文即可通过，中文词不再生效", async () => {
    // 回归守卫：确认词早先是模块级常量 `"清理"`，切到英文后提示语、占位符、校验
    // 三者仍全部是中文——用户得对着英文界面输入中文才能确认。
    // 这里切到英文后同时断言两件事：英文词放行，且中文词**不再**放行
    // （只断言前者的话，"两种词都接受"的退化实现也能通过，而那等于防线被放宽）。
    useMaintenanceHandlers();
    await i18n.changeLanguage("en");
    try {
      const user = userEvent.setup();
      renderPage();

      await user.click(await screen.findByRole("button", { name: /Empty shard dirs/ }));
      const previewDialog = await screen.findByRole("dialog");
      await user.click(within(previewDialog).getByRole("button", { name: "Move to recycle bin" }));

      await waitFor(() => expect(screen.getAllByRole("dialog").length).toBeGreaterThan(0));
      const confirmDialog = screen.getAllByRole("dialog").at(-1) as HTMLElement;
      const submit = within(confirmDialog).getByRole("button", {
        name: "delete",
      }) as HTMLButtonElement;
      expect(submit.disabled).toBe(true);

      const input = within(confirmDialog).getByLabelText("Confirmation word");
      // 中文「清理」在英文界面不再被接受。
      await user.type(input, "清理");
      expect(submit.disabled).toBe(true);
      await user.clear(input);
      // 英文词放行，且大小写不敏感（这道门挡的是"没意识到在做什么"，不是拼写）。
      await user.type(input, "Delete");
      await waitFor(() => expect(submit.disabled).toBe(false));
    } finally {
      await i18n.changeLanguage("zh");
    }
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
    await user.click(await screen.findByRole("tab", { name: /存储清理/ }));
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
    await user.click(await screen.findByRole("tab", { name: /存储清理/ }));
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
