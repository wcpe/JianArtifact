// 分片上传（FR-137 第三通道）：init → 逐片上传 → 进度 → complete 后导入记录 pending_restart；
// 取消（abort）会话标记 aborted；续传只补缺失片；缺片错误如实展示。
//
// 通道位置（两段式任务台）：上传不再是页面上的一张卡，也不是「导入与传输」弹窗里的一个标签页
// ——弹窗已删除，两个通道的表单**就地展开**在页面里（testid `import-inline`），入口是挂在事件表
// 区块标题行里的动作条（`backup-actions`）的两个按钮 `action-import-url` / `action-import-upload`。
//
// 重要前提：内联区与其下两个通道**常驻挂载**，收起只置 display:none。上传面板必须在挂载时
// 探测未完成的上传会话（见 ImportInline 文件头），因此本文件的 helper 不能再依赖"打开弹窗"
// 这一动作来让面板出现——面板本来就在 DOM 里，只需点按钮把它显示出来。
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { BackupsTab } from "../src/components/backup/BackupsTab";
import { ApiError, setToken } from "../src/api/client";
import * as endpoints from "../src/api/endpoints";
import { renderWithProviders } from "./harness";

// 用例间恢复所有 spy，避免替换实现泄漏到后续用例。
afterEach(() => {
  vi.restoreAllMocks();
});

function renderTab() {
  return renderWithProviders(<BackupsTab />, {
    route: "/migrations?tab=backups",
    authenticated: true,
  });
}

/**
 * 动作条里的按钮（挂在事件表区块标题行内）。
 *
 * 必须限定在 `backup-actions` 内：内联导入区的标题与按钮同名（「从 URL 导入」/「上传本机文件」），
 * 不限定就会命中错的那个（AmbiguousElement 或拿到隐藏态的那一个）。
 */
function actionButton(name: string | RegExp): HTMLElement {
  return within(screen.getByTestId("backup-actions")).getByRole("button", { name });
}

/** 等动作条渲染好（冻结状态与导入记录都回来后再点，避免与首载赛跑）。 */
async function actionButtonAsync(name: string | RegExp): Promise<HTMLElement> {
  await screen.findByTestId("backup-actions");
  return actionButton(name);
}

/**
 * 展开内联上传通道，返回 user 与选文件 input。
 *
 * 点动作条的「上传本机文件」就地展开上传表单（不再是弹窗），因此这里断言的是
 * `import-channel-upload` 可见——若实现改成默认展开 URL 通道、或把上传通道条件卸载，
 * 本 helper 会先失败，正是我们要守住的两个行为。
 */
async function openUploadTab() {
  const user = userEvent.setup();
  await user.click(await actionButtonAsync("上传包恢复"));
  expect(screen.queryByRole("dialog")).toBeNull();
  const channel = screen.getByTestId("import-channel-upload");
  expect(channel.style.display).not.toBe("none");
  expect(screen.getByTestId("import-inline").style.display).not.toBe("none");
  const input = await within(channel).findByTestId("backup-upload-input");
  return { user, channel, input };
}

/** 生成指定体积的本地文件（Node File，与 jsdom 垫片同源）。 */
function makeFile(sizeBytes: number, name = "bk-upload.tar.gz"): File {
  return new File([new Uint8Array(sizeBytes)], name, { type: "application/gzip" });
}

/**
 * 放慢每片上传，便于在 active 阶段观察进度条 / 点取消。
 * 必须先捕获原始实现再 spy：若在 mockImplementation 内调用被 spy 的函数会无限递归。
 *
 * 500ms/片（3 片 ≈ 1.5s）而非更短：取消用例要在这个窗口里完成"点取消 → 等二次确认弹窗
 * 渲染 → 再点确认"三次交互，jsdom 下每次点击耗时并不恒定，窗口过窄会偶发地在上传已完成后
 * 才发出 abort（现象是会话状态变成 completed 而非 aborted）。
 */
function slowChunks() {
  const real = endpoints.uploadBackupChunk;
  return vi
    .spyOn(endpoints, "uploadBackupChunk")
    .mockImplementation(async (id, idx, body, opts) => {
      await new Promise((r) => setTimeout(r, 500));
      return real(id, idx, body, opts);
    });
}

describe("分片上传备份包", () => {
  it("选文件→init→逐片上传→进度出现→complete 后导入记录出现且为 pending_restart", async () => {
    slowChunks();
    renderTab();
    const { user, input } = await openUploadTab();

    // 20 MiB → 按服务端 8 MiB chunkSize 切 3 片，拉长 active 阶段便于观察进度。
    const file = makeFile(20 * 1024 * 1024);
    await user.upload(input, file);

    // 进度条出现且带百分比（在 active 阶段内断言）。
    await waitFor(
      () => {
        expect(screen.getByTestId("upload-progress")).toBeTruthy();
        expect(screen.getByTestId("upload-percent").textContent).toContain("%");
      },
      { timeout: 8000 },
    );

    // 本面板给出「已提交导入 / 需重启」回执。
    await waitFor(
      () => {
        expect(screen.getByText("上传完成，已提交导入")).toBeTruthy();
      },
      { timeout: 15_000 },
    );

    // complete 触发导入：新导入记录（origin=upload）经 queued→…→pending_restart 推进。
    // 记录已并入统一事件表（不再需要"切到记录标签"），新行按事件时间直接出现在表里——
    // 上传后仍停留在展开的上传表单上，两者同屏，正是本次重构的收益。
    //
    // 必须限定在事件表内查找：complete 之后**恢复进度弹窗会自动打开**，弹窗状态行也渲染
    // 同一个 importId；不限定作用域会同时命中表格单元格与弹窗，断言因"找到多个元素"失败。
    const table = await screen.findByTestId("backup-event-table");
    const newId = await within(table).findByText(/^imp-\d{14}-\d{3}$/, {}, { timeout: 15_000 });
    const row = newId.closest("tr") as HTMLElement;
    expect(row.dataset.rowKind).toBe("import");
    await waitFor(
      () => {
        expect(within(row).getByText("待重启")).toBeTruthy();
      },
      { timeout: 15_000 },
    );
  }, 30_000);

  it("途中勾选「覆盖已有数据」后，complete 时把它透传给后端", async () => {
    // 这一条是本轮要修的核心缺陷：契约的 CompleteBackupUploadRequest 本就支持 overwrite，
    // 但界面从未暴露它，于是"往非空实例导"必然在**异步暂存阶段**失败
    // （HTTP 早返回 202，用户只看到"上传成功、随后记录变失败"，无从知道差在哪）。
    // 因此必须验证：勾上开关后，complete 请求体的 overwrite 真的为 true。
    const completeSpy = vi.spyOn(endpoints, "completeBackupUpload");
    renderTab();
    const { user, input } = await openUploadTab();

    // 默认不勾（false 不该被静默写成 true：覆盖会动目标实例的既有数据）。
    const overwriteBox = await screen.findByTestId("upload-overwrite");
    expect((overwriteBox as HTMLInputElement).checked).toBe(false);

    await user.click(overwriteBox);
    expect((overwriteBox as HTMLInputElement).checked).toBe(true);

    // 选文件走完流程（单片 1 MiB，无需放慢）。
    await user.upload(input, makeFile(1 * 1024 * 1024));

    await waitFor(
      () => {
        expect(completeSpy).toHaveBeenCalled();
        const opts = completeSpy.mock.calls[0]?.[1];
        expect(opts?.overwrite).toBe(true);
      },
      { timeout: 15_000 },
    );
  }, 25_000);

  it("上传进行中禁止收起内联面板（否则进度与取消入口一起消失）", async () => {
    // 弹窗时代这条守的是"上传中禁止关闭弹窗"；改内联后等价的危险动作变成**收起面板**
    // ——进度条与取消按钮都在面板里，收起来等于把唯一的进度反馈藏掉。
    //
    // 收起有**两条路径**，必须都守住：
    //   ① 面板自己的「收起」按钮（ImportInline 内，busy 时 disabled）；
    //   ② 动作条上那个已展开的通道按钮（点它会 toggle 成 null，即收起）。
    // ②是实测发现的漏洞：busy 状态原先只存在于 ImportInline 内部，动作条拿着 setChannel
    // 直接就能把面板收掉绕过①；现由页面上报 busy 并同时管住两条路径。
    slowChunks();
    renderTab();
    const { user, input } = await openUploadTab();

    await user.upload(input, makeFile(20 * 1024 * 1024));
    // 等到确实在上传。
    await screen.findByTestId("upload-progress", {}, { timeout: 8000 });

    // 路径①：收起按钮必须在忙碌期间禁用：点它面板不能消失。
    const collapse = screen.getByRole("button", { name: "收起" });
    expect(collapse.hasAttribute("disabled")).toBe(true);
    await user.click(collapse);
    expect(screen.getByTestId("import-inline").style.display).not.toBe("none");
    expect(screen.getByTestId("upload-progress")).toBeTruthy();

    // 路径②：动作条上的通道按钮同样必须禁用。这一条是有效的回归守卫——
    // 去掉 ActionCards 的 disabled={uploadBusy} 后它会转红（点按钮会把面板收掉）。
    const cardUpload = screen.getByTestId("action-import-upload");
    expect(cardUpload.hasAttribute("disabled")).toBe(true);
    await user.click(cardUpload);
    expect(screen.getByTestId("import-inline").style.display).not.toBe("none");
    expect(screen.getByTestId("upload-progress")).toBeTruthy();
  }, 25_000);

  it("中途取消（abort）后会话标记为已中止", async () => {
    const createSpy = vi.spyOn(endpoints, "createBackupUpload");
    // 放慢每片上传，确保能在 active 阶段点取消。
    slowChunks();

    renderTab();
    const { user, input } = await openUploadTab();
    await user.upload(input, makeFile(20 * 1024 * 1024));
    await screen.findByTestId("upload-progress", {}, { timeout: 8000 });

    // 面板内取消 → 二次确认弹窗 → 确认。
    await user.click(screen.getByRole("button", { name: "取消上传" }));
    // 二次确认由 ModalsProvider 渲染（内联面板不是 Modal，所以这里的 dialog 只有
    // 确认弹窗这一个）。按标题定位，与主面板的标题互不冲突。
    const confirm = await waitFor(() => {
      const found = screen
        .getAllByRole("dialog")
        .find((d) => within(d).queryByRole("heading", { name: "取消上传" }));
      expect(found).toBeTruthy();
      return found as HTMLElement;
    });
    // 确认弹窗里只有一个「取消上传」按钮（另一个是「取消」，即 cancelLabel）。
    await user.click(within(confirm).getByRole("button", { name: "取消上传" }));

    // 会话状态变为 aborted（审计保留，GET 仍返回 200）。
    await waitFor(
      async () => {
        const created = createSpy.mock.results[0]?.value as
          Promise<{ uploadId: string }> | undefined;
        expect(created).toBeTruthy();
        const id = (await created!).uploadId;
        const s = await endpoints.getBackupUpload(id);
        expect(s.status).toBe("aborted");
      },
      { timeout: 5_000 },
    );
  }, 20_000);

  it("续传：预置未完成会话（uploadedChunks）→ 只上传缺失片", async () => {
    // 直接调用端点预置会话前需先登录（mock 管理员令牌）。
    setToken("mock.jwt.token");
    const file = makeFile(10 * 1024 * 1024); // 2 片

    // 预置会话并先落盘第 1 片（末片），模拟“上次只传了一半”。
    const init = await endpoints.createBackupUpload({ fileName: file.name, totalBytes: file.size });
    const chunkSize = init.chunkSize;
    await endpoints.uploadBackupChunk(init.uploadId, 1, file.slice(chunkSize, file.size));
    localStorage.setItem(
      "jianartifact.backupUpload",
      JSON.stringify({
        uploadId: init.uploadId,
        fileName: file.name,
        totalBytes: file.size,
        chunkSize,
      }),
    );

    const chunkSpy = vi.spyOn(endpoints, "uploadBackupChunk");
    renderTab();
    const { user, input } = await openUploadTab();

    // 未完成会话的提示：面板一展开就该看到（探测在挂载时已完成）。
    const title = await screen.findByText("发现未完成的上传", {}, { timeout: 5000 });
    const callout = title.closest(".mantine-Alert-root") as HTMLElement;
    expect(callout).toBeTruthy();
    // 文件名嵌在「上次的上传「…」尚未完成」整句里，用正则匹配。
    expect(within(callout).getByText(/bk-upload\.tar\.gz/)).toBeTruthy();

    // 继续上传 → 选同一文件。
    await user.click(within(callout).getByRole("button", { name: "继续上传" }));
    await user.upload(input, file);

    // 完成后本面板给出回执。
    await waitFor(
      () => {
        expect(screen.getByText("上传完成，已提交导入")).toBeTruthy();
      },
      { timeout: 15_000 },
    );

    // 只补传了缺失的第 0 片，未重复上传已落盘的第 1 片。
    // 仅统计本会话的分片调用（并行负载下可能有其它用例遗留的在途请求落入同一 spy）。
    const calledIndexes = chunkSpy.mock.calls
      .filter((c) => c[0] === init.uploadId)
      .map((c) => c[1]);
    expect(calledIndexes).toEqual([0]);
  }, 25_000);

  it("收起内联面板后重新展开，未完成会话仍被发现（面板必须保持挂载）", async () => {
    // 续传探测在面板挂载时执行（读 localStorage → GET 拉回 uploadedChunks）。
    // 若面板被条件卸载（而不是 display:none），探测就永远跑不到，"只补缺失片"的能力
    // 整个失效——这正是它必须常驻挂载的原因。这条用例守住那个前提。
    setToken("mock.jwt.token");
    const file = makeFile(10 * 1024 * 1024);
    const init = await endpoints.createBackupUpload({ fileName: file.name, totalBytes: file.size });
    await endpoints.uploadBackupChunk(init.uploadId, 1, file.slice(init.chunkSize, file.size));
    localStorage.setItem(
      "jianartifact.backupUpload",
      JSON.stringify({
        uploadId: init.uploadId,
        fileName: file.name,
        totalBytes: file.size,
        chunkSize: init.chunkSize,
      }),
    );

    renderTab();
    const { user } = await openUploadTab();
    expect(await screen.findByText("发现未完成的上传", {}, { timeout: 5000 })).toBeTruthy();

    // 收起再展开：提示仍在（面板没被卸载，会话也没丢）。
    const collapse = screen.getByRole("button", { name: "收起" });
    await user.click(collapse);
    expect(screen.getByTestId("import-inline").style.display).toBe("none");

    await user.click(await actionButtonAsync("上传包恢复"));
    expect(screen.getByTestId("import-channel-upload").style.display).not.toBe("none");
    expect(await screen.findByText("发现未完成的上传", {}, { timeout: 5000 })).toBeTruthy();
  }, 25_000);

  it("缺片错误如实展示（含缺失序号）", async () => {
    // 模拟后端：分片不齐，拒绝 complete，返回缺失序号。
    vi.spyOn(endpoints, "completeBackupUpload").mockRejectedValue(
      new ApiError("chunks_missing", "缺少分片：1,3", 400),
    );

    renderTab();
    const { user, input } = await openUploadTab();
    await user.upload(input, makeFile(1 * 1024 * 1024)); // 单片，上传成功但 complete 被拒。

    // 错误如实展示（含缺失序号）。
    const errText = await screen.findByText("缺少分片：1,3", {}, { timeout: 8000 });
    expect(errText).toBeTruthy();
  }, 15_000);
});
