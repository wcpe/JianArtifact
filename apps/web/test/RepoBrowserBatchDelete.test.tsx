// FR-103/108：旧批删入口回归到统一资产操作 API，选择方式改为树行交互。
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

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

async function expandToVersion(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByText("com"));
  await user.click(await screen.findByText("example"));
  await user.click(await screen.findByText("app"));
  await user.click(await screen.findByText("1.0.0"));
}

function renderBrowser(authenticated: boolean, user?: User) {
  return renderWithProviders(
    <RepoBrowser repoName="maven-releases" forcedFormat="raw" forcedType="hosted" />,
    { route: "/", authenticated, user },
  );
}

describe("FR-105 右键资产操作", () => {
  it("管理员多选后不显示删除所选工具栏，右键删除调用统一 API 并清空选择", async () => {
    const user = userEvent.setup();
    let body: { action: string; targets: { type: string; path: string }[] } | null = null;
    server.use(
      http.post("*/api/v1/repositories/:name/assets/operations", async ({ request }) => {
        body = (await request.json()) as typeof body;
        return HttpResponse.json({ operationId: "op-1", affected: 2 });
      }),
    );
    renderBrowser(true);
    await expandToVersion(user);
    await user.click(screen.getByText("app-1.0.0.jar"));
    fireEvent.click(screen.getByText("app-1.0.0.pom"), { ctrlKey: true });

    expect(screen.queryByRole("button", { name: /删除所选/ })).toBeNull();
    fireEvent.contextMenu(screen.getByText("app-1.0.0.jar"));
    await user.click(await screen.findByRole("menuitem", { name: "删除" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "删除" }));

    await waitFor(() =>
      expect(body).toEqual({
        action: "delete",
        overrideReason: "管理员资产操作",
        targets: [
          { type: "raw_path", path: JAR_PATH },
          { type: "raw_path", path: POM_PATH },
        ],
      }),
    );
  });

  it("非管理员不显示右键危险操作或复选框", async () => {
    const user = userEvent.setup();
    renderBrowser(true, NON_ADMIN_USER);
    await expandToVersion(user);
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(screen.queryByRole("button", { name: /删除所选/ })).toBeNull();
    fireEvent.contextMenu(screen.getByText("app-1.0.0.jar"));
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("右键统一操作失败时页面仍不显示工具栏", async () => {
    const user = userEvent.setup();
    server.use(
      http.post("*/api/v1/repositories/:name/assets/operations", () =>
        HttpResponse.json({ error: { code: "conflict", message: "操作失败" } }, { status: 409 }),
      ),
    );
    renderBrowser(true);
    await expandToVersion(user);
    await user.click(screen.getByText("app-1.0.0.jar"));
    expect(screen.queryByRole("button", { name: /删除所选/ })).toBeNull();
    fireEvent.contextMenu(screen.getByText("app-1.0.0.jar"));
    await user.click(await screen.findByRole("menuitem", { name: "删除" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "删除" }));
    await waitFor(() => expect(screen.getByText("操作失败")).toBeTruthy());
    expect(screen.queryByRole("button", { name: /删除所选/ })).toBeNull();
  });
});
