// 备份与搬迁 Tab：视口滚动锁、备份列表渲染、生成向导全流程回归。
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { BackupsTab } from "../src/components/backup/BackupsTab";
import { renderWithProviders } from "./harness";

function renderTab() {
  return renderWithProviders(<BackupsTab />, {
    route: "/migrations?tab=backups",
    authenticated: true,
  });
}

describe("备份与搬迁", () => {
  it("锁定视口高度：外层不滚动，列表在剩余高度内滚动", async () => {
    renderTab();

    const tab = await screen.findByTestId("backups-tab");
    expect(tab.style.height).toContain("calc(100dvh");
    expect(tab.style.overflow).toBe("hidden");
  });

  it("备份列表用 Table 渲染，状态走 StatusPill", async () => {
    renderTab();

    // Mantine Table 渲染为原生 table 角色；页面现含备份表与导入记录表两张，
    // 「方式」列仅出现在备份包表，据此锁定该表避免与导入记录表混淆。
    const wayHeader = await screen.findByText("方式");
    const table = wayHeader.closest("table") as HTMLElement;
    const headers = within(table).getAllByRole("columnheader");
    expect(headers.map((h) => h.textContent)).toEqual([
      "包标识",
      "方式",
      "状态",
      "大小",
      "包含",
      "创建时间",
      "操作",
    ]);

    // 预置的已完成包。
    expect(within(table).getByText("bk-20260910-090000-a1b2c3")).toBeTruthy();
    expect(within(table).getByText("已完成")).toBeTruthy();
    expect(within(table).getByText("热备份")).toBeTruthy();
  });

  it("页面内不重复渲染大标题", async () => {
    renderTab();
    await screen.findByTestId("backups-tab");
    // 标题由页眉面包屑承担；分区卡标题为「备份与搬迁」但不应是 heading 级大标题。
    expect(screen.queryByRole("heading", { name: "迁移与搬迁" })).toBeNull();
  });

  it("生成向导：选方式 → 填备注 → 生成完成后给出下载链接", async () => {
    const user = userEvent.setup();
    renderTab();

    await user.click(await screen.findByRole("button", { name: "生成备份包" }));

    const dialog = await screen.findByRole("dialog");
    // 第一步：选择方式，默认热备份。
    expect(within(dialog).getByText("热备份（不停服）")).toBeTruthy();
    expect(within(dialog).getByText("冻结窗口（停写）")).toBeTruthy();

    await user.click(within(dialog).getByRole("button", { name: "下一步" }));
    // 第二步：备注 + 边界说明。
    expect(within(dialog).getByText(/不含任何密钥或节点本地配置/)).toBeTruthy();

    await user.click(within(dialog).getByRole("button", { name: "开始生成" }));

    // 第三步：等待 devmock 推进到 done，并签发下载链接。
    await waitFor(
      () => {
        expect(within(dialog).getByText(/备份包已生成/)).toBeTruthy();
      },
      { timeout: 10_000 },
    );
    const link = within(dialog).getByText(/\/api\/v1\/backups\/.+\/download\?/);
    expect(link.textContent).toContain("token=");
  }, 20_000);

  // —— FR-134：写入冻结窗口 ——
  it("冻结卡初始显示「可写」；点冻结并选档位后显示「已冻结」与解冻时间", async () => {
    const user = userEvent.setup();
    renderTab();

    // 初始未冻结：状态徽章为「可写」。
    expect(await screen.findByText("可写")).toBeTruthy();

    // 打开冻结 Modal，选择有界档位后确认。
    await user.click(await screen.findByRole("button", { name: "冻结写入" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByText("4 小时"));
    await user.click(within(dialog).getByRole("button", { name: "冻结" }));

    // 冻结成功后徽章转为「已冻结」，并显示自动解冻时间。
    await waitFor(
      () => {
        expect(screen.getByText("已冻结")).toBeTruthy();
      },
      { timeout: 5_000 },
    );
    expect(screen.getByText(/自动解冻时间/)).toBeTruthy();
  }, 15_000);

  it("解冻需二次确认，确认后回到「可写」", async () => {
    const user = userEvent.setup();
    renderTab();

    // 先冻结。
    await user.click(await screen.findByRole("button", { name: "冻结写入" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByText("2 小时"));
    await user.click(within(dialog).getByRole("button", { name: "冻结" }));
    await waitFor(() => expect(screen.getByText("已冻结")).toBeTruthy(), { timeout: 5_000 });

    // 解冻走 confirmDanger：弹窗确认「解冻」后才生效。
    await user.click(screen.getByRole("button", { name: "解冻写入" }));
    const confirmBtn = await screen.findByRole("button", { name: "解冻" });
    await user.click(confirmBtn);

    await waitFor(
      () => {
        expect(screen.getByText("可写")).toBeTruthy();
      },
      { timeout: 5_000 },
    );
  }, 15_000);

  // —— FR-137：从 URL 导入备份包 ——
  it("从 URL 导入提交后出现新记录且是 pending_restart（含「需重启」提示）", async () => {
    const user = userEvent.setup();
    renderTab();

    // 种子已有一条待重启记录，提示可见。
    expect(await screen.findByText("需重启服务后生效")).toBeTruthy();

    await user.click(await screen.findByRole("button", { name: "从 URL 导入" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(
      within(dialog).getByPlaceholderText("https://example.com/backups/bk.tar.gz"),
      "https://example.com/backups/bk.tar.gz",
    );
    await user.click(within(dialog).getByRole("button", { name: "开始导入" }));

    // 受理记录按 queued → fetching → staging → pending_restart 推进，最终态由轮询可见。
    const newId = await screen.findByText(/^imp-\d{14}-\d{3}$/);
    const row = newId.closest("tr") as HTMLElement;
    await waitFor(
      () => {
        expect(within(row).getByText("待重启")).toBeTruthy();
      },
      { timeout: 15_000 },
    );
    expect(within(row).getByText("需重启服务后生效")).toBeTruthy();
  }, 25_000);

  it("导入记录状态用 StatusPill；失败行显示错误摘要", async () => {
    renderTab();

    const failedId = await screen.findByText("imp-20260909-160000-g7h8i9");
    const row = failedId.closest("tr") as HTMLElement;
    // 状态徽章（StatusPill）显示「失败」。
    expect(within(row).getByText("失败")).toBeTruthy();
    // 错误列给出错误摘要。
    expect(within(row).getByText(/schemaVersion/)).toBeTruthy();
  });
});

