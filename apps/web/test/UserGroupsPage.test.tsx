// 用户组管理（FR-36）测试：列表渲染、新建 / 改名 / 删除、成员增删。
// 走 devmock 的 MSW server，断言真实请求体而不是只断言界面文案。
import { server } from "@jianartifact/devmock/node";
import { http, HttpResponse } from "msw";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { UserGroupsPage } from "../src/pages/UserGroupsPage";
import { renderWithProviders } from "./harness";

/** 打开种子组的成员弹窗。 */
async function openMembersDialog(user: ReturnType<typeof userEvent.setup>) {
  await screen.findByText("release-team");
  await user.click(screen.getAllByRole("button", { name: "成员" })[0]!);
  return await screen.findByRole("dialog");
}

describe("用户组管理", () => {
  it("渲染种子组与其成员数", async () => {
    renderWithProviders(<UserGroupsPage />, { route: "/user-groups", authenticated: true });

    expect(await screen.findByText("release-team")).toBeTruthy();
    // 种子组有一个成员（developer），概览带的「成员关系数」应为 1。
    // 三个 KPI 取值都是数字文本，故锁定「成员关系数」所在的 KPI 单元（标签 + 数值的容器）再断言。
    const label = await screen.findByText("成员关系数");
    expect(label.parentElement?.parentElement?.textContent).toBe("成员关系数1");
  });

  it("新建组后列表出现该组", async () => {
    const user = userEvent.setup();
    renderWithProviders(<UserGroupsPage />, { route: "/user-groups", authenticated: true });
    await screen.findByText("release-team");

    await user.click(screen.getByRole("button", { name: "新建用户组" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText(/组名/), "ops-team");
    await user.type(within(dialog).getByLabelText(/说明/), "运维值班组");
    await user.click(within(dialog).getByRole("button", { name: "新建" }));

    await waitFor(() => expect(screen.getByText("ops-team")).toBeTruthy());
  });

  it("新建组把组名与说明原样提交", async () => {
    let body: { name?: string; description?: string } | null = null;
    server.use(
      http.post("*/api/v1/user-groups", async ({ request }) => {
        body = (await request.json()) as { name?: string; description?: string };
        return HttpResponse.json(
          {
            id: 9,
            name: body.name,
            description: body.description,
            createdAt: "2026-01-01T00:00:00Z",
          },
          { status: 201 },
        );
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<UserGroupsPage />, { route: "/user-groups", authenticated: true });
    await screen.findByText("release-team");

    await user.click(screen.getByRole("button", { name: "新建用户组" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText(/组名/), "qa-team");
    await user.type(within(dialog).getByLabelText(/说明/), "质量组");
    await user.click(within(dialog).getByRole("button", { name: "新建" }));

    await waitFor(() => expect(body).toEqual({ name: "qa-team", description: "质量组" }));
  });

  it("编辑弹窗预填当前组名并可改名", async () => {
    let body: { name?: string; description?: string } | null = null;
    server.use(
      http.patch("*/api/v1/user-groups/:id", async ({ request }) => {
        body = (await request.json()) as { name?: string; description?: string };
        return HttpResponse.json({
          id: 1,
          name: body.name ?? "release-team",
          description: body.description ?? "负责发布窗口的团队",
          createdAt: "2026-01-01T00:00:00Z",
        });
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<UserGroupsPage />, { route: "/user-groups", authenticated: true });
    await screen.findByText("release-team");

    await user.click(screen.getAllByRole("button", { name: "编辑" })[0]!);
    const dialog = await screen.findByRole("dialog");
    // 预填：用户改一个字即可，不必重新输入整条组名。
    expect((within(dialog).getByLabelText(/组名/) as HTMLInputElement).value).toBe("release-team");

    await user.clear(within(dialog).getByLabelText(/组名/));
    await user.type(within(dialog).getByLabelText(/组名/), "release-core");
    await user.click(within(dialog).getByRole("button", { name: "保存" }));

    await waitFor(() =>
      expect(body).toEqual({ name: "release-core", description: "负责发布窗口的团队" }),
    );
  });

  it("删除组走二次确认后调用删除端点", async () => {
    let deleted = false;
    server.use(
      http.delete("*/api/v1/user-groups/:id", () => {
        deleted = true;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<UserGroupsPage />, { route: "/user-groups", authenticated: true });
    await screen.findByText("release-team");

    await user.click(screen.getAllByRole("button", { name: "删除" })[0]!);
    const dialog = await screen.findByRole("dialog");
    // 确认文案要点明「该组的授权会一并失效」，不能只写一个泛化的「确定删除？」。
    expect(within(dialog).getByText(/访问控制中的授权会一并失效/)).toBeTruthy();
    await user.click(within(dialog).getByRole("button", { name: "删除" }));

    await waitFor(() => expect(deleted).toBe(true));
  });

  it("成员弹窗列出种子成员并可移出", async () => {
    let removed = false;
    server.use(
      http.delete("*/api/v1/user-groups/:id/members/:userId", () => {
        removed = true;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<UserGroupsPage />, { route: "/user-groups", authenticated: true });
    const dialog = await openMembersDialog(user);

    expect(await within(dialog).findByText("developer")).toBeTruthy();
    await user.click(within(dialog).getAllByRole("button", { name: "移出" })[0]!);
    // 移出也是破坏性操作，走二次确认。确认框是**另一层** dialog（叠在成员弹窗之上），
    // 此时页面上有两个 dialog，要按确认文案定位到确认框再点它的「移出」按钮。
    await waitFor(() =>
      expect(
        screen
          .getAllByRole("dialog")
          .some((d) => d.textContent?.includes("确定把该用户移出这个组")),
      ).toBe(true),
    );
    const confirm = screen
      .getAllByRole("dialog")
      .find((d) => d.textContent?.includes("确定把该用户移出这个组"))!;
    await user.click(within(confirm).getByRole("button", { name: "移出" }));

    await waitFor(() => expect(removed).toBe(true));
    // 断言「成员表已空」而不是「页面上没有 developer 文本」：移出后该用户会回到候选下拉里，
    // 文本仍在 DOM 中（Select 会把全部选项渲染出来）。
    await waitFor(() => expect(within(dialog).findByText("该组暂无成员")).toBeTruthy());
    expect(within(dialog).queryByRole("button", { name: "移出" })).toBeNull();
  });

  it("添加成员把所选用户 ID 提交到成员端点", async () => {
    let body: { userId?: number } | null = null;
    server.use(
      http.post("*/api/v1/user-groups/:id/members", async ({ request }) => {
        body = (await request.json()) as { userId?: number };
        return HttpResponse.json(
          { userId: body.userId ?? 0, username: "auditor", createdAt: "2026-01-01T00:00:00Z" },
          { status: 201 },
        );
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<UserGroupsPage />, { route: "/user-groups", authenticated: true });
    const dialog = await openMembersDialog(user);
    await within(dialog).findByText("developer");

    await user.click(within(dialog).getByRole("combobox", { name: "添加成员" }));
    await user.click(await screen.findByRole("option", { name: "auditor" }));
    await user.click(within(dialog).getByRole("button", { name: "添加成员" }));

    // 提交的是所选用户的 ID（种子里 auditor 为 id 5），而不是组 ID 或固定值。
    await waitFor(() => expect(body?.userId).toBe(5));
    expect(await within(dialog).findByText("auditor")).toBeTruthy();
  });

  it("候选用户里排除已在组内的成员", async () => {
    const user = userEvent.setup();
    renderWithProviders(<UserGroupsPage />, { route: "/user-groups", authenticated: true });
    const dialog = await openMembersDialog(user);
    await within(dialog).findByText("developer");

    await user.click(within(dialog).getByRole("combobox", { name: "添加成员" }));
    // developer 已在组内（种子成员），不应再出现在候选里；auditor 仍应可选。
    expect(await screen.findByRole("option", { name: "auditor" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "developer" })).toBeNull();
  });
});
