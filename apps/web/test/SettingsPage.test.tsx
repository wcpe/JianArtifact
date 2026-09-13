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
  syncInterval: 5,
  allowedHosts: [] as string[],
  originTokenEnabled: false,
  originTokenHeader: "",
  originTokenValue: "",
};

describe("normalizeHostInput", () => {
  it("剥离协议 / 路径 / 端口并统一小写", () => {
    expect(normalizeHostInput("https://repo.wcpe.top")).toBe("repo.wcpe.top");
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
    expect(screen.getByText("安全防护")).toBeTruthy();
    expect(screen.getByText("允许访问的域名")).toBeTruthy();
    expect(screen.getByText("未限制")).toBeTruthy();
    expect(screen.getByRole("button", { name: "保存设置" })).toBeTruthy();
    expect(screen.queryByText("集群")).toBeNull();
    expect(screen.queryByText(/JIAN_/)).toBeNull();
  });

  it("粘贴完整 URL 的域名归一化后入列并提交", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    const input = await screen.findByPlaceholderText("repo.example.com");
    await user.type(input, "https://repo.wcpe.top/path");
    await user.keyboard("{Enter}");

    // 列表里显示的是归一化后的主机名，且计数随之更新。
    expect(await screen.findByText("repo.wcpe.top")).toBeTruthy();
    expect(screen.getByText("共 1 条")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "保存设置" }));
    await waitFor(() => expect(api.putSettings).toHaveBeenCalledTimes(1));
    expect(api.putSettings).toHaveBeenCalledWith(
      expect.objectContaining({ allowedHosts: ["repo.wcpe.top"] }),
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
});
