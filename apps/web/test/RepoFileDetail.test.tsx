// FR-102：文件详情删除按钮——仅管理员可见可用；确认后调用协议 DELETE 并刷新文件树。
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

/** 逐级展开 maven-releases 目录树并点选 jar 文件，等右侧详情渲染。 */
async function openJarDetail(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByText("com"));
  await user.click(await screen.findByText("example"));
  await user.click(await screen.findByText("app"));
  await user.click(await screen.findByText("1.0.0"));
  await user.click(await screen.findByText("app-1.0.0.jar"));
  await screen.findByText(JAR_PATH);
}

function renderBrowser(authenticated: boolean, user?: User) {
  return renderWithProviders(<RepoBrowser repoName="maven-releases" allowUpload />, {
    route: "/",
    authenticated,
    user,
  });
}

describe("FR-102 文件删除", () => {
  it("管理员选中文件后显示删除按钮", async () => {
    const user = userEvent.setup();
    renderBrowser(true);
    await openJarDetail(user);
    expect(screen.getByRole("button", { name: "删除" })).toBeTruthy();
  });

  it("非管理员选中文件后不显示删除按钮", async () => {
    const user = userEvent.setup();
    renderBrowser(true, NON_ADMIN_USER);
    await openJarDetail(user);
    expect(screen.queryByRole("button", { name: "删除" })).toBeNull();
  });

  it("确认删除后调用协议 DELETE 并刷新文件树", async () => {
    const user = userEvent.setup();
    let deletedUrl = "";
    let treeCalls = 0;
    // 拦截协议 DELETE（devmock 未实现）与目录树（计数以验证刷新）。
    server.use(
      http.delete("*/repository/maven-releases/*", ({ request }) => {
        deletedUrl = request.url;
        return new HttpResponse(null, { status: 204 });
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
    await openJarDetail(user);
    const callsBeforeDelete = treeCalls;

    await user.click(screen.getByRole("button", { name: "删除" }));
    // 确认对话框出现后点「删除」确认。
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "删除" }));

    // DELETE 命中协议路径且带 Bearer。
    await waitFor(() =>
      expect(deletedUrl).toContain(
        `/repository/maven-releases/com/example/app/1.0.0/app-1.0.0.jar`,
      ),
    );
    // 成功后触发文件树刷新（重新拉取根目录）。
    await waitFor(() => expect(treeCalls).toBeGreaterThan(callsBeforeDelete));
    // 成功通知。
    expect(await screen.findByText("删除成功")).toBeTruthy();
  });
});
