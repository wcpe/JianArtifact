// 设置页回归测试：集群令牌不回显，留空保存及仅切换启用状态都必须保留已有令牌。
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  getSettings: vi.fn(),
  putSettings: vi.fn(),
  getClusterStatus: vi.fn(),
  setClusterConfig: vi.fn(),
}));

vi.mock("../src/api/endpoints", () => api);

import { SettingsPage } from "../src/pages/SettingsPage";
import { renderWithProviders } from "./harness";

const clusterStatus = {
  nodeId: "node-a",
  peerUrl: "https://peer.example.com",
  peers: [{ url: "https://peer.example.com" }],
  tokenSet: true,
  enabled: true,
  watermark: 0,
  hasWatermark: false,
};

describe("设置页集群令牌保持", () => {
  beforeEach(() => {
    api.getSettings.mockResolvedValue({
      anonymousAccess: true,
      publicUrl: "https://repo.example.com",
      upstreamTimeout: 30,
      syncInterval: 5,
    });
    api.getClusterStatus.mockResolvedValue(clusterStatus);
    api.putSettings.mockResolvedValue({});
    api.setClusterConfig.mockResolvedValue(clusterStatus);
    api.setClusterConfig.mockClear();
  });

  it("令牌留空保存时省略 token 字段", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings#cluster", authenticated: true });

    expect(await screen.findByDisplayValue("https://peer.example.com")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "保存配置" }));

    await waitFor(() =>
      expect(api.setClusterConfig).toHaveBeenCalledWith({
        peers: [{ url: "https://peer.example.com", token: undefined }],
        enabled: true,
      }),
    );
  });

  it("仅关闭自动同步时仍提交原对端且不发送空 token", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings#cluster", authenticated: true });

    const enabled = await screen.findByRole("switch", { name: "自动同步" });
    await user.click(enabled);
    await user.click(screen.getByRole("button", { name: "保存配置" }));

    await waitFor(() =>
      expect(api.setClusterConfig).toHaveBeenCalledWith({
        peers: [{ url: "https://peer.example.com", token: undefined }],
        enabled: false,
      }),
    );
  });
});
