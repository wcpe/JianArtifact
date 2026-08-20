// FR-103：文件树多选 + 工具栏「删除所选」批量删除——管理员可见，确认后调批量删除 API 并刷新树清空勾选。
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { store } from "@jianartifact/devmock";
import { server } from "@jianartifact/devmock/node";
import { RepoBrowser } from "../src/components/repo/RepoBrowser";
import type { User } from "../src/api/types";
import { renderWithProviders } from "./harness";

const NON_ADMIN_USER: User = {
  id: 2,
  username: "developer",
  role: "user",
  status: "active",
  createdAt: "2026-01-02T00:00:00Z",
};

const JAR_PATH = "com/example/app/1.0.0/app-1.0.0.jar";
const POM_PATH = "com/example/app/1.0.0/app-1.0.0.pom";

/** 逐级展开 maven-releases 目录树到 1.0.0 层。 */
async function expandToVersion(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByText("com"));
  await user.click(await screen.findByText("example"));
  await user.click(await screen.findByText("app"));
  await user.click(await screen.findByText("1.0.0"));
}

function renderBrowser(authenticated: boolean, user?: User) {
  return renderWithProviders(<RepoBrowser repoName="maven-releases" allowUpload />, {
    route: "/",
    authenticated,
    user,
  });
}

describe("FR-103 批量删除", () => {
  it("管理员勾选两个文件后显示「删除所选」，确认后调批量删除 API 并刷新树、清空勾选", async () => {
    const user = userEvent.setup();
    let batchBody: { paths: string[] } | null = null;
    let treeCalls = 0;
    // 拦截批量删除端点与目录树（计数以验证刷新）。
    server.use(
      http.post("*/api/v1/repositories/:name/assets/batch-delete", async ({ request }) => {
        batchBody = (await request.json()) as { paths: string[] };
        return HttpResponse.json({ deleted: 2, failed: [] });
      }),
      http.get("*/api/v1/repositories/:name/tree", ({ request, params }) => {
        treeCalls += 1;
        const url = new URL(request.url);
        const entry = store.listDirectory(
          String(params.name),
          url.searchParams.get("prefix") ?? "",
        );
        return entry
          ? HttpResponse.json(entry)
          : HttpResponse.json(
              { error: { code: "not_found", message: "仓库不存在" } },
              { status: 404 },
            );
      }),
    );

    renderBrowser(true);
    await expandToVersion(user);

    // 勾选 jar 与 pom 两个文件行。
    await user.click(screen.getByRole("checkbox", { name: "选择 app-1.0.0.jar" }));
    await user.click(screen.getByRole("checkbox", { name: "选择 app-1.0.0.pom" }));

    // 工具栏出现「删除所选（2）」。
    const deleteBtn = await screen.findByRole("button", { name: "删除所选（2）" });
    const treeCallsBefore = treeCalls;

    await user.click(deleteBtn);
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "删除" }));

    // 请求体应含全部勾选路径。
    await waitFor(() =>
      expect(batchBody?.paths).toEqual(expect.arrayContaining([JAR_PATH, POM_PATH])),
    );
    // 成功后刷新树并清空勾选。
    await waitFor(() => expect(treeCalls).toBeGreaterThan(treeCallsBefore));
    await waitFor(() => expect(screen.queryByRole("button", { name: "删除所选（2）" })).toBeNull());
    // 成功通知。
    expect(await screen.findByText("已删除 2 个制品")).toBeTruthy();
  });

  it("非管理员不显示复选框与「删除所选」", async () => {
    const user = userEvent.setup();
    renderBrowser(true, NON_ADMIN_USER);
    await expandToVersion(user);
    expect(screen.queryByRole("checkbox", { name: "选择 app-1.0.0.jar" })).toBeNull();
    expect(screen.queryByRole("button", { name: /删除所选/ })).toBeNull();
  });

  it("切换目录后清空原目录勾选，新的目录不能累计批量删除", async () => {
    const user = userEvent.setup();
    server.use(
      http.get("*/api/v1/repositories/:name/tree", ({ request }) => {
        const prefix = new URL(request.url).searchParams.get("prefix") ?? "";
        if (prefix === "") {
          return HttpResponse.json({ directories: ["dir-a/", "dir-b/"], files: [] });
        }
        if (prefix === "dir-a/") {
          return HttpResponse.json({
            directories: [],
            files: [
              {
                path: "dir-a/a.jar",
                size: 1,
                hash: "a",
                updatedAt: "2026-08-19T00:00:00Z",
              },
            ],
          });
        }
        return HttpResponse.json({
          directories: [],
          files: [
            {
              path: "dir-b/b.jar",
              size: 1,
              hash: "b",
              updatedAt: "2026-08-19T00:00:00Z",
            },
          ],
        });
      }),
    );

    renderBrowser(true);
    await user.click(await screen.findByText("dir-a"));
    await user.click(await screen.findByRole("checkbox", { name: "选择 a.jar" }));
    expect(await screen.findByRole("button", { name: "删除所选（1）" })).toBeTruthy();

    await user.click(screen.getByText("dir-b"));
    expect(screen.queryByRole("button", { name: /删除所选/ })).toBeNull();

    await user.click(await screen.findByRole("checkbox", { name: "选择 b.jar" }));
    expect(await screen.findByRole("button", { name: "删除所选（1）" })).toBeTruthy();
  });

  it("进入搜索结果后清空浏览目录中的勾选", async () => {
    const user = userEvent.setup();
    server.use(http.get("*/api/v1/search", () => HttpResponse.json({ items: [], total: 0 })));
    renderBrowser(true);
    await expandToVersion(user);
    await user.click(screen.getByRole("checkbox", { name: "选择 app-1.0.0.jar" }));
    expect(await screen.findByRole("button", { name: "删除所选（1）" })).toBeTruthy();

    await user.type(screen.getByPlaceholderText(/搜索制品/), "app");
    await user.keyboard("{Enter}");

    await waitFor(() => expect(screen.queryByRole("button", { name: /删除所选/ })).toBeNull());
  });

  it("部分失败时提示失败明细，成功部分照常生效并刷新树", async () => {
    const user = userEvent.setup();
    let batchBody: { paths: string[] } | null = null;
    let treeCalls = 0;
    server.use(
      http.post("*/api/v1/repositories/:name/assets/batch-delete", async ({ request }) => {
        batchBody = (await request.json()) as { paths: string[] };
        return HttpResponse.json({
          deleted: 1,
          failed: [{ path: "com/example/app/1.0.0/app-1.0.0.pom", error: "资源不存在" }],
        });
      }),
      http.get("*/api/v1/repositories/:name/tree", ({ request, params }) => {
        treeCalls += 1;
        const url = new URL(request.url);
        const entry = store.listDirectory(
          String(params.name),
          url.searchParams.get("prefix") ?? "",
        );
        return entry
          ? HttpResponse.json(entry)
          : HttpResponse.json(
              { error: { code: "not_found", message: "仓库不存在" } },
              { status: 404 },
            );
      }),
    );

    renderBrowser(true);
    await expandToVersion(user);
    await user.click(screen.getByRole("checkbox", { name: "选择 app-1.0.0.jar" }));
    await user.click(screen.getByRole("checkbox", { name: "选择 app-1.0.0.pom" }));

    const deleteBtn = await screen.findByRole("button", { name: "删除所选（2）" });
    const treeCallsBefore = treeCalls;
    await user.click(deleteBtn);
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "删除" }));

    await waitFor(() =>
      expect(batchBody?.paths).toEqual(expect.arrayContaining([JAR_PATH, POM_PATH])),
    );
    // 失败明细提示。
    expect(await screen.findByText(/部分制品删除失败：/)).toBeTruthy();
    // 成功部分照常刷新树并清空勾选。
    await waitFor(() => expect(treeCalls).toBeGreaterThan(treeCallsBefore));
    await waitFor(() => expect(screen.queryByRole("button", { name: "删除所选（2）" })).toBeNull());
  });
});
