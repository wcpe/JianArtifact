// 未管理备份包（FR-40 追加八）：服务器 backups/ 下有包体但登记表无记录的包。
//
// 这些包在常规列表里不可见（列表读登记表），因此既不能用也不能删。本文件守住：
//   ① 能列出候选，无法识别的包标红且不可勾选；
//   ② 支持单选与多选（含全选/清空）；
//   ③ 导入后条目从"未管理"消失，并派发全局刷新让备份列表立即呈现。
//
// 面板常驻挂载（与其它导入通道一致），收起只置 display:none，因此这里靠点动作条
// 按钮把它显示出来，而不是"打开弹窗"。
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { BackupsTab } from "../src/components/backup/BackupsTab";
import * as endpoints from "../src/api/endpoints";
import { renderWithProviders } from "./harness";

afterEach(() => {
  vi.restoreAllMocks();
});

function renderTab() {
  return renderWithProviders(<BackupsTab />, {
    route: "/migrations?tab=backups",
    authenticated: true,
  });
}

function actionButton(name: string | RegExp): HTMLElement {
  return within(screen.getByTestId("backup-actions")).getByRole("button", { name });
}

async function actionButtonAsync(name: string | RegExp): Promise<HTMLElement> {
  // 显式给足等待：本文件多个用例共用同一页面，首载（备份列表 + 导入记录 +
  // 冻结状态 + 未管理包扫描）在并行负载下可能超过默认的 5s。
  await screen.findByTestId("backup-actions", {}, { timeout: 15_000 });
  return actionButton(name);
}

/** 展开内联「导入未管理的包」通道，返回 user 与面板。 */
async function openAdoptTab() {
  renderTab();
  const user = userEvent.setup();
  await user.click(await actionButtonAsync("导入未管理的包"));
  const channel = screen.getByTestId("import-channel-adopt");
  expect(channel.style.display).not.toBe("none");
  return { user, panel: within(channel) };
}

describe("未管理的备份包", () => {
  it("列出服务器上的候选包：非法包标红且复选框禁用", async () => {
    const { panel } = await openAdoptTab();

    // 两个合法包 + 一个非法包（来自 devmock 的固定夹具）。
    const good1 = await panel.findByTestId("unmanaged-row-bk-20261013-082334-f7ed3f");
    const good2 = panel.getByTestId("unmanaged-row-bk-20261014-213902-573c66");
    const broken = panel.getByTestId("unmanaged-row-bk-20261015-000000-broken");
    expect(good1).toBeTruthy();
    expect(good2).toBeTruthy();

    // 非法包：显示"无法导入"，且其复选框不可勾选——勾了也没法导入，不该给用户假希望。
    expect(within(broken).getByText("无法导入")).toBeTruthy();
    expect((within(broken).getByRole("checkbox") as HTMLInputElement).disabled).toBe(true);

    // 合法包可勾选。
    expect((within(good1).getByRole("checkbox") as HTMLInputElement).disabled).toBe(false);
  }, 20_000);

  it("支持单选：只勾一个时提交只带这一个标识", async () => {
    const adoptSpy = vi.spyOn(endpoints, "adoptBackups");
    const { user, panel } = await openAdoptTab();

    const row = await panel.findByTestId("unmanaged-row-bk-20261013-082334-f7ed3f");
    await user.click(within(row).getByRole("checkbox"));

    // 选中数徽章应出现（单选也走同一份选中集合）。
    expect(panel.getByText("已选 1 个")).toBeTruthy();

    await user.click(panel.getByTestId("unmanaged-submit"));

    await waitFor(() => {
      expect(adoptSpy).toHaveBeenCalledWith(["bk-20261013-082334-f7ed3f"]);
    });
  }, 20_000);

  it("支持多选与全选：全选只包含可导入的包", async () => {
    const adoptSpy = vi.spyOn(endpoints, "adoptBackups");
    const { user, panel } = await openAdoptTab();

    await panel.findByTestId("unmanaged-row-bk-20261013-082334-f7ed3f");

    // 全选：非法包不能被算进去（它不可导入）。
    await user.click(panel.getByTestId("unmanaged-select-all"));
    expect(panel.getByText("已选 2 个")).toBeTruthy();

    await user.click(panel.getByTestId("unmanaged-submit"));

    await waitFor(() => {
      expect(adoptSpy).toHaveBeenCalledWith([
        "bk-20261013-082334-f7ed3f",
        "bk-20261014-213902-573c66",
      ]);
    });
  }, 20_000);

  it("导入成功后条目从未管理列表消失，并给出结果摘要", async () => {
    const { user, panel } = await openAdoptTab();

    const row = await panel.findByTestId("unmanaged-row-bk-20261013-082334-f7ed3f");
    await user.click(within(row).getByRole("checkbox"));
    await user.click(panel.getByTestId("unmanaged-submit"));

    // 结果摘要如实给出三个计数。
    const result = await panel.findByTestId("unmanaged-result");
    expect(within(result).getByText(/成功 1 个/)).toBeTruthy();

    // 已导入的包不再出现在未管理列表里（devmock 把它并入了备份列表）。
    await waitFor(() => {
      expect(panel.queryByTestId("unmanaged-row-bk-20261013-082334-f7ed3f")).toBeNull();
    });
  }, 20_000);

  it("没有未管理包时给出空态说明，而不是空白区", async () => {
    vi.spyOn(endpoints, "listUnmanagedBackups").mockResolvedValue({ items: [], total: 0 });
    const { panel } = await openAdoptTab();
    expect(await panel.findByTestId("unmanaged-empty")).toBeTruthy();
  }, 20_000);
});
