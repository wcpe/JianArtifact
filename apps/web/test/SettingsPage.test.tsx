// 设置页回归测试：仅保留可保存的服务设置，不展示部署与集群内部信息。
import { screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ getSettings: vi.fn(), putSettings: vi.fn() }));

vi.mock("../src/api/endpoints", () => api);

import { SettingsPage } from "../src/pages/SettingsPage";
import { renderWithProviders } from "./harness";

describe("设置页", () => {
  beforeEach(() => {
    api.getSettings.mockResolvedValue({
      anonymousAccess: true,
      publicUrl: "https://repo.example.com",
      upstreamTimeout: 30,
      syncInterval: 5,
    });
    api.putSettings.mockResolvedValue({});
  });

  it("只展示可保存服务设置，不暴露集群或部署字段", async () => {
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });

    expect(await screen.findByRole("heading", { name: "服务设置" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "保存设置" })).toBeTruthy();
    expect(screen.queryByText("集群")).toBeNull();
    expect(screen.queryByText(/JIAN_/)).toBeNull();
  });
});
