// 「用这个包恢复」确认弹窗的冻结建议（问题 3）的守卫测试。
//
// 守的是什么：恢复期间既有写入无处落地，界面**提示**先冻结并给出入口，但**不强制**。
//   - 未冻结：显示冻结建议 + 冻结按钮（点它上抛 onFreeze，弹窗自己不直接冻结）；
//   - 已冻结：不再劝一次，改为提醒"重启后记得解冻"——忘记解冻会把实例变成只读，
//     那比它要防的问题更常见也更痛，所以这里不能反复劝说。
//
// 顺带守一条既有契约：`onAccepted` 必须带 **importId** 交回外层（受理只是开始，
// 校验与暂存还在跑，页面要据此打开进度弹窗），而不是让外层"刷新一下就好"。
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RestoreFromPackageModal } from "../src/components/backup/RestoreFromPackageModal";
import type { BackupPackage } from "../src/api/types";
import * as endpoints from "../src/api/endpoints";
import { renderWithProviders } from "./harness";

afterEach(() => {
  vi.restoreAllMocks();
});

/** 造一个可用的本机备份包。 */
function buildPackage(): BackupPackage {
  return {
    packageId: "bk-20261014-120000-a1b2c3",
    mode: "hot",
    origin: "manual",
    status: "done",
    sizeBytes: 4 * 1024 * 1024,
    createdAt: "2026-10-14T12:00:00Z",
  };
}

function renderModal(props: {
  onClose?: () => void;
  onAccepted?: (importId: string) => void;
  onFreeze?: () => void;
  frozen?: boolean;
}) {
  return renderWithProviders(
    <RestoreFromPackageModal
      pkg={buildPackage()}
      onClose={props.onClose ?? vi.fn()}
      onAccepted={props.onAccepted}
      onFreeze={props.onFreeze}
      frozen={props.frozen}
    />,
    { route: "/migrations?tab=backups", authenticated: true },
  );
}

describe("用本机包恢复的冻结建议", () => {
  it("未冻结时显示冻结建议与冻结按钮；点击上抛 onFreeze（弹窗自己不冻结）", async () => {
    // 去掉这个区块，本用例必须转红。
    const onFreeze = vi.fn();
    const freezeSpy = vi.spyOn(endpoints, "freezeWrites");

    renderModal({ onFreeze, frozen: false });

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/建议先冻结写入/)).toBeTruthy();

    const user = userEvent.setup();
    await user.click(within(dialog).getByTestId("restore-freeze"));

    expect(onFreeze).toHaveBeenCalledTimes(1);
    // 弹窗只负责"给入口"：冻结由外层的冻结弹窗统一执行（那里能选时长与填原因）。
    expect(freezeSpy).not.toHaveBeenCalled();
  });

  it("已冻结时不显示「建议冻结」，改为显示「记得解冻」", async () => {
    renderModal({ onFreeze: vi.fn(), frozen: true });

    const dialog = await screen.findByRole("dialog");
    expect(
      within(dialog).getByText("已冻结写入，重启后记得解冻（否则实例保持只读）。"),
    ).toBeTruthy();
    expect(within(dialog).queryByText(/建议先冻结写入/)).toBeNull();
    // 已冻结时也不该再给一个"冻结写入"按钮（点了没有意义）。
    expect(within(dialog).queryByTestId("restore-freeze")).toBeNull();
    // 覆盖开关与提交按钮仍要在场：冻结建议只是附加提示，不能把主流程挤掉。
    expect(within(dialog).getByTestId("restore-overwrite")).toBeTruthy();
    expect(within(dialog).getByTestId("restore-start")).toBeTruthy();
  });

  it("不传 onFreeze 时只提示不给按钮（能力探测不可用的部署形态不显示死按钮）", async () => {
    renderModal({ frozen: false });

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/建议先冻结写入/)).toBeTruthy();
    expect(within(dialog).queryByTestId("restore-freeze")).toBeNull();
  });

  it("受理成功把 importId 交回外层（受理只是开始，后续进度要靠它打开）", async () => {
    vi.spyOn(endpoints, "restoreFromLocalPackage").mockResolvedValue({
      importId: "imp-20261014-120000-xyz789",
      origin: "local",
      status: "queued",
      operator: "admin",
      overwrite: true,
      deep: false,
      createdAt: "2026-10-14T12:00:00Z",
      updatedAt: "2026-10-14T12:00:00Z",
    });
    const onAccepted = vi.fn();
    const onClose = vi.fn();

    renderModal({ onAccepted, onClose, frozen: false });

    const dialog = await screen.findByRole("dialog");
    const user = userEvent.setup();
    await user.click(within(dialog).getByTestId("restore-overwrite"));
    await user.click(within(dialog).getByTestId("restore-start"));

    await waitFor(() => {
      expect(onAccepted).toHaveBeenCalledWith("imp-20261014-120000-xyz789");
    });
    // 受理即关窗：接下来由进度弹窗接手（不然用户会对着一个已经无效的确认框发愣）。
    expect(onClose).toHaveBeenCalled();
  });

  it("包已被淘汰（404）时交回 onStale 处置，而不是裸报「备份包不存在」", async () => {
    // 实机踩到：弹窗开着时包被保留策略淘汰（30 秒间隔 + 保留 2 份），
    // 用户读完说明再点「开始恢复」就拿到 404。裸报后端文案正是用户先前
    // 反馈「复制链接会失败」的同一个观感问题，故必须走陈旧行统一处置。
    const { ApiError } = await import("../src/api/client");
    vi.spyOn(endpoints, "restoreFromLocalPackage").mockRejectedValue(
      new ApiError("not_found", "备份包不存在", 404),
    );
    const onStale = vi.fn(() => true);
    const onClose = vi.fn();

    renderWithProviders(
      <RestoreFromPackageModal
        pkg={buildPackage()}
        onClose={onClose}
        onStale={onStale}
        frozen={false}
      />,
      { route: "/migrations?tab=backups", authenticated: true },
    );

    const dialog = await screen.findByRole("dialog");
    const user = userEvent.setup();
    await user.click(within(dialog).getByTestId("restore-start"));

    await waitFor(() => {
      expect(onStale).toHaveBeenCalledTimes(1);
    });
    // 交回处置即关窗：外层会刷新列表（那条已消失的包随之从表里消失）。
    expect(onClose).toHaveBeenCalled();
  });

  it("非 404 的失败仍按普通错误处理，不被陈旧行逻辑吞掉", async () => {
    const { ApiError } = await import("../src/api/client");
    vi.spyOn(endpoints, "restoreFromLocalPackage").mockRejectedValue(
      new ApiError("conflict", "目标实例非空，需要覆盖", 409),
    );
    // 弹窗把**每个**失败都交给 onStale 裁决，由它决定"这算不算陈旧"——
    // 这里用与真实 handleStaleRow 相同的 404 过滤口径，验证 409 会被它放行到普通错误分支。
    const onStale = vi.fn((err: unknown) => err instanceof ApiError && err.status === 404);
    const onClose = vi.fn();

    renderWithProviders(
      <RestoreFromPackageModal
        pkg={buildPackage()}
        onClose={onClose}
        onStale={onStale}
        frozen={false}
      />,
      { route: "/migrations?tab=backups", authenticated: true },
    );

    const dialog = await screen.findByRole("dialog");
    const user = userEvent.setup();
    await user.click(within(dialog).getByTestId("restore-start"));

    await waitFor(() => {
      expect(onStale).toHaveBeenCalledTimes(1);
    });
    // 409 不是"这一行陈旧"：刷新列表也解决不了"需要勾选覆盖"，故不能关窗走陈旧路径。
    expect(onClose).not.toHaveBeenCalled();
  });
});
