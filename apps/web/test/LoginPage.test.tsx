// 登录页集成测试：验证已初始化实例展示登录标题，以及登录成功后写入会话令牌。
// （空库实例的初始化引导及自举流程见 SetupPage.test.tsx。）
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { getToken } from "../src/api/client";
import { LoginPage } from "../src/pages/LoginPage";
import { renderWithProviders } from "./harness";

describe("登录页", () => {
  it("已初始化实例展示登录标题", async () => {
    renderWithProviders(<LoginPage />, { route: "/login" });
    expect(await screen.findByText("登录管理端")).toBeTruthy();
  });

  it("登录成功写入会话令牌", async () => {
    const user = userEvent.setup();
    renderWithProviders(<LoginPage />, { route: "/login" });
    await screen.findByText("登录管理端");

    await user.type(screen.getByLabelText(/用户名/), "admin");
    await user.type(screen.getByLabelText(/口令/), "password1");
    await user.click(screen.getByRole("button", { name: "登录" }));

    await waitFor(() => expect(getToken()).toBe("mock.jwt.token"));
  });
});
