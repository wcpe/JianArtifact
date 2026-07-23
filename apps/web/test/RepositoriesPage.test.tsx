// 仓库管理集成测试：渲染种子仓库；未鉴权时列表进入错误态。
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { RepositoriesPage } from "../src/pages/RepositoriesPage";
import { renderWithProviders } from "./harness";

describe("仓库管理", () => {
  it("渲染种子仓库", async () => {
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    expect(await screen.findByText("maven-releases")).toBeTruthy();
    expect(await screen.findByText("npm-proxy")).toBeTruthy();
    expect(await screen.findByText("raw-hosted")).toBeTruthy();
  });

  it("未鉴权进入错误态", async () => {
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: false });
    expect(await screen.findByTestId("state-error")).toBeTruthy();
  });

  it("选择 proxy 类型后填上游地址并新建", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    await screen.findByText("raw-hosted");

    await user.click(screen.getByRole("button", { name: "新建仓库" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText(/名称/), "my-proxy");

    // 选择类型为 proxy，触发上游地址输入渲染。
    await user.click(within(dialog).getByLabelText(/类型/));
    await user.click(await screen.findByRole("option", { name: "proxy" }));
    await user.type(within(dialog).getByLabelText(/上游地址/), "https://repo.example.com/raw");

    await user.click(within(dialog).getByRole("button", { name: "新建" }));
    await waitFor(() => expect(screen.getByText("my-proxy")).toBeTruthy());
  });
});
