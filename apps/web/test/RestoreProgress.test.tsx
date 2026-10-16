// 恢复进度弹窗（FR-40 追加七）的守卫测试。
//
// 守的是什么：受理恢复后只弹一个 toast 就再无反馈——用户不知道它在跑、跑到哪、失败没有。
// 本文件把「进度可见」这条承诺钉死成可执行断言：
//   - 阶段与状态如实展示（含拉取阶段的真实百分比，暂存阶段不编百分比）；
//   - **轮询真的在推进 UI**（去掉轮询 effect 必须转红）；
//   - **到终态就停**（继续拉只是白打接口——这条守卫的是"别白打接口"）；
//   - 失败记录如实展示后端原文 / 错误码，并提供「刷新」；
//   - 到终态通知外层刷新一次（且进行中不得提前通知）。
//
// 实现用 `window.setInterval` 轮询、间隔 1500ms，故时序用例统一走
// `vi.useFakeTimers({ shouldAdvanceTime: true })` + `advanceTimersByTimeAsync`，
// 与 BackupsTab.test.tsx 里演练轮询超时那条同一范式（shouldAdvanceTime 让 findBy*
// 的真实时间等待不被假定时器冻死）。
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RestoreProgressModal } from "../src/components/backup/RestoreProgressModal";
import type { BackupImport } from "../src/api/types";
import * as endpoints from "../src/api/endpoints";
import { renderWithProviders } from "./harness";

/** 必须与 RestoreProgressModal 内的 POLL_INTERVAL_MS 一致（该常量未导出，故此处复写）。 */
const POLL_INTERVAL_MS = 1500;

// 用例间恢复所有 spy，避免替换实现泄漏到后续用例。
afterEach(() => {
  vi.restoreAllMocks();
});

/** 造一条导入记录：只关心"此刻处于哪个阶段"的用例不必手写全部字段。 */
function buildImport(
  over: Partial<BackupImport> & { status: BackupImport["status"] },
): BackupImport {
  return {
    importId: "imp-20261014-120000-abc123",
    origin: "url",
    operator: "admin",
    overwrite: false,
    deep: false,
    createdAt: "2026-10-14T12:00:00Z",
    updatedAt: "2026-10-14T12:00:00Z",
    ...over,
  };
}

function renderModal(props: {
  importId: string | null;
  onClose?: () => void;
  onSettled?: () => void;
  onFreeze?: () => void;
  frozen?: boolean;
}) {
  return renderWithProviders(
    <RestoreProgressModal
      importId={props.importId}
      onClose={props.onClose ?? vi.fn()}
      onSettled={props.onSettled}
      onFreeze={props.onFreeze}
      frozen={props.frozen}
    />,
    { route: "/migrations?tab=backups", authenticated: true },
  );
}

describe("恢复进度弹窗", () => {
  it("打开即显示状态、阶段与拉取进度百分比", async () => {
    // 拉取阶段有真实字节数可算，必须给出百分比与字节口径。
    vi.spyOn(endpoints, "getBackupImport").mockResolvedValue(
      buildImport({
        status: "fetching",
        totalBytes: 4 * 1024 * 1024,
        fetchedBytes: 1 * 1024 * 1024,
      }),
    );

    renderModal({ importId: "imp-20261014-120000-abc123" });

    // 状态徽章 + 来源徽章 + 记录标识。
    expect(await screen.findByText("拉取中")).toBeTruthy();
    expect(screen.getByText("URL")).toBeTruthy();
    expect(screen.getByText("imp-20261014-120000-abc123")).toBeTruthy();
    // 阶段说明逐阶段写清"正在做什么"。
    expect(screen.getByText("正在从来源拉取备份包。")).toBeTruthy();
    // 1 MiB / 4 MiB = 25%，单位按 KiB/MiB 归一（与列表同口径）。
    expect(screen.getByText("已拉取 1.0 MiB / 4.0 MiB（25%）")).toBeTruthy();
    // 有真实进度条（Mantine Progress 提供 role=progressbar 与 aria-valuenow）。
    expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe("25");
  });

  it("暂存阶段无字节数可算：给不确定进度 + 对象数，不给假百分比", async () => {
    vi.spyOn(endpoints, "getBackupImport").mockResolvedValue(
      buildImport({ status: "staging", blobCount: 412 }),
    );

    renderModal({ importId: "imp-20261014-120000-abc123" });

    expect(await screen.findByText("暂存中")).toBeTruthy();
    expect(screen.getByText("正在校验包并把数据暂存到待生效位置（此步最慢）。")).toBeTruthy();
    expect(screen.getByText("处理中，共 412 个对象；此阶段耗时与包体积同阶。")).toBeTruthy();
    // 编出来的百分比比不显示更误导——这里必须没有进度条。
    expect(screen.queryByRole("progressbar")).toBeNull();
  });

  it("轮询推进：staging 经一次轮询后变 pending_restart，界面推进到「可重启」区块", async () => {
    // 这条守的是"进度真的会自己往前走"。去掉组件的轮询 effect 后，
    // 界面会永远停在「暂存中」，本用例必须转红。
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let record = buildImport({ status: "staging", blobCount: 412 });
      const spy = vi.spyOn(endpoints, "getBackupImport").mockImplementation(async () => record);

      renderModal({ importId: "imp-20261014-120000-abc123" });

      expect(await screen.findByText("暂存中")).toBeTruthy();
      expect(screen.queryByText("可以重启了")).toBeNull();

      // 后端在这一刻完成暂存（状态机推进）。
      record = buildImport({ status: "pending_restart", blobCount: 412 });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
      });

      expect(await screen.findByText("可以重启了")).toBeTruthy();
      expect(screen.getByText("待重启")).toBeTruthy();
      expect(screen.getByText("校验与暂存已完成，等待重启服务生效。")).toBeTruthy();
      // 首载 1 次 + 轮询至少 1 次。
      expect(spy.mock.calls.length).toBeGreaterThanOrEqual(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("到达终态后停止轮询（getBackupImport 调用次数不再增长）", async () => {
    // pending_restart 不会再变（除非重启），继续拉只会白打接口。
    // 若把"仅进行中才轮询"的门控改成"始终轮询"，本用例转红。
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let record = buildImport({ status: "staging" });
      const spy = vi.spyOn(endpoints, "getBackupImport").mockImplementation(async () => record);

      renderModal({ importId: "imp-20261014-120000-abc123" });
      expect(await screen.findByText("暂存中")).toBeTruthy();

      record = buildImport({ status: "pending_restart" });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
      });
      expect(await screen.findByText("可以重启了")).toBeTruthy();

      const settledCalls = spy.mock.calls.length;
      // 再推 4 个轮询间隔：一次都不该再发生。
      await act(async () => {
        await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 4);
      });
      expect(spy.mock.calls.length).toBe(settledCalls);
    } finally {
      vi.useRealTimers();
    }
  });

  it("失败记录如实展示后端原文，并给出「刷新」而不是让用户重开弹窗", async () => {
    const spy = vi
      .spyOn(endpoints, "getBackupImport")
      .mockResolvedValue(
        buildImport({ status: "failed", error: "暂存失败：目标磁盘空间不足（需 12 GiB）" }),
      );

    renderModal({ importId: "imp-20261014-120000-abc123" });

    expect(await screen.findByText("失败")).toBeTruthy();
    expect(screen.getByText("恢复未完成，详见下方原因。")).toBeTruthy();
    expect(screen.getByText("暂存失败：目标磁盘空间不足（需 12 GiB）")).toBeTruthy();

    // 「刷新」重拉记录：失败是终态（不轮询），只有这一个出口。
    const before = spy.mock.calls.length;
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "刷新" }));
    await waitFor(() => {
      expect(spy.mock.calls.length).toBeGreaterThan(before);
    });
  });

  it("错误码存在时展示本地可读说法（而不是把 code 甩给用户）", async () => {
    vi.spyOn(endpoints, "getBackupImport").mockResolvedValue(
      buildImport({ status: "failed", errorCode: "target_not_empty" }),
    );

    renderModal({ importId: "imp-20261014-120000-abc123" });

    expect(await screen.findByText("目标实例非空，导入前请开启「覆盖已有数据」。")).toBeTruthy();
  });

  it("到达终态时通知外层刷新一次（进行中不得提前通知）", async () => {
    // 列表里的状态徽章靠这个回调跟着变；提前通知等于让外层拿到过期状态。
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let record = buildImport({ status: "staging" });
      vi.spyOn(endpoints, "getBackupImport").mockImplementation(async () => record);
      const onSettled = vi.fn();

      renderModal({ importId: "imp-20261014-120000-abc123", onSettled });
      expect(await screen.findByText("暂存中")).toBeTruthy();
      expect(onSettled).not.toHaveBeenCalled();

      record = buildImport({ status: "pending_restart" });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
      });

      await waitFor(() => {
        expect(onSettled).toHaveBeenCalledTimes(1);
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it("待重启且未冻结：给出冻结建议与冻结入口（点击上抛 onFreeze）", async () => {
    // 恢复真正的最后一步是"重启"，不能只留用户对着一条待重启记录发愣。
    // 冻结此刻才提示，是因为"现在要不要先冻结"这个决策的真实时点就在重启前。
    const onFreeze = vi.fn();
    vi.spyOn(endpoints, "getBackupImport").mockResolvedValue(
      buildImport({ status: "pending_restart" }),
    );

    renderModal({ importId: "imp-20261014-120000-abc123", onFreeze, frozen: false });

    expect(await screen.findByText("可以重启了")).toBeTruthy();
    expect(screen.getByText(/建议先冻结写入/)).toBeTruthy();

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "冻结写入" }));
    expect(onFreeze).toHaveBeenCalledTimes(1);
  });

  it("待重启且已冻结：不再劝一次，改为提醒重启后解冻", async () => {
    vi.spyOn(endpoints, "getBackupImport").mockResolvedValue(
      buildImport({ status: "pending_restart" }),
    );

    renderModal({ importId: "imp-20261014-120000-abc123", onFreeze: vi.fn(), frozen: true });

    expect(await screen.findByText("可以重启了")).toBeTruthy();
    expect(screen.queryByText(/建议先冻结写入/)).toBeNull();
    expect(screen.getByText("已冻结写入，重启后记得解冻（否则实例保持只读）。")).toBeTruthy();
  });

  it("记录读不到（404）时说明恢复已生效，而不是报「读取失败」", async () => {
    // 实机踩到：恢复生效会把整个数据库替换成包里的快照，而这条导入记录正写在
    // **被替换掉的那个库里**，重启后自然消失。此刻若渲染成通用错误（"找不到记录"），
    // 说的与事实正好相反——用户刚恢复成功，界面却在报错。
    const { ApiError } = await import("../src/api/client");
    vi.spyOn(endpoints, "getBackupImport").mockRejectedValue(
      new ApiError("not_found", "导入记录不存在", 404),
    );

    renderModal({ importId: "imp-20261014-120000-abc123" });

    const applied = await screen.findByTestId("restore-applied");
    expect(within(applied).getByText("恢复已生效")).toBeTruthy();
    // 不得出现后端那条"记录不存在"的错误文案。
    expect(screen.queryByText("导入记录不存在")).toBeNull();
    // 给一个明确的收尾出口。
    expect(within(applied).getByRole("button", { name: "知道了" })).toBeTruthy();
  });
});
