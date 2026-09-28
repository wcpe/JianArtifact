// 用户管理集成测试：渲染种子用户列表、走完新建用户表单落库回显，
// 并覆盖发布策略（FR-109）从单仓库到多仓库的批量应用。
import { server } from "@jianartifact/devmock/node";
import { http, HttpResponse } from "msw";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { UsersPage } from "../src/pages/UsersPage";
import { renderWithProviders } from "./harness";

/** 打开首个用户（admin）的发布策略弹窗。 */
async function openPolicyDialog(user: ReturnType<typeof userEvent.setup>) {
  await screen.findByText("admin");
  await user.click(screen.getAllByRole("button", { name: "发布策略" })[0]!);
  return await screen.findByRole("dialog");
}

/** 在弹窗的仓库多选里选/取消选一个仓库。 */
async function toggleRepo(
  user: ReturnType<typeof userEvent.setup>,
  dialog: HTMLElement,
  name: string,
) {
  await user.click(within(dialog).getByRole("combobox", { name: "Hosted 仓库" }));
  await user.click(await screen.findByRole("option", { name }));
}

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

  it("可禁止 Web 登录并打开发布策略弹窗", async () => {
    const user = userEvent.setup();
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    await screen.findByText("admin");

    await user.click(screen.getByRole("switch", { name: "允许 Web 登录 admin" }));
    await waitFor(() =>
      expect(
        (screen.getByRole("switch", { name: "允许 Web 登录 admin" }) as HTMLInputElement).checked,
      ).toBe(false),
    );

    const dialog = await openPolicyDialog(user);
    expect(await within(dialog).findByText("允许的路径前缀")).toBeTruthy();
    // 多选仓库替换了原来的单选，且带用途说明（用户问过「发布策略干什么用」）。
    expect(within(dialog).getByRole("combobox", { name: "Hosted 仓库" })).toBeTruthy();
    expect(within(dialog).getByText(/路径前缀与配额/)).toBeTruthy();
  });

  it("单仓保存走批量端点并把逐仓库结果显示出来", async () => {
    let body: { repositories?: string[] } | null = null;
    server.use(
      http.put("*/api/v1/users/:id/publish-policies", async ({ request }) => {
        body = (await request.json()) as { repositories?: string[] };
        return HttpResponse.json({
          results: (body.repositories ?? []).map((repository) => ({ repository, ok: true })),
        });
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    const dialog = await openPolicyDialog(user);

    const prefixes = within(dialog).getByLabelText("允许的路径前缀");
    await user.clear(prefixes);
    await user.type(prefixes, "releases\nsnapshots");
    await user.clear(within(dialog).getByLabelText("每小时制品数上限"));
    await user.type(within(dialog).getByLabelText("每小时制品数上限"), "7");
    await user.clear(within(dialog).getByLabelText("每日字节数上限"));
    await user.type(within(dialog).getByLabelText("每日字节数上限"), "1024");
    await user.clear(within(dialog).getByLabelText("单文件字节数上限"));
    await user.type(within(dialog).getByLabelText("单文件字节数上限"), "512");
    await user.click(within(dialog).getByRole("button", { name: "保存" }));

    await waitFor(() => expect(body?.repositories).toEqual(["maven-releases"]));
    // 逐仓库结果可见。
    expect(await within(dialog).findByText("保存结果")).toBeTruthy();
    expect(within(dialog).getAllByText("maven-releases").length).toBeGreaterThan(0);
    expect(within(dialog).getByText("成功")).toBeTruthy();
    // 编辑内容保留（未被保存响应覆盖）。
    expect((prefixes as HTMLTextAreaElement).value).toBe("releases\nsnapshots");
    expect((within(dialog).getByLabelText("每小时制品数上限") as HTMLInputElement).value).toBe("7");
  });

  it("多选仓库时把同一份策略批量应用到全部所选仓库", async () => {
    let body: { repositories?: string[]; allowedPrefixes?: string[] } | null = null;
    server.use(
      http.put("*/api/v1/users/:id/publish-policies", async ({ request }) => {
        body = (await request.json()) as { repositories?: string[]; allowedPrefixes?: string[] };
        return HttpResponse.json({
          results: (body.repositories ?? []).map((repository) => ({ repository, ok: true })),
        });
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    const dialog = await openPolicyDialog(user);
    await within(dialog).findByText("允许的路径前缀");

    await toggleRepo(user, dialog, "raw-hosted");
    // 多选口径提示：保存会统一覆盖。
    expect(await within(dialog).findByText(/统一应用到所选仓库/)).toBeTruthy();

    await user.click(within(dialog).getByRole("button", { name: "保存" }));

    await waitFor(() => expect(body?.repositories).toEqual(["maven-releases", "raw-hosted"]));
    // 两条结果都是成功：逐仓库结果各给一枚徽章。
    await waitFor(() => expect(within(dialog).getAllByText("成功").length).toBe(2));
  });

  it("发布策略弹窗跨页取全量仓库（total 超单页上限时继续翻页）", async () => {
    const pages: number[] = [];
    server.use(
      http.get("*/api/v1/repositories", ({ request }) => {
        const page = Number(new URL(request.url).searchParams.get("page") ?? "1");
        pages.push(page);
        const items =
          page === 1
            ? [
                {
                  id: 1,
                  name: "maven-releases",
                  format: "maven",
                  type: "hosted",
                  visibility: "private",
                },
              ]
            : [
                {
                  id: 999,
                  name: "page2-hosted",
                  format: "raw",
                  type: "hosted",
                  visibility: "private",
                },
              ];
        // total 超过单页上限（100）：只取第一页会漏掉后面的 hosted 仓库。
        return HttpResponse.json({ items, total: 101 });
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    const dialog = await openPolicyDialog(user);

    await waitFor(() => expect(pages).toContain(2));
    await user.click(within(dialog).getByRole("combobox", { name: "Hosted 仓库" }));
    expect(await screen.findByRole("option", { name: /page2-hosted/ })).toBeTruthy();
  });

  it("逐仓库结果里列出失败仓库与原因", async () => {
    server.use(
      http.put("*/api/v1/users/:id/publish-policies", () =>
        HttpResponse.json({
          results: [
            { repository: "maven-releases", ok: true },
            { repository: "raw-hosted", ok: false, error: "存储写入失败" },
          ],
        }),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    const dialog = await openPolicyDialog(user);
    await within(dialog).findByText("允许的路径前缀");

    await toggleRepo(user, dialog, "raw-hosted");
    await user.click(within(dialog).getByRole("button", { name: "保存" }));

    // 失败仓库与其原因都必须可见，而不是只给一句笼统的失败。
    expect(await within(dialog).findByText("存储写入失败")).toBeTruthy();
    expect(within(dialog).getByText("失败")).toBeTruthy();
    expect(await screen.findByText("1 个仓库保存失败")).toBeTruthy();
  });

  it("未选择仓库时禁止保存并给出提示", async () => {
    const user = userEvent.setup();
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    const dialog = await openPolicyDialog(user);
    await within(dialog).findByText("允许的路径前缀");

    // 取消唯一的已选仓库（MultiSelect 里再点一次已选选项即取消）。
    await toggleRepo(user, dialog, "maven-releases");

    expect(await within(dialog).findByText("请至少选择一个仓库")).toBeTruthy();
    expect(
      (within(dialog).getByRole("button", { name: "保存" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  it("整体拒绝时保留编辑内容并展示后端错误", async () => {
    server.use(
      http.put("*/api/v1/users/:id/publish-policies", () =>
        HttpResponse.json(
          {
            error: {
              code: "validation_error",
              message: "仓库 raw-hosted：不是 Hosted 仓库，无法配置发布策略",
            },
          },
          { status: 400 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    const dialog = await openPolicyDialog(user);
    await within(dialog).findByText("允许的路径前缀");

    const prefixes = within(dialog).getByLabelText("允许的路径前缀");
    await user.clear(prefixes);
    await user.type(prefixes, "restricted");
    await user.click(within(dialog).getByRole("button", { name: "保存" }));

    expect(
      await screen.findByText("仓库 raw-hosted：不是 Hosted 仓库，无法配置发布策略"),
    ).toBeTruthy();
    expect((prefixes as HTMLTextAreaElement).value).toBe("restricted");
    expect(within(dialog).getByRole("button", { name: "保存" })).toBeTruthy();
  });
});
