// 设置页回归测试：
// - 仅保留可保存的服务设置，不展示部署与集群内部信息；
// - 域名白名单粘贴完整 URL 必须归一化为主机名（历史 bug：误报「含非法字符」）；
// - 表单改动后出现「有未保存的改动」，保存成功后回到「已保存」。
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ getSettings: vi.fn(), putSettings: vi.fn() }));

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

    await user.click(screen.getByRole("switch", { name: "回源 Token 校验" }));
    expect(await screen.findByText("有未保存的改动")).toBeTruthy();

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
    expect(screen.getAllByText("保存后从下一轮起生效")).toHaveLength(5);
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

    const input = screen.getByLabelText("孤儿 blob 回收间隔（秒）");
    await user.clear(input);
    await user.type(input, "5");
    await user.click(screen.getByRole("button", { name: "保存「孤儿 blob 回收间隔（秒）」" }));

    await waitFor(() => expect(api.putSettings).toHaveBeenCalledTimes(1));
    expect(api.putSettings).toHaveBeenCalledWith({ blobGcInterval: 5 });
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
    // 同时把可填区间写出来，用户不必自己猜能填多少。
    // 3600 上界被多个请求类字段共用，因此这里断言「至少有一处」而非唯一。
    expect(screen.getAllByText("允许范围：1 – 3600").length).toBeGreaterThan(0);
    // 不可保存：按钮必须处于禁用态（本项目未引入 jest-dom，用原生属性断言）。
    expect(
      (screen.getByRole("button", { name: `保存「${label}」` }) as HTMLButtonElement).disabled,
    ).toBe(true);
    expect(api.putSettings).not.toHaveBeenCalled();
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
