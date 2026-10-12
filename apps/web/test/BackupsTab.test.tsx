// 备份与搬迁 Tab：视口滚动锁、备份列表渲染、生成向导全流程回归。
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { MigrationsPage } from "../src/pages/MigrationsPage";
import { renderWithProviders } from "./harness";

function renderTab() {
  return renderWithProviders(<MigrationsPage />, {
    route: "/migrations?tab=backups",
    authenticated: true,
  });
}

/**
 * 从某行的「⋯」溢出菜单里点某个动作。
 *
 * 行内只留一个「⋯」按钮，全部动作都在菜单里——这样操作列只需容纳一个按钮，
 * 不会出现按钮被压成残片或整表横向滚动（此前带文字的多个行内按钮实测需要约
 * 207px，而表格可用宽只有 984px）。测试走真实路径：菜单必须能打开、项必须可点。
 */
async function clickRowMenuAction(
  user: ReturnType<typeof userEvent.setup>,
  row: HTMLElement,
  action: string,
) {
  await user.click(within(row).getByRole("button", { name: "更多操作" }));
  const menuItem = await screen.findByRole("menuitem", { name: action });
  await user.click(menuItem);
}

describe("备份与搬迁", () => {
  it("锁定视口高度：外层不滚动，列表在剩余高度内滚动", async () => {
    renderTab();

    const page = await screen.findByTestId("migrations-page");
    expect(page.style.height).toContain("calc(100dvh");
    expect(page.style.overflow).toBe("hidden");
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
      "来源",
      "方式",
      "状态",
      "大小",
      "包含",
      "创建时间",
      "操作",
    ]);

    // 预置的已完成包。
    const manualRow = within(table)
      .getByText("bk-20260910-090000-a1b2c3")
      .closest("tr") as HTMLElement;
    expect(within(manualRow).getByText("已完成")).toBeTruthy();
    expect(within(manualRow).getByText("热备份")).toBeTruthy();

    // 操作列只有一个「⋯」按钮：带文字的行内动作按钮会需要约 207px 而表格挤不出，
    // 结果是按钮被 flex 压成残片（可读性丧失）或整表横向滚动。守护"只有一个按钮"。
    const rowButtons = within(manualRow).getAllByRole("button");
    expect(rowButtons.length).toBe(1);
    expect(rowButtons[0].getAttribute("aria-label")).toBe("更多操作");
  });

  // —— FR-40：来源列（手动 / 自动）——
  it("来源列区分手动与自动备份", async () => {
    renderTab();

    const wayHeader = await screen.findByText("方式");
    const table = wayHeader.closest("table") as HTMLElement;

    // 种子同时含手动包与自动包，两种徽章文案都要出现——
    // 只断言其中一个会让"两种来源都渲染成同一文案"的漂移蒙混过关。
    expect(within(table).getByText("手动")).toBeTruthy();
    expect(within(table).getByText("自动")).toBeTruthy();

    // 自动包的备注（周期自动备份）与来源徽章在同一行。
    const autoRow = within(table)
      .getByText("bk-20260910-030000-d4e5f6")
      .closest("tr") as HTMLElement;
    expect(within(autoRow).getByText("自动")).toBeTruthy();
    const manualRow = within(table)
      .getByText("bk-20260910-090000-a1b2c3")
      .closest("tr") as HTMLElement;
    expect(within(manualRow).getByText("手动")).toBeTruthy();
  });

  // —— FR-40：自动备份配置卡（默认折叠 + 展开后可编辑 + 保存即生效）——
  it("自动备份配置卡默认折叠，只显示当前值摘要，不占用列表高度", async () => {
    renderTab();

    // 折叠态：卡上必须写明"保存即生效"，不再有"需重启"的旧文案。
    expect(await screen.findByText("保存即生效，无需重启")).toBeTruthy();
    expect(screen.queryByText("间隔改动需重启生效")).toBeNull();

    // 默认收起：三个输入框都不可见（Mantine Collapse 收起时不渲染内容）。
    // 这是高度回归的直接守卫——本卡展开时有 365px，会把备份列表挤到 80px。
    expect(screen.queryByTestId("auto-interval")).toBeNull();
    expect(screen.queryByTestId("keep-last")).toBeNull();
    expect(screen.queryByTestId("retention-days")).toBeNull();

    // 折叠态必须给出当前值摘要，否则用户无法一眼看出自动备份是否在跑。
    // mock 的间隔为 0 → 必须明说"已关闭"，而不是显示"每 0 秒备份一次"。
    expect(screen.getByText(/自动备份已关闭/)).toBeTruthy();
    expect(screen.getByText(/保留最近 7 份/)).toBeTruthy();
    expect(screen.getByText(/30 天内/)).toBeTruthy();

    // 展开开关存在且语义正确。
    const toggle = screen.getByRole("button", { name: "配置" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
  });

  it("展开后可编辑三项配置，保存后提交到 settings 通道", async () => {
    const user = userEvent.setup();
    renderTab();

    // 先展开（默认折叠）。
    const toggle = await screen.findByRole("button", { name: "配置" });
    await user.click(toggle);

    // 三项输入都在，且初值来自 settings（mock 里为 0 / 7 / 30）。
    const interval = await screen.findByTestId("auto-interval");
    const keepLast = screen.getByTestId("keep-last");
    const retentionDays = screen.getByTestId("retention-days");
    expect((interval as HTMLInputElement).value).toBe("0");
    expect((keepLast as HTMLInputElement).value).toBe("7");
    expect((retentionDays as HTMLInputElement).value).toBe("30");

    // 未改动时保存按钮禁用：避免用户误以为"点了没反应"。
    const saveBtn = screen.getByRole("button", { name: "保存" });
    expect(saveBtn.hasAttribute("disabled")).toBe(true);

    // 改间隔 → 保存按钮变可用 → 点击保存 → 成功提示出现。
    await user.clear(interval);
    await user.type(interval, "60");
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "保存" }).hasAttribute("disabled")).toBe(false);
    });
    await user.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(
      () => {
        expect(screen.getByText(/自动备份配置已保存/)).toBeTruthy();
      },
      { timeout: 10_000 },
    );
  }, 20_000);

  it("自动备份配置卡的 0 值不被误当成空值", async () => {
    renderTab();
    // 间隔 0 是有意义的取值（关闭）；若归一化写成 `|| 默认值`，折叠摘要会显示成
    // "每 7 秒备份一次"之类的默认值，而不是"已关闭"。直接断言摘要语义而非数字。
    expect(await screen.findByText(/自动备份已关闭/)).toBeTruthy();
  });

  // —— FR-40：恢复演练入口与报告 ——
  it("演练入口打开报告弹窗，历史报告列出逐项结论（失败项如实展示）", async () => {
    const user = userEvent.setup();
    renderTab();

    const wayHeader = await screen.findByText("方式");
    const table = wayHeader.closest("table") as HTMLElement;
    const row = within(table).getByText("bk-20260910-090000-a1b2c3").closest("tr") as HTMLElement;
    await clickRowMenuAction(user, row, "演练");

    const dialog = await screen.findByRole("dialog");
    // 弹窗说明必须讲清"只读路径"这一关键边界。
    expect(within(dialog).getByText(/不会写入待重启标记/)).toBeTruthy();

    // 两级校验的差异必须写在界面上：正常包跑两级结果完全一样（内容没问题时，
    // "没验内容"与"验过内容"的结论当然相同），用户只看报告看不出区别，
    // 若不解释就会以为深校验是多余的。这里守护住"差异说明存在"这件事。
    expect(within(dialog).getByText(/浅校验（只看结构/)).toBeTruthy();
    expect(within(dialog).getByText(/深校验（逐字节核对内容/)).toBeTruthy();
    // 浅校验必须显式声明它漏判的场景，否则等于把"看结构"说成"能验内容"。
    expect(within(dialog).getByText(/内容被静默篡改但文件结构完好/)).toBeTruthy();
    // 深校验标注"建议"，把更严格的一档明确推荐给用户。
    expect(within(dialog).getByText("建议")).toBeTruthy();
    // 默认档位是深校验：浅校验对「内容被静默篡改但结构完好」的包会漏判，
    // 而演练的目的就是回答"这份包真的能恢复吗"，因此默认取更严格的一档。
    // 用完整标题精确匹配：说明文本里也含"深校验"三字，宽松匹配会命中多个 radio。
    // 本仓库未引入 jest-dom 的 toBeChecked，故直接断言 Mantine 的选中属性。
    const deepCard = within(dialog).getByRole("radio", { name: /^深校验（逐字节核对内容，慢）/ });
    expect(deepCard.getAttribute("aria-checked")).toBe("true");

    // 预置的失败报告被如实渲染：总判定「未通过」+ 失败检查项与原因都在。
    await waitFor(
      () => {
        expect(within(dialog).getByText("未通过")).toBeTruthy();
      },
      { timeout: 10_000 },
    );
    // 失败原因同时出现在总摘要与检查项说明两处，用 getAllByText 断言"至少出现"。
    expect(within(dialog).getAllByText(/manifest 74481 实际 74478/).length).toBeGreaterThan(0);
    // 四项检查项名称都在（结构化报告，不是一句 ok 字符串）。
    for (const name of ["归档完整性", "解包", "SQLite 一致性", "blob 规模"]) {
      expect(within(dialog).getByText(name)).toBeTruthy();
    }
  }, 20_000);

  it("发起演练后轮询到新报告（202 受理 → 报告出现）", async () => {
    const user = userEvent.setup();
    renderTab();

    const wayHeader = await screen.findByText("方式");
    const table = wayHeader.closest("table") as HTMLElement;
    const row = within(table).getByText("bk-20260910-030000-d4e5f6").closest("tr") as HTMLElement;
    await clickRowMenuAction(user, row, "演练");

    const dialog = await screen.findByRole("dialog");
    // 先等到预置报告渲染完，再计数，避免把异步首载当作"新报告"。
    await waitFor(
      () => {
        expect(within(dialog).getAllByText(/通过|未通过/).length).toBeGreaterThan(0);
      },
      { timeout: 10_000 },
    );
    const before = within(dialog).getAllByText(/^(通过|未通过)$/).length;

    await user.click(within(dialog).getByRole("button", { name: "发起演练" }));

    // 受理提示可见，随后轮询把新报告拉进来（报告条数增加）。
    await waitFor(
      () => {
        expect(within(dialog).getAllByText(/^(通过|未通过)$/).length).toBeGreaterThan(before);
      },
      { timeout: 15_000 },
    );
  }, 25_000);

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
