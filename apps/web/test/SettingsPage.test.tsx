// 设置页回归测试：
// - 仅保留可保存的服务设置，不展示部署与集群内部信息；
// - 域名白名单粘贴完整 URL 必须归一化为主机名（历史 bug：误报「含非法字符」）；
// - 表单改动后出现「有未保存的改动」，保存成功后回到「已保存」。
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  getSettings: vi.fn(),
  putSettings: vi.fn(),
  getSettingHistory: vi.fn(),
}));

vi.mock("../src/api/endpoints", () => api);

import { clearAsyncCache } from "../src/hooks/useAsync";
import { normalizeHostInput, SettingsPage } from "../src/pages/SettingsPage";
import { renderWithProviders } from "./harness";

const baseSettings = {
  anonymousAccess: true,
  publicUrl: "https://repo.example.com",
  upstreamTimeout: 30,
  allowedHosts: [] as string[],
  originTokenEnabled: false,
  originTokenHeader: "",
  originTokenValue: "",
};

describe("normalizeHostInput", () => {
  it("剥离协议 / 路径 / 端口并统一小写", () => {
    expect(normalizeHostInput("https://mirror.example.net")).toBe("mirror.example.net");
    expect(normalizeHostInput("http://Repo.Example.COM:8080/path?x=1")).toBe("repo.example.com");
    expect(normalizeHostInput("repo.example.com:8443")).toBe("repo.example.com");
    expect(normalizeHostInput("10.0.0.3")).toBe("10.0.0.3");
    expect(normalizeHostInput("https://")).toBe("");
  });

  it("IPv6 字面量不被误当端口截断", () => {
    expect(normalizeHostInput("::1")).toBe("::1");
    expect(normalizeHostInput("[::1]:8080")).toBe("::1");
  });
});

describe("设置页", () => {
  beforeEach(() => {
    clearAsyncCache();
    api.getSettings.mockReset().mockResolvedValue({ ...baseSettings });
    // 历史默认给一条记录：既能验证面板展开后真的渲染内容，也避免用例各自铺桩。
    api.getSettingHistory.mockReset().mockResolvedValue({
      key: "blob_gc_interval",
      entries: [
        { ts: "2026-10-31T07:43:42Z", actor: "admin", oldValue: "86400", newValue: "43200" },
      ],
    });
    api.putSettings
      .mockReset()
      .mockImplementation((patch: Record<string, unknown>) =>
        Promise.resolve({ ...baseSettings, ...patch }),
      );
  });

  it("只展示可保存服务设置，不暴露集群或部署字段", async () => {
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    expect(await screen.findByText("服务设置")).toBeTruthy();
    expect(screen.getAllByTestId("settings-page")).toHaveLength(1);
    expect(screen.getByText("安全防护")).toBeTruthy();
    expect(screen.getByText("允许访问的域名")).toBeTruthy();
    // 「未限制」现在同时出现在白名单分区与顶部概览带的「域名白名单」取值上，故用 getAllByText。
    expect(screen.getAllByText("未限制").length).toBeGreaterThan(0);
    expect(screen.getByRole("button", { name: "保存设置" })).toBeTruthy();
    expect(screen.queryByText("集群")).toBeNull();
    expect(screen.queryByText(/JIAN_/)).toBeNull();
  });

  it("粘贴完整 URL 的域名归一化后入列并提交", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    const input = await screen.findByPlaceholderText("repo.example.com");
    await user.type(input, "https://mirror.example.net/path");
    await user.keyboard("{Enter}");

    // 列表里显示的是归一化后的主机名，且计数随之更新。
    expect(await screen.findByText("mirror.example.net")).toBeTruthy();
    expect(screen.getByText("共 1 条")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "保存设置" }));
    await waitFor(() => expect(api.putSettings).toHaveBeenCalledTimes(1));
    expect(api.putSettings).toHaveBeenCalledWith(
      expect.objectContaining({ allowedHosts: ["mirror.example.net"] }),
    );
  });

  it("改动出现未保存提示，保存成功后消除", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    await screen.findByPlaceholderText("repo.example.com");
    expect(await screen.findByText("已保存")).toBeTruthy();

    // 用开关制造改动（不必依赖某个字段的具体取值）。
    await user.click(screen.getByRole("switch", { name: "回源 Token 校验" }));
    expect(await screen.findByText("有未保存的改动")).toBeTruthy();

    // 打开回源校验后头名与值必填——这是后端的既有约束，前端现在提前拦住。
    // 因此这里先补齐，才能走到「保存成功」。
    await user.type(screen.getByLabelText("请求头名"), "X-Jian-Origin-Token");
    await user.type(screen.getByLabelText("Token 值"), "token-value-for-test");

    await user.click(screen.getByRole("button", { name: "保存设置" }));
    await waitFor(() => expect(api.putSettings).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("已保存")).toBeTruthy();
  });

  // 13 项运行期策略的生效时机**不统一**：作业间隔要等下一轮、请求预算下一次请求就生效。
  // 一律写「立即生效」会让运维改完立刻验证，然后得出「配置没生效」的结论，
  // 故按类计数断言——任何一项被错标成别的时机都会让计数对不上。
  it("运行期策略进入即铺开，13 项按四组呈现且逐项标注生效时机", async () => {
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    expect(await screen.findByTestId("settings-runtime-policy")).toBeTruthy();
    // 不再有折叠开关：进入页面就应渲染全部输入框，不需要先点「展开编辑」。
    expect(screen.queryByRole("button", { name: "展开编辑" })).toBeNull();
    expect(screen.getByLabelText("孤儿 blob 回收间隔（秒）")).toBeTruthy();
    expect(screen.getByLabelText("回收站容量上限（字节）")).toBeTruthy();
    // 5 个作业间隔 / 3 个「下一次作业运行」的保留策略 / 2 个清理类 / 3 个请求预算。
    // 注意「保留策略」组内本是 4 项，其中回收站保留天数在下一次清理时才生效，
    // 因此这里 3 + 2 而不是 4 + 1。
    expect(screen.getAllByText("从下一轮起生效（不重排当前等待）")).toHaveLength(5);
    expect(screen.getAllByText("下一次作业运行时生效")).toHaveLength(3);
    expect(screen.getAllByText("下一次清理时生效")).toHaveLength(2);
    expect(screen.getAllByText("下一次请求生效")).toHaveLength(3);
  });

  // 展开后卡片被外层 flex 压缩会把内容裁掉：卡片自身 overflow: hidden，
  // 被压到一行多高时其下 12 个输入框既不可见也点不到，
  // 仅靠「13 项都渲染出来了」的断言发现不了（jsdom 不算布局，只能用样式断言兜住）。
  it("运行期策略卡片不参与外层 flex 收缩，内容不被裁掉", async () => {
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    const card = await screen.findByTestId("settings-runtime-policy");
    expect(card.style.flexShrink).toBe("0");
  });

  // 逐项独立保存：只提交被改的那一项，其余键不出现在请求体里
  // （后端是部分更新，漏传即保持原值；误传全部会把别的字段的未保存改动一起提交）。
  it("运行期策略每一项都能单独保存，请求体只含该项", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    await screen.findByTestId("settings-runtime-policy");
    api.putSettings.mockClear();

    // 该字段默认 86400 秒，界面按「最自然的单位」显示为 1 天。
    // 改成 2 天 —— 提交值必须是 172800 秒（单位只是输入方式，落库仍是秒）。
    const input = screen.getByLabelText("孤儿 blob 回收间隔（秒）");
    expect((input as HTMLInputElement).value).toBe("1");
    await user.clear(input);
    await user.type(input, "2");
    await user.click(screen.getByRole("button", { name: "保存「孤儿 blob 回收间隔（秒）」" }));

    await waitFor(() => expect(api.putSettings).toHaveBeenCalledTimes(1));
    expect(api.putSettings).toHaveBeenCalledWith({ blobGcInterval: 2 * 86400 });
  });

  // 输入完成即校验：越界要当场给出可填区间，而不是等点保存才弹全局错误。
  it("运行期策略越界时该项立即报错并禁用其保存按钮", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    await screen.findByTestId("settings-runtime-policy");
    api.putSettings.mockClear();

    const label = "搜索总预算（秒）";
    const input = screen.getByLabelText(label);
    await user.clear(input);
    await user.type(input, "99999");
    await user.tab(); // 失焦视为输入完成

    // 越界文案必须当场出现（而不是等点保存才弹全局错误）。
    expect(await screen.findByText("搜索总预算（秒）超出上限 3600")).toBeTruthy();
    // 不可保存：按钮必须处于禁用态（本项目未引入 jest-dom，用原生属性断言）。
    expect(
      (screen.getByRole("button", { name: `保存「${label}」` }) as HTMLButtonElement).disabled,
    ).toBe(true);
    expect(api.putSettings).not.toHaveBeenCalled();
  });

  // 单位换算：把「1 天」改成「2 天」应提交 172800 秒，
  // 而不是把用户输入的 2 直接当成秒（那会把作业周期从一天改成一瞬）。
  it("时长字段按所选单位换算后提交，界面不出现 86400 这种原始秒数", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    await screen.findByTestId("settings-runtime-policy");
    api.putSettings.mockClear();

    const label = "存储治理清理间隔（秒）";
    const input = screen.getByLabelText(label);
    // 86400 秒自动显示为 1（天），用户不必自己心算。
    expect((input as HTMLInputElement).value).toBe("1");
    await user.clear(input);
    await user.type(input, "2");
    await user.click(screen.getByRole("button", { name: `保存「${label}」` }));

    await waitFor(() => expect(api.putSettings).toHaveBeenCalledTimes(1));
    expect(api.putSettings).toHaveBeenCalledWith({ storageCleanupInterval: 172800 });
  });

  // 历史面板：展开要真的展开并展示前后值。
  // 这条钉住一个曾经踩过的坑——写入状态用 settingKey、读取却用字段名，
  // 两边键不一致时按钮点了没反应，而"按钮存在"的断言完全发现不了。
  // 历史必须用**模态框**展示，不能在页面上就地展开。
  // 就地展开会把同一行的相邻字段挤到下一行、整页跳动——用户明确要求过不许改布局。
  it("点开历史会弹出模态框并展示前后值，且不在页面上留内联面板", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    await screen.findByTestId("settings-runtime-policy");
    const btn = screen.getByRole("button", { name: "查看「孤儿 blob 回收间隔（秒）」的历史" });

    await user.click(btn);

    // 弹出的是 dialog，不是就地插入的卡片。
    expect(await screen.findByRole("dialog")).toBeTruthy();
    // 历史里的原始秒数按量纲可读化（86400 秒 → 1 天，43200 秒 → 12 小时）。
    expect(await screen.findByText(/1 天 → 12 小时/)).toBeTruthy();
  });

  // 逐项保存只回填这一项，不能把其它未保存的改动算成「已保存」。
  // 曾经用整体签名重算，导致「改 A → 改 B → 只保存 A」后顶部立刻显示已保存，
  // 用户据此离开页面，B 的改动就静默丢了。
  it("保存一项后其它未保存的改动仍然标记为未保存", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    await screen.findByTestId("settings-runtime-policy");
    api.putSettings.mockClear();

    // 改 A（孤儿 blob 回收间隔）：1 天 → 2 天。
    const aLabel = "孤儿 blob 回收间隔（秒）";
    const aInput = screen.getByLabelText(aLabel);
    await user.clear(aInput);
    await user.type(aInput, "2");

    // 改 B（存储治理清理间隔）：1 天 → 3 天，但不保存。
    const bLabel = "存储治理清理间隔（秒）";
    const bInput = screen.getByLabelText(bLabel);
    await user.clear(bInput);
    await user.type(bInput, "3");

    // 只保存 A。
    await user.click(screen.getByRole("button", { name: `保存「${aLabel}」` }));
    await waitFor(() => expect(api.putSettings).toHaveBeenCalledTimes(1));
    expect(api.putSettings).toHaveBeenCalledWith({ blobGcInterval: 2 * 86400 });

    // B 仍未保存：它必须仍处于「可保存」状态（未改动时该按钮是禁用的）。
    expect(
      (screen.getByRole("button", { name: `保存「${bLabel}」` }) as HTMLButtonElement).disabled,
    ).toBe(false);
  });

  // 清空输入框不能被静默当成 0：0 在这些项上是「禁用作业 / 不限制」，
  // 与「还没填」语义相反，落库后果完全不同。
  it("清空输入框时该项报错且不可保存，不会静默变成 0", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    await screen.findByTestId("settings-runtime-policy");
    api.putSettings.mockClear();

    const label = "孤儿 blob 回收间隔（秒）";
    const input = screen.getByLabelText(label);
    await user.clear(input);
    await user.tab(); // 失焦视为输入完成

    expect(await screen.findByText("孤儿 blob 回收间隔（秒）不能为空")).toBeTruthy();
    expect(
      (screen.getByRole("button", { name: `保存「${label}」` }) as HTMLButtonElement).disabled,
    ).toBe(true);
    expect(api.putSettings).not.toHaveBeenCalled();
  });

  // 保存后不能整页刷新：靠父组件 reload 才能更新的话，AsyncBoundary 的
  // LoadingOverlay 会盖住整页，改一个字段就闪一下。
  // 这里断言「保存成功后不再重新拉取设置」——重拉正是那次整页刷新的来源。
  it("保存后不重新拉取设置，避免整页 loading 遮罩", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    await screen.findByTestId("settings-runtime-policy");
    api.getSettings.mockClear();

    const label = "回源超时（秒）";
    const input = screen.getByLabelText(label);
    await user.clear(input);
    await user.type(input, "45");
    await user.click(screen.getByRole("button", { name: "保存设置" }));

    await waitFor(() => expect(api.putSettings).toHaveBeenCalledTimes(1));
    // 关键断言：保存成功后没有再次 getSettings（即没有触发全局 reload）。
    expect(api.getSettings).not.toHaveBeenCalled();
  });

  // 文本字段与数值字段共用同一套三态反馈：红框 + 原因、绿框 + 可用。
  // 对外 URL 只接受 http/https 绝对地址（后端 validatePublicURL 同口径），
  // 前端提前拦住可以避免「填完点保存才被拒」。
  it("对外 URL 格式非法时红框报错且禁用保存", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    const input = await screen.findByLabelText("对外基础 URL");
    await user.clear(input);
    await user.type(input, "ftp://not-allowed");

    expect(await screen.findByText("对外基础 URL必须是 http/https 开头的完整地址")).toBeTruthy();
    expect((screen.getByRole("button", { name: "保存设置" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
    expect(api.putSettings).not.toHaveBeenCalled();
  });

  // 开启回源校验却不填头名与值，后端必然拒绝；前端现在提前禁用保存。
  it("开启回源校验后头名与值必填，缺失时禁止保存", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    await user.click(await screen.findByRole("switch", { name: "回源 Token 校验" }));

    expect(await screen.findByText("请求头名不能为空")).toBeTruthy();
    expect(screen.getByText("Token 值不能为空")).toBeTruthy();
    expect((screen.getByRole("button", { name: "保存设置" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
  });

  it("13 项运行期策略整批进入提交体，且 0 不被当成空值", async () => {
    // 0 是「禁用作业 / 不限制」的合法取值：归一化若用 `|| 86400`，这里会拿到默认值。
    api.getSettings.mockResolvedValue({ ...baseSettings, blobGcInterval: 0, trashMaxBytes: 0 });
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    await screen.findByTestId("settings-runtime-policy");
    await user.click(screen.getByRole("button", { name: "保存设置" }));
    await waitFor(() => expect(api.putSettings).toHaveBeenCalledTimes(1));

    expect(api.putSettings).toHaveBeenCalledWith(
      expect.objectContaining({
        blobGcInterval: 0,
        trashMaxBytes: 0,
        storageCleanupInterval: 86400,
        backupOrphanCleanupInterval: 86400,
        trashGcInterval: 86400,
        upstreamProbeInterval: 60,
        backupOrphanGraceHours: 1,
        storageMetadataRetentionDays: 7,
        storageTempMaxAgeHours: 24,
        trashRetentionDays: 7,
        autoBlockBaseSeconds: 40,
        searchTimeoutSeconds: 10,
        capacityCacheTtlSeconds: 30,
      }),
    );
  });
});
