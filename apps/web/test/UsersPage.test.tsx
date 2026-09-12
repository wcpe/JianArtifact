// 用户管理集成测试：渲染种子用户列表，并走完新建用户表单落库回显。
import { server } from "@jianartifact/devmock/node";
import { http, HttpResponse } from "msw";
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

  it("可禁止 Web 登录并编辑发布账号策略", async () => {
    const user = userEvent.setup();
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    await screen.findByText("admin");

    await user.click(screen.getByRole("switch", { name: "允许 Web 登录 admin" }));
    await waitFor(() =>
      expect(
        (screen.getByRole("switch", { name: "允许 Web 登录 admin" }) as HTMLInputElement).checked,
      ).toBe(false),
    );

    await user.click(screen.getAllByRole("button", { name: "发布策略" })[0]);
    const dialog = await screen.findByRole("dialog");
    expect(await within(dialog).findByText("允许的路径前缀")).toBeTruthy();
    expect(within(dialog).getByText("Hosted 仓库")).toBeTruthy();
  });

  it("可编辑多个前缀并在保存后回显完整策略", async () => {
    const user = userEvent.setup();
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    await screen.findByText("admin");

    await user.click(screen.getAllByRole("button", { name: "发布策略" })[0]);
    const dialog = await screen.findByRole("dialog");
    const prefixes = within(dialog).getByLabelText("允许的路径前缀");
    await user.clear(prefixes);
    await user.type(prefixes, "releases\nsnapshots");
    await user.clear(within(dialog).getByLabelText("每小时制品数上限"));
    await user.type(within(dialog).getByLabelText("每小时制品数上限"), "7");
    await user.clear(within(dialog).getByLabelText("每日字节数上限"));
    await user.type(within(dialog).getByLabelText("每日字节数上限"), "1024");
    await user.clear(within(dialog).getByLabelText("单文件字节数上限"));
    await user.type(within(dialog).getByLabelText("单文件字节数上限"), "512");
    await user.click(within(dialog).getByRole("switch", { name: /禁止 Web 登录/ }));
    await user.click(within(dialog).getByRole("switch", { name: /禁止覆盖 Release/ }));
    await user.click(within(dialog).getByRole("button", { name: "保存" }));

    await waitFor(() => {
      expect((prefixes as HTMLTextAreaElement).value).toBe("releases\nsnapshots");
      expect((within(dialog).getByLabelText("每小时制品数上限") as HTMLInputElement).value).toBe(
        "7",
      );
      expect((within(dialog).getByLabelText("每日字节数上限") as HTMLInputElement).value).toBe(
        "1024",
      );
      expect((within(dialog).getByLabelText("单文件字节数上限") as HTMLInputElement).value).toBe(
        "512",
      );
      expect(
        (within(dialog).getByRole("switch", { name: /禁止 Web 登录/ }) as HTMLInputElement).checked,
      ).toBe(true);
      expect(
        (within(dialog).getByRole("switch", { name: /禁止覆盖 Release/ }) as HTMLInputElement)
          .checked,
      ).toBe(true);
    });
  });

  it("保存发布策略失败时保留编辑内容并展示错误", async () => {
    server.use(
      http.put("*/api/v1/users/:id/publish-policies/:repo", () =>
        HttpResponse.json(
          { error: { code: "validation_error", message: "保存发布策略被拒绝" } },
          { status: 400 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    await screen.findByText("admin");

    await user.click(screen.getAllByRole("button", { name: "发布策略" })[0]);
    const dialog = await screen.findByRole("dialog");
    const prefixes = within(dialog).getByLabelText("允许的路径前缀");
    await user.clear(prefixes);
    await user.type(prefixes, "restricted");
    await user.click(within(dialog).getByRole("button", { name: "保存" }));

    expect(await screen.findByText("保存发布策略被拒绝")).toBeTruthy();
    expect((prefixes as HTMLTextAreaElement).value).toBe("restricted");
    expect(within(dialog).getByRole("button", { name: "保存" })).toBeTruthy();
  });
});
