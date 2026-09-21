import { http, HttpResponse } from "msw";
import { Route, Routes } from "react-router-dom";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { store } from "@jianartifact/devmock";
import { server } from "@jianartifact/devmock/node";
import {
  releaseDevMockPendingRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { AclPage } from "../src/pages/AclPage";
import { renderWithProviders } from "./harness";

const MEMBER = {
  id: 2,
  username: "developer",
  role: "user" as const,
  status: "active" as const,
  createdAt: "2026-01-02T00:00:00Z",
};

function renderAcl(
  options: { user?: typeof MEMBER; token?: string } = {},
  route = "/repositories/maven-releases/acl",
) {
  window.history.replaceState({}, "", route);
  return renderWithProviders(
    <Routes>
      <Route path="/repositories/:name/acl" element={<AclPage />} />
    </Routes>,
    { route, authenticated: true, ...options },
  );
}

describe("仓库访问控制 Mock", () => {
  it("保存 ACL 成功后展示反馈并保留当前编辑结果", async () => {
    const user = userEvent.setup();
    renderAcl();

    await screen.findByText("developer");
    const action = screen.getAllByRole("combobox")[0]!;
    await user.click(action);
    await user.click(await screen.findByRole("option", { name: "写入" }));
    await user.click(screen.getByRole("button", { name: "保存访问控制" }));

    expect(await screen.findByText("保存成功")).toBeTruthy();
    await waitFor(() => expect((action as HTMLInputElement).value).toBe("写入"));
  });

  it("备用只读拒绝 ACL 保存时保留用户编辑", async () => {
    server.use(
      http.put("*/api/v1/repositories/:name/acl", () =>
        HttpResponse.json(
          { error: { code: "standby_read_only", message: "备用节点为只读，当前请求已拒绝" } },
          { status: 503 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderAcl();

    await screen.findByText("developer");
    const action = screen.getAllByRole("combobox")[0]!;
    await user.click(action);
    await user.click(await screen.findByRole("option", { name: "写入" }));
    await user.click(screen.getByRole("button", { name: "保存访问控制" }));

    expect(await screen.findByText("备用节点为只读，当前请求已拒绝")).toBeTruthy();
    expect((action as HTMLInputElement).value).toBe("写入");
  });

  it("empty 场景显示可继续编辑的空 ACL", async () => {
    renderAcl({}, "/repositories/maven-releases/acl?__mock=empty");
    expect(await screen.findByText("尚未配置任何条目")).toBeTruthy();
  });

  it("loading 场景显式释放后恢复既有 ACL", async () => {
    renderAcl({}, "/repositories/maven-releases/acl?__mock=loading");
    expect(await screen.findByTestId("state-loading")).toBeTruthy();
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBeGreaterThan(0);
    expect(await screen.findByText("developer")).toBeTruthy();
  });

  it("error 场景提供 ACL 读取重试", async () => {
    renderAcl({}, "/repositories/maven-releases/acl?__mock=error");
    expect(await screen.findByTestId("state-error")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
  });

  it("仓库 admin 授权用户在不能列用户时仍可编辑既有 ACL", async () => {
    store.setAcl("maven-releases", [{ subjectId: 2, action: "admin" }]);
    const user = userEvent.setup();
    renderAcl({ user: MEMBER, token: "mock.jwt.token:user" });

    expect(await screen.findByText("#2")).toBeTruthy();
    expect(screen.queryByText("需要管理员权限")).toBeNull();
    const action = screen.getAllByRole("combobox")[0]!;
    await user.click(action);
    await user.click(await screen.findByRole("option", { name: "写入" }));
    await user.click(screen.getByRole("button", { name: "保存访问控制" }));

    expect(await screen.findByText("保存成功")).toBeTruthy();
    expect((action as HTMLInputElement).value).toBe("写入");
  });

  it("删除 ACL 条目后保存授权并展示成功反馈", async () => {
    let saved: unknown = null;
    server.use(
      http.put("*/api/v1/repositories/:name/acl", async ({ request }) => {
        saved = await request.json();
        return HttpResponse.json({ items: [] });
      }),
    );
    const user = userEvent.setup();
    renderAcl();

    const row = (await screen.findByText("developer")).closest("tr")!;
    await user.click(within(row).getByLabelText("删除"));
    await waitFor(() => expect(row.isConnected).toBe(false));
    await user.click(screen.getByRole("button", { name: "保存访问控制" }));

    await waitFor(() => expect(saved).toEqual({ items: [] }));
    expect(await screen.findByText("保存成功")).toBeTruthy();
  });

  it("删除 ACL 条目保存失败时保留本地删除结果", async () => {
    server.use(
      http.put("*/api/v1/repositories/:name/acl", () =>
        HttpResponse.json(
          { error: { code: "standby_read_only", message: "备用节点为只读，当前请求已拒绝" } },
          { status: 503 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderAcl();

    const row = (await screen.findByText("developer")).closest("tr")!;
    await user.click(within(row).getByLabelText("删除"));
    await user.click(screen.getByRole("button", { name: "保存访问控制" }));

    expect(await screen.findByText("备用节点为只读，当前请求已拒绝")).toBeTruthy();
    expect(row.isConnected).toBe(false);
  });
});
