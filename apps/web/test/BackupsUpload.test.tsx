// 分片上传卡（FR-137 第三通道）：init → 逐片上传 → 进度 → complete 后导入记录 pending_restart；
// 取消（abort）会话标记 aborted；续传只补缺失片；缺片错误如实展示。
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

/** 生成指定体积的本地文件（Node File，与 jsdom 垫片同源）。 */
function makeFile(sizeBytes: number, name = "bk-upload.tar.gz"): File {
  return new File([new Uint8Array(sizeBytes)], name, { type: "application/gzip" });
}

/**
 * 放慢每片上传，便于在 active 阶段观察进度条 / 点取消。
 * 必须先捕获原始实现再 spy：若在 mockImplementation 内调用被 spy 的函数会无限递归。
 */
function slowChunks() {
  const real = endpoints.uploadBackupChunk;
  return vi
    .spyOn(endpoints, "uploadBackupChunk")
    .mockImplementation(async (id, idx, body, opts) => {
      await new Promise((r) => setTimeout(r, 250));
      return real(id, idx, body, opts);
    });
}

describe("分片上传备份包", () => {
  it("选文件→init→逐片上传→进度出现→complete 后导入记录出现且为 pending_restart", async () => {
    const user = userEvent.setup();
    slowChunks();
    renderTab();

    // 20 MiB → 按服务端 8 MiB chunkSize 切 3 片，拉长 active 阶段便于观察进度。
    const file = makeFile(20 * 1024 * 1024);
    const input = await screen.findByTestId("backup-upload-input");
    await user.upload(input, file);

    // 进度条出现且带百分比（在 active 阶段内断言，避免完成后续传卸载）。
    await waitFor(
      () => {
        expect(screen.getByTestId("upload-progress")).toBeTruthy();
        expect(screen.getByTestId("upload-percent").textContent).toContain("%");
      },
      { timeout: 8000 },
    );

    // 本卡给出「已提交导入 / 需重启」回执。
    await waitFor(
      () => {
        expect(screen.getByText("上传完成，已提交导入")).toBeTruthy();
        // 上传卡回执与导入记录行都带该提示，故按集合断言。
        expect(screen.getAllByText("需重启服务后生效").length).toBeGreaterThan(0);
      },
      { timeout: 15_000 },
    );

    // complete 触发导入：新导入记录（origin=upload）经 queued→…→pending_restart 推进。
    const newId = await screen.findByText(/^imp-\d{14}-\d{3}$/, {}, { timeout: 15_000 });
    const row = newId.closest("tr") as HTMLElement;
    await waitFor(
      () => {
        expect(within(row).getByText("待重启")).toBeTruthy();
      },
      { timeout: 15_000 },
    );
  }, 30_000);

  it("中途取消（abort）后会话标记为已中止", async () => {
    const user = userEvent.setup();
    const createSpy = vi.spyOn(endpoints, "createBackupUpload");
    // 放慢每片上传，确保能在 active 阶段点取消。
    slowChunks();

    renderTab();
    const file = makeFile(20 * 1024 * 1024);
    const input = await screen.findByTestId("backup-upload-input");
    await user.upload(input, file);
    await screen.findByTestId("upload-progress", {}, { timeout: 8000 });

    // 卡片内取消 → 二次确认弹窗 → 确认。
    await user.click(screen.getByRole("button", { name: "取消上传" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "取消上传" }));

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
    const user = userEvent.setup();
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

    // 重开页面发现未完成会话，提示续传（作用域取 Alert 根，标题与按钮是兄弟节点）。
    const title = await screen.findByText("发现未完成的上传", {}, { timeout: 5000 });
    const callout = title.closest(".mantine-Alert-root") as HTMLElement;
    expect(callout).toBeTruthy();
    // 文件名嵌在「上次的上传「…」尚未完成」整句里，用正则匹配。
    expect(within(callout).getByText(/bk-upload\.tar\.gz/)).toBeTruthy();

    // 继续上传 → 选同一文件。
    await user.click(within(callout).getByRole("button", { name: "继续上传" }));
    const input = await screen.findByTestId("backup-upload-input");
    await user.upload(input, file);

    // 完成后本卡给出回执。
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

  it("缺片错误如实展示（含缺失序号）", async () => {
    const user = userEvent.setup();
    // 模拟后端：分片不齐，拒绝 complete，返回缺失序号。
    vi.spyOn(endpoints, "completeBackupUpload").mockRejectedValue(
      new ApiError("chunks_missing", "缺少分片：1,3", 400),
    );

    renderTab();
    const file = makeFile(1 * 1024 * 1024); // 单片，上传成功但 complete 被拒。
    const input = await screen.findByTestId("backup-upload-input");
    await user.upload(input, file);

    // 错误如实展示（含缺失序号）。
    const errText = await screen.findByText("缺少分片：1,3", {}, { timeout: 8000 });
    expect(errText).toBeTruthy();
  }, 15_000);
});
