// 仪表盘集成测试：鉴权后拉取实例状态并渲染概览卡片。
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { DashboardPage } from "../src/pages/DashboardPage";
import { renderWithProviders } from "./harness";

describe("仪表盘", () => {
  it("渲染实例版本与用户数", async () => {
    renderWithProviders(<DashboardPage />, { route: "/dashboard", authenticated: true });
    expect(await screen.findByText("0.2.0-mock")).toBeTruthy();
    // 种子数据含 2 个用户
    expect(await screen.findByText("2")).toBeTruthy();
  });
});
