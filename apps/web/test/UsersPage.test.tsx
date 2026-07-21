// 用户管理集成测试：渲染种子用户列表，并走完新建用户表单落库回显。
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { UsersPage } from "../src/pages/UsersPage";
import { renderWithProviders } from "./harness";

describe("用户管理", () => {
  it("渲染种子用户", async () => {
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    expect(await screen.findByText("admin")).toBeTruthy();
    expect(await screen.findByText("developer")).toBeTruthy();
  });

  it("新建用户后列表出现该用户", async () => {
    const user = userEvent.setup();
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    await screen.findByText("admin");

    await user.click(screen.getByRole("button", { name: "新建用户" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText(/用户名/), "tester");
    await user.type(within(dialog).getByLabelText(/口令/), "password1");
    await user.click(within(dialog).getByRole("button", { name: "新建" }));

    await waitFor(() => expect(screen.getByText("tester")).toBeTruthy());
  });
});
