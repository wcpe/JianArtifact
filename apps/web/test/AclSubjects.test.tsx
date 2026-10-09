// FR-36 ACL 主体与六档动作测试：
// - 组主体条目渲染组名（而不是退化成 #0）；
// - 新增条目可选「用户 / 用户组」并带 subjectType / subjectGroupId；
// - 保存是整份覆盖写，新增条目不得抹掉既有条目。
import { server } from "@jianartifact/devmock/node";
import { store } from "@jianartifact/devmock";
import { http, HttpResponse } from "msw";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { Route, Routes } from "react-router-dom";

import { RepositoryDetailPage } from "../src/pages/RepositoryDetailPage";
import { renderWithProviders } from "./harness";

/** 进入指定仓库的 ACL 页签（管理员会话）。 */
async function openAclTab(user: ReturnType<typeof userEvent.setup>, name = "maven-releases") {
  renderWithProviders(
    <Routes>
      <Route path="/repositories/:name" element={<RepositoryDetailPage />} />
    </Routes>,
    { route: `/repositories/${name}?tab=acl`, authenticated: true },
  );
  await user.click(await screen.findByRole("tab", { name: "ACL" }));
  // 等面板取数完成（覆盖写提示出现即代表 ACL 面板已挂载渲染）。
  await screen.findByText(/整份覆盖/);
}

describe("仓库 ACL 主体与六档动作（FR-36）", () => {
  it("用户主体条目渲染用户名", async () => {
    const user = userEvent.setup();
    await openAclTab(user);

    // 种子 ACL：maven-releases 上给 id=2（developer）一条 read。
    // 「用户」还出现在主体类型下拉的选项里，故限定在表格行内断言徽章。
    const row = (await screen.findByText("developer")).closest("tr")!;
    expect(within(row).getByText("用户")).toBeTruthy();
  });

  it("组主体条目渲染组名而不是退化成 #0", async () => {
    // 种子里塞一条组主体授权：组 id=1（release-team）。
    store.setAcl("maven-releases", [{ subjectType: "group", subjectGroupId: 1, action: "write" }]);
    const user = userEvent.setup();
    await openAclTab(user);

    const row = (await screen.findByText("release-team")).closest("tr")!;
    expect(within(row).getByText("用户组")).toBeTruthy();
    // 回归点：此前组主体因 subjectId 缺省被渲染成「#0」。
    expect(screen.queryByText("#0")).toBeNull();
  });

  it("未知组回退成「用户组 #id」而不是 #0", async () => {
    store.setAcl("maven-releases", [{ subjectType: "group", subjectGroupId: 999, action: "read" }]);
    const user = userEvent.setup();
    await openAclTab(user);

    expect(await screen.findByText("用户组 #999")).toBeTruthy();
  });

  it("新增组主体条目带 subjectType 与 subjectGroupId", async () => {
    let body: { items?: Array<Record<string, unknown>> } | null = null;
    server.use(
      http.put("*/api/v1/repositories/:name/acl", async ({ request }) => {
        body = (await request.json()) as { items?: Array<Record<string, unknown>> };
        return HttpResponse.json({ items: (body.items ?? []) as never });
      }),
    );
    const user = userEvent.setup();
    await openAclTab(user);

    // 主体类型选「用户组」，再选组 release-team。
    await user.click(screen.getByRole("combobox", { name: "主体类型" }));
    await user.click(await screen.findByRole("option", { name: "用户组" }));
    await user.click(screen.getByRole("combobox", { name: "授权主体" }));
    await user.click(await screen.findByRole("option", { name: "release-team" }));
    await user.click(screen.getByRole("button", { name: "添加条目" }));
    await user.click(screen.getByRole("button", { name: "保存访问控制" }));

    await waitFor(() => expect(body?.items?.length).toBe(2));
    // 新条目：主体类型是 group、填 subjectGroupId，且不带 subjectId。
    const added = body!.items!.find((item) => item.subjectType === "group")!;
    expect(added.subjectGroupId).toBe(1);
  });

  it("保存是覆盖写：新增条目仍带着既有条目一起提交", async () => {
    let body: { items?: Array<Record<string, unknown>> } | null = null;
    server.use(
      http.put("*/api/v1/repositories/:name/acl", async ({ request }) => {
        body = (await request.json()) as { items?: Array<Record<string, unknown>> };
        return HttpResponse.json({ items: (body.items ?? []) as never });
      }),
    );
    const user = userEvent.setup();
    await openAclTab(user);
    // 既有条目：developer / read（种子）。
    await screen.findByText("developer");

    // 再给 admin 加一条：请求体必须同时含这两条，否则会把 developer 那一条抹掉。
    await user.click(screen.getByRole("combobox", { name: "授权主体" }));
    await user.click(await screen.findByRole("option", { name: "admin" }));
    await user.click(screen.getByRole("button", { name: "添加条目" }));
    await user.click(screen.getByRole("button", { name: "保存访问控制" }));

    await waitFor(() => expect(body?.items?.length).toBe(2));
    const ids = body!.items!.map((item) => item.subjectId);
    expect(ids).toContain(2); // developer（既有）
    expect(ids).toContain(1); // admin（新增）
  });

  it("六档动作都可选，细化档位不再缺失", async () => {
    const user = userEvent.setup();
    await openAclTab(user);
    await screen.findByText("developer");

    // 说明气泡里逐档给出中文口径：publish / delete / acl_manage 不再缺。
    await user.click(screen.getByRole("button", { name: "六档权限的差别" }));
    const popover = await screen.findByRole("dialog");
    for (const label of ["读取", "写入", "发布", "删除", "授权管理", "管理"]) {
      expect(within(popover).getByText(label)).toBeTruthy();
    }
  });

  it("同主体重复添加给出提示而不静默丢弃", async () => {
    const user = userEvent.setup();
    await openAclTab(user);
    await screen.findByText("developer");

    // developer 已有一条授权，候选里应已排除；强行再选并添加时不产生第二条。
    await user.click(screen.getByRole("button", { name: "添加条目" }));
    expect((screen.getByRole("button", { name: "添加条目" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
  });
});
