// 备份与搬迁 Tab：两段式布局、统一事件表混排、内联导入与生成 / 冻结 / 演练全流程回归。
//
// 布局已从「待处理条 + 动作 + 关键事实 + 备份列表」四段式收敛为两段式：
//   ① 内联导入区 import-inline（收起时 display:none，仍常驻挂载）
//   ② 关键事实   backup-facts
//   ③ 统一事件表 backup-list-section（flex:1，独占剩余高度；表 testid backup-event-table）
//      —— 动作条 backup-actions（五个按钮）挂在**这个区块的标题行里**，不再独立成块。
//
// 本次重构删除了四个结构，本文件不得再引用（引用即等于守着已不存在的东西）：
//   - AttentionBanner：backup-attention-banner / backup-attention-clear 与"有 N 项需要你确认"都不再有；
//   - ImportEntry 弹窗：import-tab-records 与 Modal 承载导入记录的方式已废；
//   - ImportRecordsTable / BackupTable，以及导入入口角标 import-entry-badge；
//   - ActionCards：两张常驻动作卡（约 173px）收成标题行右侧一排按钮，见 ActionBar。
// 导入记录不再有独立表格，而是与本机备份包**按 createdAt 倒序混排**在同一张表里，
// 靠行上的 data-row-kind="package|import" 区分（见"混排顺序"用例——它是本次重构的核心守卫）。
//
// 行为未变但载体变了的部分（生成向导 / 冻结 / 演练 / 配置弹窗 / 关键事实）原样保留断言，
// 只把定位方式换到新结构上；注释里写明每条守的是什么、为什么，避免后续维护者以为是冗余。
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { MigrationsPage } from "../src/pages/MigrationsPage";
import { DRILL_POLL_TIMEOUT_MS } from "../src/components/backup/DrillReportModal";
import { ApiError } from "../src/api/client";
import type { BackupImport, BackupPackage } from "../src/api/types";
import * as endpoints from "../src/api/endpoints";
import { renderWithProviders } from "./harness";

// 用例间恢复所有 spy，避免替换实现泄漏到后续用例。
afterEach(() => {
  vi.restoreAllMocks();
});

function renderTab() {
  return renderWithProviders(<MigrationsPage />, {
    route: "/migrations?tab=backups",
    authenticated: true,
  });
}

/**
 * 造一条本机备份包：只关心"什么时候发生了什么"的用例不必手写全部字段。
 * 默认 done（终态，不需轮询），时间戳由用例给出——它才是混排用例的自变量。
 */
function buildPackage(over: Partial<BackupPackage> & { packageId: string }): BackupPackage {
  return {
    mode: "hot",
    origin: "manual",
    status: "done",
    sizeBytes: 1024,
    createdAt: "2026-09-09T10:00:00Z",
    ...over,
  };
}

/**
 * 造一条导入记录：只关心"什么时候发生了什么"的用例不必手写全部字段。
 * 默认 done（终态、不需轮询）。
 */
function buildImport(over: Partial<BackupImport> & { importId: string }): BackupImport {
  return {
    origin: "url",
    status: "done",
    operator: "admin",
    overwrite: false,
    deep: false,
    createdAt: "2026-09-09T10:00:00Z",
    updatedAt: "2026-09-09T10:00:00Z",
    ...over,
  };
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

/**
 * 动作条里的按钮（挂在事件表区块标题行内，见 ActionBar）。
 *
 * 必须限定在 `backup-actions` 内：① 「从 URL 导入」/「上传本机文件」在内联导入区另有同名标题；
 * ② 内联导入区的标题文案与按钮同名（「从 URL 导入」/「上传本机文件」）。
 * 不限定就会命中错的那个。
 */
function actionButton(name: string | RegExp): HTMLElement {
  return within(screen.getByTestId("backup-actions")).getByRole("button", { name });
}

/**
 * 等动作条里的按钮出现后再返回它。
 *
 * 动作条随页面首屏一起渲染，但部分用例要等数据就绪（例如冻结状态回来后按钮才换文案），
 * 故统一走异步等待，避免与首载赛跑。
 */
async function actionButtonAsync(name: string | RegExp): Promise<HTMLElement> {
  await screen.findByTestId("backup-actions");
  return actionButton(name);
}

/**
 * 统一事件表里的数据行（不含表头），并先等行数到位。
 *
 * 必须等到位再断言顺序：备份包与导入记录两份数据各自异步到达，若在只到一半时读行，
 * "顺序"断言就在一个不完整的序列上做结论（既可能错报红，也可能让"没排序"蒙混过关）。
 * 行数由调用方给出，正是为了强迫用例明确"我期待几条"。
 */
async function eventRows(expected: number): Promise<HTMLTableRowElement[]> {
  const table = await screen.findByTestId("backup-event-table");
  const read = () => Array.from(table.querySelectorAll<HTMLTableRowElement>("tbody tr"));
  await waitFor(() => expect(read().length).toBe(expected));
  return read();
}

/**
 * 行标识：取标识单元格第一行的等宽文本。
 *
 * 第二行是「类型徽章 + 摘要（备注 / 内容规模 / 来源地址）」，直接读 td.textContent 会把
 * 徽章与摘要一起拼进来，断言"顺序"时就分不清到底是标识在换位还是摘要在换位。
 * 标识单元格的第一个 Text 节点就是标识（IdentityCell 先渲染 id、再渲染第二行）。
 */
function rowId(row: HTMLElement): string {
  const cell = within(row).getAllByRole("cell")[0];
  const mono = cell.querySelector(".mantine-Text-root");
  return (mono?.textContent ?? "").trim();
}

/**
 * 元素（或其祖先）是否被 display:none 隐藏——用于断言"不可见"而不依赖 jsdom 的布局。
 * jsdom 没有布局引擎，`toBeVisible` 之类的判据不可用，只能沿 display 链判断。
 */
function isHidden(el: HTMLElement): boolean {
  for (let node: HTMLElement | null = el; node; node = node.parentElement) {
    if (node.style.display === "none") return true;
  }
  return false;
}

/** 按标识取事件表里的一行（标识文本在标识单元格第一行，全表唯一）。 */
async function eventRow(id: string): Promise<HTMLElement> {
  const found = await screen.findByText(id);
  return found.closest("tr") as HTMLElement;
}

/** 关键事实条里的单个事实块（图标 + 标签 + 取值）。 */
function factBlock(facts: HTMLElement, label: string): HTMLElement {
  const block = within(facts).getByText(label).closest(".mantine-Stack-root") as HTMLElement;
  expect(block).toBeTruthy();
  return block;
}

// devmock 混排后的期望顺序（按 createdAt 倒序）：
//   2 个本机包（6h / 12h）在前，3 条导入记录（20h / 24h / 48h）在后。
// 刻意把两条包的标识写成"看起来比导入记录旧"的形式（bk-20260910 vs imp-20260909），
// 使"按标识排序"或"按前缀分块"这类错误实现无法靠巧合通过。
const EXPECTED_EVENT_IDS = [
  "bk-20260910-090000-a1b2c3",
  "bk-20260910-030000-d4e5f6",
  "imp-20260909-160000-g7h8i9",
  "imp-20260909-100000-d4e5f6",
  "imp-20260908-140000-a1b2c3",
] as const;

const EXPECTED_EVENT_KINDS = ["package", "package", "import", "import", "import"] as const;

describe("备份与搬迁", () => {
  it("锁定视口高度：外层不滚动，列表在剩余高度内滚动", async () => {
    renderTab();

    const page = await screen.findByTestId("migrations-page");
    expect(page.style.height).toContain("calc(100dvh");
    expect(page.style.overflow).toBe("hidden");
  });

  // —— 任务台信息架构：动作收进事件表标题行 → 关键事实 → 统一事件表 ——
  it("动作条挂在事件表区块标题行内，页面上不再有独立动作区，事件表独占剩余高度", async () => {
    renderTab();
    await screen.findByTestId("backups-tab");

    const actions = screen.getByTestId("backup-actions");
    const inline = screen.getByTestId("import-inline");
    const facts = screen.getByTestId("backup-facts");
    const listSection = screen.getByTestId("backup-list-section");

    // 本次的核心结构变化：动作条（原两张常驻卡，约 173px）移进事件表区块的标题行。
    // 一旦有人把它挪回页面流里独立成块，这里就转红。
    expect(listSection.contains(actions)).toBe(true);

    // 剩余顺序不变：（内联导入区）→ 关键事实 → 事件表。
    for (const [before, after] of [
      [inline, facts],
      [facts, listSection],
    ] as const) {
      expect((before.compareDocumentPosition(after) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0).toBe(
        true,
      );
    }

    // 只有事件表吃剩余高度；内联区与事实条固定高度（flexShrink:0）。
    // 这是"展开任何表单都不会把列表换成空白"的结构前提——它们一旦参与伸缩，
    // 就会回到"展开即挤压"的老毛病。
    expect(listSection.style.flex).toContain("1");
    for (const section of [inline, facts]) {
      expect((section.parentElement as HTMLElement).style.flexShrink).toBe("0");
    }

    // 已删除的旧结构都不得回归：状态条、页面内的导入区块、待处理条、两张动作卡。
    expect(screen.queryByTestId("backup-status-strip")).toBeNull();
    expect(screen.queryByTestId("backup-import-section")).toBeNull();
    expect(screen.queryByTestId("backup-attention-banner")).toBeNull();
    expect(screen.queryByTestId("backup-attention-clear")).toBeNull();
  });

  it("统一事件表列头固定为八列，标识单元格第二行给出备注与内容规模", async () => {
    renderTab();

    // 事件表是页面里唯一一张表（下面"同一张表承载两类"另有专门守卫）。
    const table = await screen.findByTestId("backup-event-table");
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["标识", "来源", "方式", "状态", "大小", "创建时间", "错误", "操作"]);

    // 原「包含」列已并入标识单元格第二行——信息一条没丢，但不再占一列宽度。
    const packageRow = within(table).getByText("bk-20260910-090000-a1b2c3").closest("tr");
    const identityCell = within(packageRow as HTMLElement).getAllByRole("cell")[0];
    expect(within(identityCell).getByText(/例行备份/)).toBeTruthy();
    expect(within(identityCell).getByText(/74481 制品 · 20 仓库/)).toBeTruthy();

    // 操作列只有一个「⋯」按钮：带文字的行内动作按钮会需要约 207px 而表格挤不出，
    // 结果是按钮被 flex 压成残片（可读性丧失）或整表横向滚动。守护"只有一个按钮"。
    const rowButtons = within(packageRow as HTMLElement).getAllByRole("button");
    expect(rowButtons.length).toBe(1);
    expect(rowButtons[0].getAttribute("aria-label")).toBe("更多操作");
  });

  // —— FR-40：来源列（手动 / 自动）——
  it("来源列区分手动与自动备份", async () => {
    renderTab();

    const table = await screen.findByTestId("backup-event-table");

    // 种子同时含手动包与自动包，两种徽章文案都要出现——
    // 只断言其中一个会让"两种来源都渲染成同一文案"的漂移蒙混过关。
    expect(within(table).getByText("手动")).toBeTruthy();
    expect(within(table).getByText("自动")).toBeTruthy();

    // 自动包的备注（周期自动备份）与来源徽章在同一行。
    const autoRow = within(table)
      .getByText("bk-20260910-030000-d4e5f6")
      .closest("tr") as HTMLElement;
    expect(within(autoRow).getByText("自动")).toBeTruthy();
    expect(within(autoRow).getByText(/周期自动备份/)).toBeTruthy();
    const manualRow = within(table)
      .getByText("bk-20260910-090000-a1b2c3")
      .closest("tr") as HTMLElement;
    expect(within(manualRow).getByText("手动")).toBeTruthy();
  });

  // —— FR-40 追加四：本机包与导入记录混排进同一张表（本次重构的核心行为）——
  it("统一事件表把本机包与导入记录按创建时间倒序混排（种子数据的期望次序）", async () => {
    renderTab();
    const rows = await eventRows(5);

    // ① 行类型序列：2 个包在前、3 条导入记录在后。
    expect(rows.map((row) => row.dataset.rowKind)).toEqual([...EXPECTED_EVENT_KINDS]);

    // ② 标识顺序：种子时间戳（6h / 12h / 20h / 24h / 48h）决定了这个次序。
    //    **注意这条本身不是"排序正确"的充分判据**：种子恰好让两类各自有序、且包全比记录新，
    //    因此"两类分块拼接、完全不排序"也会长成这样。真正能证伪分块拼接的是下面那条
    //    交错用例（时间戳刻意交错），两条合起来才守得住"按时间混排"。
    expect(rows.map((row) => rowId(row))).toEqual([...EXPECTED_EVENT_IDS]);
  });

  it("两类事件的时间戳交错时仍严格按时间倒序（分块拼接/不排序都会在此转红）", async () => {
    // 这条是"混排"的真判据。刻意造出**交错**的时间戳：
    //   包   5h、30h          导入记录 10h、40h
    // 期望次序只有一种：包5h → 记录10h → 包30h → 记录40h。
    // 逐条证伪：
    //   - 分块拼接（包的都先、记录的都接上）→ 包5h → 包30h → 记录10h → 记录40h ✗
    //   - 完全不排序（沿用各自列表的到达顺序）→ 同上 ✗
    //   - 只在各类内部排序再拼接 → 同上 ✗
    //   - 升序排列 → 记录40h → … ✗
    // 且每个来源列表内部**本身是有序的**（与后端 `ORDER BY created_at DESC` 一致），
    // 所以"实现了排序"才是唯一能通过的解释。
    const earlier = (hours: number) => new Date(Date.now() - hours * 3_600_000).toISOString();
    vi.spyOn(endpoints, "listBackups").mockResolvedValue({
      items: [
        buildPackage({ packageId: "bk-new-5h", createdAt: earlier(5) }),
        buildPackage({ packageId: "bk-old-30h", createdAt: earlier(30) }),
      ],
      total: 2,
    });
    vi.spyOn(endpoints, "listBackupImports").mockResolvedValue({
      items: [
        buildImport({ importId: "imp-new-10h", createdAt: earlier(10) }),
        buildImport({ importId: "imp-old-40h", createdAt: earlier(40) }),
      ],
      total: 2,
    });

    renderTab();
    const rows = await eventRows(4);

    expect(rows.map((row) => row.dataset.rowKind)).toEqual([
      "package",
      "import",
      "package",
      "import",
    ]);
    expect(rows.map((row) => rowId(row))).toEqual([
      "bk-new-5h",
      "imp-new-10h",
      "bk-old-30h",
      "imp-old-40h",
    ]);
  }, 20_000);

  it("全页面只有一张事件表承载两类对象，没有独立的导入记录表", async () => {
    renderTab();
    await eventRows(5);

    // 只有一张表：既没有"页面里一张 + 别处一张"，也没有把导入记录另放一处。
    const tables = screen.getAllByRole("table");
    expect(tables).toHaveLength(1);
    expect(tables[0]).toBe(screen.getByTestId("backup-event-table"));
    expect(screen.getAllByTestId("backup-event-table")).toHaveLength(1);

    // 两类行都在同一张表里（不是"各占一张表"的等价值写法）。
    const kinds = Array.from(tables[0].querySelectorAll<HTMLTableRowElement>("tbody tr")).map(
      (row) => row.dataset.rowKind,
    );
    expect(kinds.filter((kind) => kind === "package")).toHaveLength(2);
    expect(kinds.filter((kind) => kind === "import")).toHaveLength(3);

    // 动作条里的两个导入按钮都在：「导入记录」入口与角标随记录表并入事件表一并删除。
    const actions = screen.getByTestId("backup-actions");
    expect(within(actions).getByTestId("action-import-url")).toBeTruthy();
    expect(within(actions).getByTestId("action-import-upload")).toBeTruthy();
    expect(within(actions).queryByRole("button", { name: /导入记录/ })).toBeNull();
    expect(screen.queryByTestId("import-entry-badge")).toBeNull();
  });

  it("每行标识第二行带类型徽章：包行「本机包」、导入行「导入记录」，两类各出现正确次数", async () => {
    renderTab();
    const rows = await eventRows(5);

    const packageRows = rows.filter((row) => row.dataset.rowKind === "package");
    const importRows = rows.filter((row) => row.dataset.rowKind === "import");
    expect(packageRows).toHaveLength(2);
    expect(importRows).toHaveLength(3);

    // 不靠标识前缀（bk- / imp-）让人猜：徽章必须逐行把"这是什么"说清楚。
    for (const row of packageRows) {
      const badge = within(row).getByTestId("event-type-package");
      expect(badge.textContent).toBe("本机包");
      // outline 变体：类型回答的是"这是什么"，不能与"状态""来源"两个维度共色相混。
      expect(badge.getAttribute("data-variant")).toBe("outline");
    }
    for (const row of importRows) {
      const badge = within(row).getByTestId("event-type-import");
      expect(badge.textContent).toBe("导入记录");
      expect(badge.getAttribute("data-variant")).toBe("outline");
    }

    // 计数：徽章数与行数一致（防止"只在第一行渲染徽章"这类半成品）。
    expect(screen.getAllByTestId("event-type-package")).toHaveLength(2);
    expect(screen.getAllByTestId("event-type-import")).toHaveLength(3);
  });

  it("导入请求是就地展开表单，不再是弹窗（弹窗回归即等于本次重构被推翻）", async () => {
    const user = userEvent.setup();
    renderTab();

    await user.click(await actionButtonAsync("从 URL 恢复"));

    // 注意：测试环境 AppProvider 传 env="test"，Mantine 的 OptionalPortal 在该环境下
    // 刻意不 portal，弹窗若真的存在一定能被 role 查到——所以这个 null 断言是有效的，
    // 不会因为"弹窗被渲染到别处"而假通过。
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByTestId("import-inline").style.display).not.toBe("none");
  });

  it("内联导入区：初始收起（display:none）→ 点按钮展开 URL 通道 → 再点同一次收起", async () => {
    const user = userEvent.setup();
    renderTab();

    const inline = await screen.findByTestId("import-inline");
    // 收起态：整块仍在 DOM 里、只是 display:none（不是条件渲染——见 ImportInline 文件头，
    // 分片上传面板要在挂载时探测未完成的上传会话，条件卸载会让续传探测永远跑不到）。
    expect(inline.style.display).toBe("none");

    await user.click(await actionButtonAsync("从 URL 恢复"));
    expect(inline.style.display).not.toBe("none");
    // 两个通道都常驻挂载，判据只能是"哪个可见"，不能是"哪个存在"。
    expect(screen.getByTestId("import-channel-url").style.display).not.toBe("none");
    expect(screen.getByTestId("import-channel-upload").style.display).toBe("none");
    // 展开中的按钮转 light，表明"表单就在这里"。
    expect(actionButton("从 URL 恢复").getAttribute("data-variant")).toBe("light");

    // 再点同一个按钮 = 撤销（收起），不是幂等的"再次展开"。
    await user.click(actionButton("从 URL 恢复"));
    expect(inline.style.display).toBe("none");
    expect(actionButton("从 URL 恢复").getAttribute("data-variant")).toBe("filled");
  });

  it("内联导入区：点「上传备份包」切到上传通道，URL 通道转为隐藏", async () => {
    const user = userEvent.setup();
    renderTab();

    await user.click(await actionButtonAsync("上传备份包"));

    expect(screen.getByTestId("import-inline").style.display).not.toBe("none");
    expect(screen.getByTestId("import-channel-upload").style.display).not.toBe("none");
    expect(screen.getByTestId("import-channel-url").style.display).toBe("none");
    expect(actionButton("上传备份包").getAttribute("data-variant")).toBe("light");

    // 切到 URL 再切回来：切换只改可见性，两个通道的节点都不被卸载（已填内容不丢）。
    await user.click(actionButton("从 URL 恢复"));
    expect(screen.getByTestId("import-channel-url").style.display).not.toBe("none");
    expect(screen.getByTestId("import-channel-upload").style.display).toBe("none");
  });

  it("上传面板常驻挂载：从未点开上传通道也能查到选文件 input（续传探测的前提）", async () => {
    // 这条守的是一个具体的坑：分片上传面板在**挂载时**读 localStorage 里的会话并 GET 拉回
    // 已落盘分片（续传探测）。若把 import-inline 或上传通道改成条件渲染，探测就永远跑不到，
    // "只补缺失片"的能力整个失效，而后续用例（收起重开仍能发现会话）仍在同一份 state 上
    // 通过，抓不到。故这里刻意**一次都不点开上传通道**，直接断言节点已在 DOM 里。
    renderTab();
    await screen.findByTestId("backups-tab");

    expect(screen.getByTestId("import-inline").style.display).toBe("none");
    const input = within(screen.getByTestId("import-channel-upload")).getByTestId(
      "backup-upload-input",
    );
    expect((input as HTMLInputElement).type).toBe("file");
    // 节点存在但整条祖先链是隐藏的：正是"常驻挂载 + display:none"而非"条件渲染"。
    // 若改成条件渲染，上面两行会先抛"找不到元素"，这条用例随即转红。
    expect(isHidden(input)).toBe(true);
  });

  it("pending_restart 的「需重启服务后生效」跟着状态走，落在状态单元格里", async () => {
    // 它原先在导入记录表独立的「进度」列里；合并成一张表后没有进度列，提示必须随状态
    // 一起搬进状态单元格——否则"包导进来了但要重启才生效"这件事会在界面上凭空消失。
    renderTab();
    const row = await eventRow("imp-20260908-140000-a1b2c3");

    const statusCell = within(row).getAllByRole("cell")[3];
    expect(within(statusCell).getByText("待重启")).toBeTruthy();
    expect(within(statusCell).getByText("需重启服务后生效")).toBeTruthy();

    // 反向判据：别的行不该带这句提示（避免"给每行都渲染一次"这种假通过）。
    const doneRow = await eventRow("imp-20260909-100000-d4e5f6");
    expect(within(doneRow).queryByText("需重启服务后生效")).toBeNull();
  });

  it("导入记录行有状态与错误摘要，但没有可执行动作（契约里没有删记录端点）", async () => {
    renderTab();
    const row = await eventRow("imp-20260909-160000-g7h8i9");
    const cells = within(row).getAllByRole("cell");

    // 状态徽章（StatusPill）显示「失败」，错误列给出摘要（同一行可读到）。
    expect(within(cells[3] as HTMLElement).getByText("失败")).toBeTruthy();
    expect(within(cells[6] as HTMLElement).getByText(/schemaVersion/)).toBeTruthy();

    // 操作列是「—」且整行没有按钮：记录本身是导入流水线的账，不提供删除等动作。
    // （若哪天给记录加了动作，「⋯」会先在这里露头。）
    expect((cells[7] as HTMLElement).textContent).toBe("—");
    expect(within(row).queryByRole("button")).toBeNull();
  });

  // —— FR-40 追加二：空状态文案专属（原属导入记录表，现属统一事件表）——
  it("两类事件都为空时给出统一事件表专属文案", async () => {
    vi.spyOn(endpoints, "listBackups").mockResolvedValue({ items: [], total: 0 });
    vi.spyOn(endpoints, "listBackupImports").mockResolvedValue({ items: [], total: 0 });

    renderTab();

    // 文案落在事件表区块内，且说清"事件会按时间出现在这里"——这是空表的引导，
    // 不是备份包空列表的旧文案。
    const listSection = screen.getByTestId("backup-list-section");
    expect(await within(listSection).findByText("暂无备份与导入记录")).toBeTruthy();
    expect(
      within(listSection).getByText(/生成一个备份包，或从 URL 导入 \/ 上传一个包/),
    ).toBeTruthy();
    expect(within(listSection).getByText(/事件会按时间出现在这里/)).toBeTruthy();

    // 真的空了就不该有表（空态替换表格，而不是渲染一张只有表头的表）。
    expect(screen.queryByTestId("backup-event-table")).toBeNull();
  }, 20_000);

  // —— 任务台信息架构：第一段「动作」——
  it("动作条给出六个操作按钮，两条主路径的说明迁到「说明」气泡里", async () => {
    const user = userEvent.setup();
    renderTab();

    const actions = await screen.findByTestId("backup-actions");
    // 两张卡收成一行按钮后，六个操作都还在（少一个都是功能入口丢失）。
    // 「上传包恢复」在 FR-40 追加八改名为「上传备份包」：上传不再直接恢复，
    // 只把包收进列表；恢复是入库之后的独立动作。
    // 「导入未管理的包」是新增入口，对应服务器 backups/ 下有包体但无登记的包。
    for (const name of [
      "生成备份包",
      "冻结写入",
      "从 URL 恢复",
      "上传备份包",
      "导入未管理的包",
      "自动备份设置",
    ]) {
      expect(within(actions).getByRole("button", { name })).toBeTruthy();
    }

    // 原卡里的两条说明文字不再常驻占高度，改挂「说明」气泡——必须仍能被用户读到，
    // 否则这次收编就变成了"删说明"。顺带守住冻结态下换成那句更有用的提示。
    // 该按钮的可访问名是它的 aria-label（等于气泡标题），不是可见的「说明」两字。
    await user.click(within(actions).getByRole("button", { name: "备份与搬迁怎么用" }));
    expect(await screen.findByText("立即备份")).toBeTruthy();
    expect(screen.getByText("搬迁到新机器")).toBeTruthy();
    expect(screen.getByText(/冻结写入后打一份完整的快照包/)).toBeTruthy();
    expect(screen.getByText(/把已有备份包传到目标实例/)).toBeTruthy();
  }, 20_000);

  it("冻结状态由「冻结/解冻写入」按钮自身体现，不再有独立的常驻状态徽章", async () => {
    // 这条替代了原先「卡常驻显示状态（可写）」的守卫，因为状态展示方式变了：
    // 状态与按钮本就一一对应（可写 ↔ 「冻结写入」、已冻结 ↔ 「解冻写入」），
    // 动作条收成一行按钮后不再单列一只 StatusPill。
    // 仍要守住的是**解冻入口必须可达**——冻结期间若没有它，就会形成
    // 「已冻结 → 按钮禁用 → 打不开弹窗 → 永远解不了冻」的死锁。
    renderTab();

    const actions = await screen.findByTestId("backup-actions");
    expect(within(actions).getByRole("button", { name: "冻结写入" })).toBeTruthy();
    expect(within(actions).queryByRole("button", { name: "解冻写入" })).toBeNull();
  });

  // —— 待重启生效（FR-40 追加六）：让"为什么导入不了"可见 ——
  it("有待生效恢复时导入按钮转橙并可点开说明，重启命令可直接复制", async () => {
    // 这条守卫的根因：导入成功后只写 restore.pending，后续导入一律 409 拒绝；
    // 而该状态此前只体现在导入记录行的 pending_restart 徽章上——徽章被更近的事件
    // 挤出视野后，用户只看到"导入一直被拒"却看不出原因。
    //
    // FR-40 追加八后范围收窄：只有「从 URL 恢复」会写 restore.pending，
    // 因此只有它需要挡住并解释；「上传备份包」与「导入未管理的包」只入库、
    // 不写 pending，待重启时仍可正常使用（见下方断言）。
    const user = userEvent.setup();
    vi.spyOn(endpoints, "getBackupRestoreStatus").mockResolvedValue({
      pending: true,
      packageId: "bk-20260101-000000-abc123",
      origin: "url",
      stagedAt: "2026-01-01T00:00:00Z",
    });
    renderTab();

    const actions = await screen.findByTestId("backup-actions");
    const urlBtn = within(actions).getByTestId("action-import-url");
    const upBtn = within(actions).getByTestId("action-import-upload");
    const adoptBtn = within(actions).getByTestId("action-import-adopt");

    // URL 导入按钮必须带说明（title/aria），否则用户只看到"变橙了"却不知为何。
    expect(urlBtn.getAttribute("title")).toContain("待重启");
    // 上传与导入未管理包不写 pending，因此不该被待重启状态标记或挡住。
    expect(upBtn.getAttribute("title")).toBeNull();
    expect(adoptBtn.getAttribute("title")).toBeNull();

    // 点击不是展开表单（展开了提交也必被拒），而是解释原因。
    await user.click(urlBtn);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/需重启服务后才会替换本机数据/)).toBeTruthy();
    // 必须给出**可照做**的重启命令，并显示待生效的包是哪一份。
    expect(within(dialog).getByText(/systemctl restart jianartifact/)).toBeTruthy();
    expect(within(dialog).getByText(/bk-20260101-000000-abc123/)).toBeTruthy();
    // 一键重启入口也必须在这里（实机验收补上的）：该状态跨页面刷新一直存在，
    // 用户重开页面后从橙色按钮进到的就是这个弹窗；若只有一条要自己复制的命令，
    // 一键重启在最需要它的场景里等于不存在。
    expect(within(dialog).getByTestId("restart-service")).toBeTruthy();
    // 内联表单不得被打开：此时展开只会诱导用户去提交一个注定被拒的表单。
    expect(screen.getByTestId("import-inline").style.display).toBe("none");
  }, 20_000);

  it("无待生效恢复时导入按钮保持原样（不常驻打扰）", async () => {
    vi.spyOn(endpoints, "getBackupRestoreStatus").mockResolvedValue({ pending: false });
    renderTab();

    const actions = await screen.findByTestId("backup-actions");
    const urlBtn = within(actions).getByTestId("action-import-url");
    // 没有待重启时不得挂提示——该状态是临时的，常驻提示会白占注意力
    // （用户此前正是以"这个汇总提示不要"为由删掉了待处理条）。
    expect(urlBtn.getAttribute("title")).toBeNull();
    // 且仍能正常展开表单。
    const user = userEvent.setup();
    await user.click(urlBtn);
    expect(screen.getByTestId("import-inline").style.display).not.toBe("none");
  }, 20_000);

  it("「自动备份设置」入口必须可达，且打开后三项配置在弹窗里", async () => {
    // 这条专门防"重做布局时把已交付功能的入口整个弄丢"：FR-40 的
    // 「间隔 / 保留份数 / 保留天数可在后台修改」全靠这个入口进入，
    // 入口一旦消失，功能在界面上等于不存在（而其它用例只测弹窗内容，抓不到）。
    const user = userEvent.setup();
    renderTab();

    const actions = await screen.findByTestId("backup-actions");
    await user.click(within(actions).getByRole("button", { name: "自动备份设置" }));

    const dialog = await screen.findByRole("dialog");
    expect(await within(dialog).findByTestId("auto-interval")).toBeTruthy();
    expect(within(dialog).getByTestId("keep-last")).toBeTruthy();
    expect(within(dialog).getByTestId("retention-days")).toBeTruthy();

    // 三项输入**不在**事件表区块内：本次 IA 重排的核心就是"配置不再常驻页面流"。
    // （这里不断言 `tab.contains(dialog)`：测试环境 AppProvider 传 env="test"，
    // Mantine 的 OptionalPortal 在该环境下刻意不 portal，容器关系与生产不同；
    // 而"配置项是否在列表区块内"才是与布局无关、且真正表达本次改造意图的判据。）
    const listSection = screen.getByTestId("backup-list-section");
    expect(listSection.contains(within(dialog).getByTestId("auto-interval"))).toBe(false);
    // 事件表区块仍在拿 flex:1 —— 弹窗开着也不改变它（Modal 不参与页面 flex 布局）。
    expect(listSection.style.flex).toContain("1");
  }, 20_000);

  it("打开配置弹窗后，事件表区块仍存在且未被挤掉", async () => {
    const user = userEvent.setup();
    renderTab();

    // 打开配置弹窗（此前这一步会把列表压到 0 行可见）。
    await user.click(await screen.findByRole("button", { name: "自动备份设置" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByTestId("auto-interval")).toBeTruthy();

    // 事件表区块仍是 flex:1（Modal 不在布局流内，弹窗开着也不影响它）。
    const listSection = screen.getByTestId("backup-list-section");
    expect(listSection).toBeTruthy();
    expect(listSection.style.flex).toContain("1");

    // 事件表本身也还在（弹窗没有把页面内容替换掉）。
    expect(within(listSection).getByTestId("backup-event-table")).toBeTruthy();
  }, 20_000);

  it("配置弹窗内可编辑三项配置，保存后提交到 settings 通道", async () => {
    const user = userEvent.setup();
    renderTab();

    await user.click(await screen.findByRole("button", { name: "自动备份设置" }));
    const dialog = await screen.findByRole("dialog");

    // 三项输入都在，且初值来自 settings（mock 里为 0 / 7 / 30）。
    const interval = await within(dialog).findByTestId("auto-interval");
    const keepLast = within(dialog).getByTestId("keep-last");
    const retentionDays = within(dialog).getByTestId("retention-days");
    expect((interval as HTMLInputElement).value).toBe("0");
    expect((keepLast as HTMLInputElement).value).toBe("7");
    expect((retentionDays as HTMLInputElement).value).toBe("30");

    // 未改动时保存按钮禁用：避免用户误以为"点了没反应"。
    const saveBtn = within(dialog).getByRole("button", { name: "保存" });
    expect(saveBtn.hasAttribute("disabled")).toBe(true);

    // 改间隔 → 保存按钮变可用 → 点击保存 → 成功提示出现。
    await user.clear(interval);
    await user.type(interval, "60");
    await waitFor(() => {
      expect(within(dialog).getByRole("button", { name: "保存" }).hasAttribute("disabled")).toBe(
        false,
      );
    });
    await user.click(within(dialog).getByRole("button", { name: "保存" }));
    await waitFor(
      () => {
        expect(screen.getByText(/自动备份配置已保存/)).toBeTruthy();
      },
      { timeout: 10_000 },
    );

    // 保存后回读：新值必须真的落到设置里。此前 devmock 的 PUT /settings 不透传这三项，
    // 保存只会弹成功提示、值却原样退回，用例因此必须断言"回读到的就是新值"。
    await waitFor(() => {
      expect((within(dialog).getByTestId("auto-interval") as HTMLInputElement).value).toBe("60");
    });

    // 「保存即生效，无需重启」必须可见（卡改弹窗后该提示不能丢）。
    expect(within(dialog).getByText("保存即生效，无需重启")).toBeTruthy();
  }, 20_000);

  it("自动备份配置的 0 值不被误当成空值", async () => {
    const user = userEvent.setup();
    renderTab();

    // 间隔 0 是有意义的取值（关闭）。它有两个可观察面，都要断言：
    //   ① 关键事实条如实显示「已关闭」（见下方"关键事实条"用例的同一判据）；
    //   ② 配置弹窗里的输入就是 0 本身，而不是被 `|| 默认值` 归一化后的 7。
    await user.click(await screen.findByRole("button", { name: "自动备份设置" }));
    const dialog = await screen.findByRole("dialog");
    const interval = (await within(dialog).findByTestId("auto-interval")) as HTMLInputElement;
    expect(interval.value).toBe("0");
  }, 20_000);

  // —— 任务台信息架构：第二段「关键事实」——
  it("关键事实条五个标签齐全，间隔 0 时「下次自动备份」显示「已关闭」而不是某个时间", async () => {
    renderTab();

    const facts = await screen.findByTestId("backup-facts");
    for (const label of ["最近成功备份", "下次自动备份", "最近校验", "占用", "保留策略"]) {
      expect(within(facts).getByText(label)).toBeTruthy();
    }

    // 0 是"关闭"的语义，不是"每 0 秒"、也不是一个推算出来的时刻。
    // 这条守卫的根因：间隔 0 若被 `||` 兜底或被当成有效间隔去推算，用户会以为
    // 自动备份在跑（实际是关的），或看到一个假的"下次执行时间"。
    const nextAuto = factBlock(facts, "下次自动备份");
    expect(within(nextAuto).getByText("已关闭")).toBeTruthy();
    expect(within(nextAuto).queryByText(/每 0 秒/)).toBeNull();
    expect(within(nextAuto).queryByText(/\d{1,2}:\d{2}/)).toBeNull();

    // 保留策略摘要（原常驻状态条上的"保留最近 N 份 · N 天内"）迁到本事实条后必须仍在。
    // mock 的份数 7、天数 30，间隔 0 → 必须明说「已关闭」而不是"每 0 秒备份一次"。
    const policy = factBlock(facts, "保留策略");
    expect(within(policy).getByText(/已关闭/)).toBeTruthy();
    expect(within(policy).getByText(/保留最近 7 份/)).toBeTruthy();
    expect(within(policy).getByText(/30 天内/)).toBeTruthy();

    // 「最近校验」取自落库的演练报告（devmock 预置一条失败报告），
    // 不是借 /verify 的即时结论冒充历史——所以这里必须如实显示「未通过」。
    const lastVerify = factBlock(facts, "最近校验");
    await waitFor(
      () => {
        expect(within(lastVerify).getByText(/未通过/)).toBeTruthy();
      },
      { timeout: 10_000 },
    );
  }, 20_000);

  it("「下次自动备份」在算出的时刻已过、等心跳来跑时显示「即将执行」，不得说成「等待首次执行」", async () => {
    // 这条守卫的根因（实机验收发现）：`factNextPending`（"等待首次执行"）被同时用在了
    // 两个语义相反的场合——① 确实还没有自动包；② 算出的下次时刻已过、等下一轮心跳。
    // ② 的场景下实例上**已经有自动包**了，显示"等待首次执行"是假话；且 30 秒这类短间隔
    // 下几乎总走 ②，用户会看到一个自相矛盾的结论。故两者必须分开。
    const longAgo = new Date(Date.now() - 12 * 3_600_000).toISOString();
    vi.spyOn(endpoints, "listBackups").mockResolvedValue({
      items: [
        buildPackage({
          packageId: "bk-manual-6h",
          createdAt: new Date(Date.now() - 6 * 3_600_000).toISOString(),
        }),
        // 自动包存在但很旧 → 间隔 60s 下"下次"必然已过点。
        buildPackage({ packageId: "bk-auto-12h", origin: "auto", createdAt: longAgo }),
      ],
      total: 2,
    });
    vi.spyOn(endpoints, "listBackupImports").mockResolvedValue({ items: [], total: 0 });
    // 显式给全字段：`vi.spyOn` 已把原方法换掉，mock 内部再调它只会调到自己。
    vi.spyOn(endpoints, "getSettings").mockResolvedValue({
      anonymousAccess: false,
      publicUrl: "",
      upstreamTimeout: 30,
      syncInterval: 60,
      allowedHosts: [],
      originTokenEnabled: false,
      originTokenHeader: "",
      originTokenValue: "",
      backupAutoInterval: 60,
      backupKeepLast: 7,
      backupRetentionDays: 30,
    });

    renderTab();

    const facts = await screen.findByTestId("backup-facts");
    const nextAuto = factBlock(facts, "下次自动备份");
    await waitFor(() => {
      expect(within(nextAuto).getByText("即将执行")).toBeTruthy();
    });
    // 反向断言：这一支下绝不能出现"等待首次执行"（实例上明明有自动包）。
    expect(within(nextAuto).queryByText("等待首次执行")).toBeNull();
    // 也不能凭空给出一个未来时刻（已过点，就没有"下次时刻"可说）。
    expect(within(nextAuto).queryByText(/\d{1,2}:\d{2}/)).toBeNull();
  }, 20_000);

  // —— FR-137：从 URL 导入（内联表单，提交后记录直接落在统一事件表里）——
  it("从 URL 导入提交后事件表出现新记录，且为 pending_restart（含「需重启」提示）", async () => {
    const user = userEvent.setup();
    renderTab();

    // 入口就地展开 URL 表单——不需要用户自己先找记录表再切标签（那是弹窗时代的动作）。
    await user.click(await actionButtonAsync("从 URL 恢复"));
    const urlChannel = screen.getByTestId("import-channel-url");
    expect(urlChannel.style.display).not.toBe("none");

    await user.type(
      within(urlChannel).getByPlaceholderText("https://example.com/backups/bk.tar.gz"),
      "https://example.com/backups/bk.tar.gz",
    );
    await user.click(within(urlChannel).getByRole("button", { name: "开始导入" }));

    // 受理记录按 queued → fetching → staging → pending_restart 推进，最终态由事件表的轮询可见。
    // 全程不需要任何"切标签"动作：新记录直接按事件时间出现在表里，这是混排的收益所在。
    //
    // 必须限定在事件表内查找：受理后**恢复进度弹窗会自动打开**，而弹窗的状态行也渲染同一个
    // importId。不限定作用域就会同时命中两处（表格单元格与弹窗里的 Code），断言随即因
    // "找到多个元素"失败——而那时功能其实完全正常，正是这里要守的场景。
    const table = await screen.findByTestId("backup-event-table");
    const newId = await within(table).findByText(/^imp-\d{14}-\d{3}$/);
    const row = newId.closest("tr") as HTMLElement;
    await waitFor(
      () => {
        expect(within(row).getByText("待重启")).toBeTruthy();
      },
      { timeout: 15_000 },
    );
    expect(within(row).getByText("需重启服务后生效")).toBeTruthy();
    // 新行是导入记录（不是被误当成包），且操作列没有动作。
    expect(row.dataset.rowKind).toBe("import");
    expect(within(row).queryByRole("button")).toBeNull();
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

    // 「生成备份包」现在就在事件表区块头（动作条挂在那里），不存在重复按钮。
    await user.click(await actionButtonAsync("生成备份包"));

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

  // —— FR-134：写入冻结窗口（状态与解冻入口都在动作条上）——
  it("初始可写（动作条给「冻结写入」）；冻结后转「解冻写入」并把自动解冻时间挂进标题", async () => {
    const user = userEvent.setup();
    renderTab();

    // 初始未冻结：动作条给的是「冻结写入」，且没有解冻入口。
    await screen.findByTestId("backup-actions");
    await waitFor(() => expect(actionButton("冻结写入")).toBeTruthy());
    expect(actionButton("冻结写入").textContent).toBe("冻结写入");
    expect(screen.queryByRole("button", { name: "解冻写入" })).toBeNull();

    // 打开冻结 Modal，选择有界档位后确认。
    await user.click(actionButton("冻结写入"));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByText("4 小时"));
    await user.click(within(dialog).getByRole("button", { name: "冻结" }));

    // 冻结成功后：动作条换成「解冻写入」（橙色 light 变体表明已冻结状态）。
    // 待处理条已随本次重构删除，冻结态的可观察面就是按钮本身——不再有"计数推到 3"这类断言可守。
    await waitFor(
      () => {
        expect(actionButton("解冻写入")).toBeTruthy();
      },
      { timeout: 5_000 },
    );
    const unfreeze = actionButton("解冻写入");
    // 状态不再单列一只常驻徽章，但仍要如实可读：自动解冻时间挂在按钮的 title 上。
    // 用 formatStamp 过的可读时刻，而不是裸 ISO（裸时间戳对人没有意义）。
    expect(unfreeze.getAttribute("title")).toMatch(/自动解冻时间/);
    expect(unfreeze.getAttribute("title")).not.toMatch(/T\d{2}:\d{2}:\d{2}/);
  }, 20_000);

  it("解冻需二次确认，确认后回到可写（解冻入口不在弹窗里，避免死锁）", async () => {
    const user = userEvent.setup();
    renderTab();

    // 先冻结。
    await user.click(await actionButtonAsync("冻结写入"));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByText("2 小时"));
    await user.click(within(dialog).getByRole("button", { name: "冻结" }));
    await waitFor(() => expect(actionButton("解冻写入")).toBeTruthy(), { timeout: 5_000 });

    // 解冻入口在**动作条上**（不是弹窗里）：冻结按钮在已冻结时必须禁用，
    // 若解冻也只藏在弹窗内，就会形成「已冻结 → 按钮禁用 → 打不开弹窗 → 永远解不了冻」的死锁。
    // 这条用例同时守住"解冻入口可达"这件事。
    await user.click(actionButton("解冻写入"));
    const confirmBtn = await screen.findByRole("button", { name: "解冻" });
    await user.click(confirmBtn);

    await waitFor(
      () => {
        expect(actionButton("冻结写入")).toBeTruthy();
      },
      { timeout: 5_000 },
    );
    // 回到可写：按钮文案必须跟着翻回来，且不再带冻结态的 title。
    expect(actionButton("冻结写入").getAttribute("title")).toBeNull();
    expect(screen.queryByRole("button", { name: "解冻写入" })).toBeNull();
  }, 20_000);

  // —— FR-40：恢复演练入口与报告 ——
  it("演练入口打开报告弹窗，历史报告列出逐项结论（失败项如实展示）", async () => {
    const user = userEvent.setup();
    renderTab();

    const row = await eventRow("bk-20260910-090000-a1b2c3");
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

    const row = await eventRow("bk-20260910-030000-d4e5f6");
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

  // —— FR-40 追加：演练弹窗对已淘汰的包给出可操作提示（而非裸后端文案）——
  it("演练弹窗遇 404 时给出保留策略提示与刷新按钮", async () => {
    const user = userEvent.setup();
    // 模拟"这一行已陈旧"：该包已被保留策略淘汰，后端返回 404。
    vi.spyOn(endpoints, "listBackupVerifications").mockRejectedValue(
      new ApiError("not_found", "备份包不存在", 404),
    );

    renderTab();

    const row = await eventRow("bk-20260910-090000-a1b2c3");
    await clickRowMenuAction(user, row, "演练");

    // 不再是无从下手的裸文案，而是说清"包可能已被保留策略删除 + 刷新列表"。
    const dialog = await screen.findByRole("dialog");
    expect(await within(dialog).findByText(/可能已被保留策略删除/)).toBeTruthy();
    // 弹窗工具条上本来也有一个「刷新」（重拉报告），因此按 Alert 作用域定位提示里的那个，
    // 避免命中两个同名按钮——两者语义不同：一个刷新报告列表，一个刷新外层备份列表。
    const alert = within(dialog)
      .getByText(/可能已被保留策略删除/)
      .closest(".mantine-Alert-root");
    expect(alert).toBeTruthy();
    expect(within(alert as HTMLElement).getByRole("button", { name: "刷新" })).toBeTruthy();
  }, 20_000);

  // —— 实测反馈的缺陷：演练途中包被保留策略淘汰，弹窗永久卡在「演练进行中」——
  it("演练途中包被淘汰：不得永久显示「演练进行中」，须停止轮询并说明演练已中断", async () => {
    // 复现路径（测试站实测）：间隔 30s + KEEP_LAST=2 ⇒ auto 包存活约 60–90 秒。
    // 用户对某个 auto 包发起演练 → 演练还没跑完，包已被淘汰 → verifications 恒 404。
    // 旧实现只在"新报告出现"时才停轮询，于是永久转圈；而 404 落在 refreshError 上
    // （旧数据在场），只看 state.error 的判据连"包已被淘汰"都不显示。
    const user = userEvent.setup();
    renderTab();

    const row = await eventRow("bk-20260910-030000-d4e5f6");
    await clickRowMenuAction(user, row, "演练");
    const dialog = await screen.findByRole("dialog");

    // 先等预置报告加载完（旧数据在场），这之后才让接口开始 404 —— 正是实测的时序。
    await waitFor(
      () => {
        expect(within(dialog).getAllByText(/通过|未通过/).length).toBeGreaterThan(0);
      },
      { timeout: 10_000 },
    );

    // 受理之后包被淘汰：此后 verifications 一律 404。
    const verifySpy = vi
      .spyOn(endpoints, "listBackupVerifications")
      .mockRejectedValue(new ApiError("not_found", "备份包不存在", 404));
    vi.spyOn(endpoints, "drillBackup").mockResolvedValue({
      packageId: "bk-20260910-030000-d4e5f6",
      level: "deep",
      acceptedAt: new Date().toISOString(),
    });

    await user.click(within(dialog).getByRole("button", { name: "发起演练" }));

    // 核心断言：必须出现"包已被淘汰"的可操作提示，且**不能**留下转圈。
    expect(await within(dialog).findByText(/可能已被保留策略删除/)).toBeTruthy();
    expect(within(dialog).queryByText(/演练进行中/)).toBeNull();
    // 并如实点明"这次演练被中断了"，否则用户会以为是自己点错。
    expect(within(dialog).getByText(/本次演练已被中断/)).toBeTruthy();

    // **包缺失时不得再显示历史报告数**（实机踩到的自相矛盾）：`useAsync` 刻意
    // "刷新失败不丢旧数据"，于是旧报告数会与新错误提示同屏——一边"历史报告 1"、
    // 一边"包已被删除"，用户无从判断哪个是真的。旧数据在包缺失时必然是过时的。
    const historyLabel =
      within(dialog)
        .getByText(/历史报告/)
        .closest("p") ?? dialog;
    expect((historyLabel as HTMLElement).textContent ?? "").not.toMatch(/\d/);

    // **轮询必须真的停下来**（这条才是"永久卡住"的根因所在）。
    // 只看文案不够：文案由 packageMissing 决定，而卡住是"定时器不停"造成的——
    // 二者可以分别正确/错误。旧实现正是"文案对了但定时器还在跑"，
    // 所以必须直接观察请求是否继续发出（间隔 1.5s，等 5s 足以跨过 3 轮）。
    const callsAtStop = verifySpy.mock.calls.length;
    await new Promise((resolve) => setTimeout(resolve, 5_000));
    expect(verifySpy.mock.calls.length).toBe(callsAtStop);
  }, 40_000);

  // —— FR-40 追加六：本机已有包的恢复入口（实测反馈"只能备份无法恢复"）——
  it("事件表提供「用这个包恢复」入口，且菜单里三个检查动作都带用途说明", async () => {
    // 这条守卫的根因：另外三条恢复通道（URL / 上传 / CLI）都要求包来自**外部**，
    // 于是页面上明明列着可用的包，却没有任何按钮能用它恢复——用户会直接得出
    // "只能备份、不能恢复"的结论。缺的是**入口**，不是机制。
    const user = userEvent.setup();
    renderTab();

    const row = await eventRow("bk-20260910-090000-a1b2c3");
    await user.click(within(row).getByRole("button", { name: "更多操作" }));

    expect(await screen.findByRole("menuitem", { name: "用这个包恢复" })).toBeTruthy();

    // 三个检查类动作必须各自带 title（"校验/深度校验/演练"只有几个字，
    // 用户看不出分工与代价，实测反馈"为什么不写提示"）。
    for (const [name, want] of [
      ["校验", /只读归档本身/],
      ["深度校验", /逐 blob 比对内容摘要/],
      ["演练", /真解包、真恢复一次/],
    ] as const) {
      const item = screen.getByRole("menuitem", { name });
      expect(item.getAttribute("title")).toMatch(want);
    }
  }, 20_000);

  it("点「用这个包恢复」弹出确认弹窗，并明确告知会替换本机数据且需重启", async () => {
    const user = userEvent.setup();
    renderTab();

    const row = await eventRow("bk-20260910-090000-a1b2c3");
    await clickRowMenuAction(user, row, "用这个包恢复");

    const dialog = await screen.findByRole("dialog");
    // "会替换本机数据 + 需重启生效"必须写在按钮之前——这是不可逆动作，
    // 用户按下之前就该知道会发生什么（演练只读可以随便点，恢复不行）。
    expect(within(dialog).getByText(/替换本机现有数据/)).toBeTruthy();
    expect(within(dialog).getByText(/需重启服务后才真正替换/)).toBeTruthy();
    // 覆盖开关必须前置（本机非空时没有它会直接失败，那时用户已等了一轮校验）。
    expect(within(dialog).getByTestId("restore-overwrite")).toBeTruthy();
    expect(within(dialog).getByTestId("restore-start")).toBeTruthy();
  }, 20_000);

  it("确认恢复后提交 overwrite 并刷新列表；不勾选时以 false 提交", async () => {
    const user = userEvent.setup();
    // 记录提交参数：这条守卫确保勾选框真的透传到请求，而不是只改了本地状态。
    const restoreSpy = vi.spyOn(endpoints, "restoreFromLocalPackage").mockResolvedValue({
      importId: "imp-local-1",
      origin: "local",
      status: "queued",
      operator: "admin",
      overwrite: true,
      deep: false,
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    });
    renderTab();

    const row = await eventRow("bk-20260910-090000-a1b2c3");
    await clickRowMenuAction(user, row, "用这个包恢复");
    const dialog = await screen.findByRole("dialog");

    await user.click(within(dialog).getByTestId("restore-overwrite"));
    await user.click(within(dialog).getByTestId("restore-start"));

    await waitFor(() => {
      expect(restoreSpy).toHaveBeenCalledWith("bk-20260910-090000-a1b2c3", { overwrite: true });
    });
  }, 20_000);

  it("演练长时间不出报告：到达超时上限后停止轮询并如实说明（不能无限转圈）", async () => {
    // 与上一条互补：上一条走"包被淘汰（404）"这条终止路径，这条走"接口一直成功返回、
    // 但新报告永远不出现"——例如后端崩在演练中途、或任务被吞掉。
    // 若只有 404 一个终止条件，这种情况就会永久转圈，所以必须有时间兜底。
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      renderTab();

      const row = await eventRow("bk-20260910-030000-d4e5f6");
      await clickRowMenuAction(user, row, "演练");
      const dialog = await screen.findByRole("dialog");
      await waitFor(
        () => {
          expect(within(dialog).getAllByText(/通过|未通过/).length).toBeGreaterThan(0);
        },
        { timeout: 10_000 },
      );

      // 让接口**持续成功**返回，但内容里始终没有本次演练的新报告。
      const stale = await endpoints.listBackupVerifications("bk-20260910-030000-d4e5f6", {
        page: 1,
        page_size: 20,
      });
      vi.spyOn(endpoints, "listBackupVerifications").mockResolvedValue(stale);
      vi.spyOn(endpoints, "drillBackup").mockResolvedValue({
        packageId: "bk-20260910-030000-d4e5f6",
        level: "deep",
        // 受理时刻设在未来：既有报告都早于它，故 settled 不会成立 —— 模拟"报告永不到达"。
        acceptedAt: new Date(Date.now() + 3_600_000).toISOString(),
      });

      await user.click(within(dialog).getByRole("button", { name: "发起演练" }));
      await waitFor(() => {
        expect(within(dialog).getByText(/演练进行中/)).toBeTruthy();
      });

      // 推过超时上限：转圈必须停下，并给出可照做的说明。
      await vi.advanceTimersByTimeAsync(DRILL_POLL_TIMEOUT_MS + 2_000);

      await waitFor(() => {
        expect(within(dialog).queryByText(/演练进行中/)).toBeNull();
      });
      expect(within(dialog).getByText(/未在预期时间内收到报告/)).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
  }, 40_000);
});
