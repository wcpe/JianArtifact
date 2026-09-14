// MSW 双端拦截层测试：经 Node server 拦截全局 fetch，校验 0.2.0 端点的契约行为。
// 与 contract.test.ts（纯响应函数 ↔ 契约）互补：此处覆盖状态流转、鉴权与错误码。
import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { server } from "../src/node";
import {
  DEV_MOCK_ROUTE_HEADER,
  DEV_MOCK_SCENARIO_HEADER,
  pendingDevMockRequests,
  releaseDevMockPendingRequests,
  resetDevMockScenario,
  setDevMockScenario,
  waitForDevMockPendingRequest,
} from "../src/scenario";
import { MOCK_TOKEN, resetStore, store } from "../src/store";

const auth = { Authorization: `Bearer ${MOCK_TOKEN}` };
const userAuth = { Authorization: "Bearer mock.jwt.token:user" };

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  resetDevMockScenario();
  resetStore();
});
afterAll(() => server.close());

describe("devmock MSW 端点行为", () => {
  it("正常场景透传给既有 handler，路由范围的错误场景只影响对应请求", async () => {
    setDevMockScenario("/users", "error");

    const normal = await fetch("http://localhost/api/v1/status", {
      headers: { [DEV_MOCK_SCENARIO_HEADER]: "normal", [DEV_MOCK_ROUTE_HEADER]: "/dashboard" },
    });
    expect(normal.status).toBe(200);

    const scoped = await fetch("http://localhost/api/v1/users", {
      headers: { ...auth, [DEV_MOCK_ROUTE_HEADER]: "/users" },
    });
    expect(scoped.status).toBe(500);
    await expect(scoped.json()).resolves.toEqual({
      error: expect.objectContaining({ code: "devmock_scenario_error" }),
    });

    const otherRoute = await fetch("http://localhost/api/v1/users", {
      headers: { ...auth, [DEV_MOCK_ROUTE_HEADER]: "/tokens" },
    });
    expect(otherRoute.status).toBe(200);
  });

  it("显式错误场景拦截写请求且不会改动 store", async () => {
    const before = (await fetch("http://localhost/api/v1/users", { headers: auth }).then((res) =>
      res.json(),
    )) as {
      total: number;
    };
    const failed = await fetch("http://localhost/api/v1/users", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...auth,
        [DEV_MOCK_SCENARIO_HEADER]: "error",
        [DEV_MOCK_ROUTE_HEADER]: "/users",
      },
      body: JSON.stringify({ username: "scenario-error", password: "password1" }),
    });
    expect(failed.status).toBe(500);

    const after = (await fetch("http://localhost/api/v1/users", { headers: auth }).then((res) =>
      res.json(),
    )) as {
      total: number;
    };
    expect(after.total).toBe(before.total);
  });

  it("备用只读场景与真实服务一致地拒绝协议写入和退出登录", async () => {
    const before = store.status();
    const standbyHeaders = {
      ...auth,
      [DEV_MOCK_SCENARIO_HEADER]: "standby_read_only",
      [DEV_MOCK_ROUTE_HEADER]: "/repositories/maven-releases",
    };

    for (const method of ["PUT", "DELETE"]) {
      const response = await fetch("http://localhost/repository/maven-releases/example.jar", {
        method,
        headers: standbyHeaders,
      });
      expect(response.status).toBe(503);
      await expect(response.json()).resolves.toEqual(
        expect.objectContaining({ error: expect.objectContaining({ code: "standby_read_only" }) }),
      );
    }

    const logout = await fetch("http://localhost/api/v1/auth/logout", {
      method: "POST",
      headers: standbyHeaders,
    });
    expect(logout.status).toBe(503);
    await expect(logout.json()).resolves.toEqual(
      expect.objectContaining({ error: expect.objectContaining({ code: "standby_read_only" }) }),
    );

    const login = await fetch("http://localhost/api/v1/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...standbyHeaders },
      body: JSON.stringify({ username: "admin", password: "whatever" }),
    });
    expect(login.status).toBe(200);
    expect(store.status()).toEqual(before);
  });

  it("加载场景仅在显式释放或重置 pending 后继续，不依赖计时器", async () => {
    const pending = fetch("http://localhost/api/v1/status", {
      headers: { [DEV_MOCK_SCENARIO_HEADER]: "loading", [DEV_MOCK_ROUTE_HEADER]: "/dashboard" },
    });
    await waitForDevMockPendingRequest();
    expect(pendingDevMockRequests()).toBe(1);

    expect(releaseDevMockPendingRequests()).toBe(1);
    expect((await pending).status).toBe(200);
    expect(pendingDevMockRequests()).toBe(0);

    const resetPending = fetch("http://localhost/api/v1/status", {
      headers: { [DEV_MOCK_SCENARIO_HEADER]: "loading", [DEV_MOCK_ROUTE_HEADER]: "/dashboard" },
    });
    await waitForDevMockPendingRequest();
    resetDevMockScenario();
    expect((await resetPending).status).toBe(200);
    expect(pendingDevMockRequests()).toBe(0);
  });

  it("加载请求取消后会立即从 pending 中移除且不能再被释放", async () => {
    const controller = new AbortController();
    const pending = fetch("http://localhost/api/v1/status", {
      headers: { [DEV_MOCK_SCENARIO_HEADER]: "loading", [DEV_MOCK_ROUTE_HEADER]: "/dashboard" },
      signal: controller.signal,
    });
    await waitForDevMockPendingRequest();
    expect(pendingDevMockRequests()).toBe(1);

    controller.abort();
    await expect(pending).rejects.toMatchObject({ name: "AbortError" });
    expect(pendingDevMockRequests()).toBe(0);
    expect(releaseDevMockPendingRequests()).toBe(0);
  });

  it("公开状态端点无需鉴权", async () => {
    const res = await fetch("http://localhost/api/v1/status");
    expect(res.status).toBe(200);
    const body = (await res.json()) as { initialized: boolean; userCount: number };
    expect(body.initialized).toBe(true);
    expect(body.userCount).toBeGreaterThan(0);
  });

  it("基础设置在开发态 Mock 中可读取和保存", async () => {
    const current = await fetch("http://localhost/api/v1/settings", { headers: auth });
    expect(current.status).toBe(200);
    await expect(current.json()).resolves.toMatchObject({
      anonymousAccess: true,
      publicUrl: "https://repo.example.com",
      upstreamTimeout: 30,
    });

    const saved = await fetch("http://localhost/api/v1/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({
        anonymousAccess: false,
        publicUrl: "https://repo.example.com",
        upstreamTimeout: 45,
      }),
    });
    expect(saved.status).toBe(200);
    await expect(saved.json()).resolves.toMatchObject({
      anonymousAccess: false,
      upstreamTimeout: 45,
    });
  });

  it("在线迁移接收直填地址和认证，但任务不会保存或返回认证材料", async () => {
    const password = "private-password";
    const token = "private-token";
    const discovered = await fetch("http://localhost/api/v1/migrations/discover", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({
        sourceType: "online_rest",
        sourceConfig: { url: "https://nexus.example" },
        sourceAuth: { type: "basic", username: "nexus-user", password },
      }),
    });
    expect(discovered.status).toBe(200);

    const list = await fetch("http://localhost/api/v1/migrations", { headers: auth });
    expect(list.status).toBe(200);
    const body = (await list.json()) as { items: Record<string, unknown>[] };
    const task = body.items[0];
    expect(task).toMatchObject({
      sourceConfig: { url: "https://nexus.example" },
      sourceAuthType: "basic",
    });
    expect(JSON.stringify(task)).not.toContain(password);
    expect(JSON.stringify(task)).not.toContain(token);
    expect(task).not.toHaveProperty("sourceAuth");

    const created = await fetch("http://localhost/api/v1/migrations", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({
        sourceType: "online_rest",
        sourceConfig: { url: "https://nexus.example" },
        sourceAuth: { type: "bearer", token },
      }),
    });
    expect(created.status).toBe(201);
    const createdTask = (await created.json()) as Record<string, unknown>;
    expect(createdTask).toMatchObject({ sourceAuthType: "bearer" });
    expect(JSON.stringify(createdTask)).not.toContain(token);
    expect(createdTask).not.toHaveProperty("sourceAuth");
  });

  it("迁移仅管理员可操作，发现后保持 planned 并由显式 start 推进", async () => {
    const userList = await fetch("http://localhost/api/v1/migrations", { headers: userAuth });
    expect(userList.status).toBe(403);

    const userTokenName = "nexus-user-token-name";
    const userTokenSecret = "nexus-user-token-secret";
    const discovered = await fetch("http://localhost/api/v1/migrations/discover", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({
        sourceType: "online_rest",
        sourceConfig: { url: "https://nexus.example" },
        sourceAuth: { type: "basic", username: userTokenName, password: userTokenSecret },
      }),
    });
    expect(discovered.status).toBe(200);
    const body = (await discovered.json()) as { taskId: number };

    const planned = await fetch(`http://localhost/api/v1/migrations/${body.taskId}`, {
      headers: auth,
    });
    expect(planned.status).toBe(200);
    const task = (await planned.json()) as Record<string, unknown>;
    expect(task).toMatchObject({ status: "planned", sourceAuthType: "basic" });
    expect(JSON.stringify(task)).not.toContain(userTokenName);
    expect(JSON.stringify(task)).not.toContain(userTokenSecret);

    const started = await fetch(`http://localhost/api/v1/migrations/${body.taskId}/start`, {
      method: "POST",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({}),
    });
    expect(started.status).toBe(200);
    await expect(started.json()).resolves.toMatchObject({ status: "running" });

    const repeatedStart = await fetch(`http://localhost/api/v1/migrations/${body.taskId}/start`, {
      method: "POST",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({}),
    });
    expect(repeatedStart.status).toBe(409);
  });

  it("迁移 normal 场景自动提供完整生命周期夹具", async () => {
    const listed = await fetch("http://localhost/api/v1/migrations?page_size=20", {
      headers: {
        ...auth,
        [DEV_MOCK_SCENARIO_HEADER]: "normal",
        [DEV_MOCK_ROUTE_HEADER]: "/migrations",
      },
    });

    const list = (await listed.json()) as { items: { status: string }[]; total: number };
    expect(list.total).toBeGreaterThan(0);
    expect(new Set(list.items.map((task) => task.status))).toEqual(
      new Set(["planned", "running", "failed", "cancelled", "completed"]),
    );

    const normalHeaders = {
      ...auth,
      "Content-Type": "application/json",
      [DEV_MOCK_SCENARIO_HEADER]: "normal",
      [DEV_MOCK_ROUTE_HEADER]: "/migrations/1",
    };
    const detail = await fetch("http://localhost/api/v1/migrations/1", { headers: normalHeaders });
    await expect(detail.json()).resolves.toMatchObject({ id: 1, status: "planned" });

    const started = await fetch("http://localhost/api/v1/migrations/1/start", {
      method: "POST",
      headers: normalHeaders,
      body: JSON.stringify({}),
    });
    await expect(started.json()).resolves.toMatchObject({ id: 1, status: "running" });

    const report = await fetch("http://localhost/api/v1/migrations/1/report", {
      headers: normalHeaders,
    });
    await expect(report.json()).resolves.toMatchObject({ taskId: 1, status: "running" });
  });

  it("迁移生命周期夹具包含全部终态，非法操作不改变任务状态", async () => {
    store.seedMigrationLifecycleFixtures();

    const listed = await fetch("http://localhost/api/v1/migrations?page_size=20", {
      headers: auth,
    });
    expect(listed.status).toBe(200);
    const list = (await listed.json()) as { items: { id: number; status: string }[] };
    expect(new Set(list.items.map((task) => task.status))).toEqual(
      new Set(["planned", "running", "failed", "cancelled", "completed"]),
    );

    const planned = list.items.find((task) => task.status === "planned");
    const failed = list.items.find((task) => task.status === "failed");
    const completed = list.items.find((task) => task.status === "completed");
    expect(planned).toBeDefined();
    expect(failed).toBeDefined();
    expect(completed).toBeDefined();

    const rejectedResume = await fetch(`http://localhost/api/v1/migrations/${planned?.id}/resume`, {
      method: "POST",
      headers: auth,
    });
    expect(rejectedResume.status).toBe(409);

    const cancelled = await fetch(`http://localhost/api/v1/migrations/${planned?.id}/cancel`, {
      method: "POST",
      headers: auth,
    });
    await expect(cancelled.json()).resolves.toMatchObject({ status: "cancelled" });

    const resumed = await fetch(`http://localhost/api/v1/migrations/${failed?.id}/resume`, {
      method: "POST",
      headers: auth,
    });
    await expect(resumed.json()).resolves.toMatchObject({ status: "running" });

    const finalized = await fetch(`http://localhost/api/v1/migrations/${completed?.id}/finalize`, {
      method: "POST",
      headers: auth,
    });
    await expect(finalized.json()).resolves.toMatchObject({ status: "completed" });

    const rejectedCancel = await fetch(
      `http://localhost/api/v1/migrations/${completed?.id}/cancel`,
      {
        method: "POST",
        headers: auth,
      },
    );
    expect(rejectedCancel.status).toBe(409);

    const stillCompleted = await fetch(`http://localhost/api/v1/migrations/${completed?.id}`, {
      headers: auth,
    });
    await expect(stillCompleted.json()).resolves.toMatchObject({ status: "completed" });
  });

  it("迁移读取支持空态、受控失败、显式加载释放与任务 404", async () => {
    const empty = await fetch("http://localhost/api/v1/migrations", {
      headers: {
        ...auth,
        [DEV_MOCK_SCENARIO_HEADER]: "empty",
        [DEV_MOCK_ROUTE_HEADER]: "/migrations",
      },
    });
    await expect(empty.json()).resolves.toEqual({ items: [], total: 0 });

    const failed = await fetch("http://localhost/api/v1/migrations", {
      headers: {
        ...auth,
        [DEV_MOCK_SCENARIO_HEADER]: "error",
        [DEV_MOCK_ROUTE_HEADER]: "/migrations",
      },
    });
    expect(failed.status).toBe(500);

    const pending = fetch("http://localhost/api/v1/migrations", {
      headers: {
        ...auth,
        [DEV_MOCK_SCENARIO_HEADER]: "loading",
        [DEV_MOCK_ROUTE_HEADER]: "/migrations",
      },
    });
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBe(1);
    expect((await pending).status).toBe(200);

    const missing = await fetch("http://localhost/api/v1/migrations/999", { headers: auth });
    expect(missing.status).toBe(404);
  });

  it("迁移详情 loading 释放后保持生命周期夹具，empty 详情返回稳定空任务", async () => {
    const loadingHeaders = {
      ...auth,
      "Content-Type": "application/json",
      [DEV_MOCK_SCENARIO_HEADER]: "loading",
      [DEV_MOCK_ROUTE_HEADER]: "/migrations/1",
    };
    const pendingDetail = fetch("http://localhost/api/v1/migrations/1", {
      headers: loadingHeaders,
    });
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBe(1);
    await expect(pendingDetail.then((response) => response.json())).resolves.toMatchObject({
      id: 1,
      status: "planned",
    });

    const pendingStart = fetch("http://localhost/api/v1/migrations/1/start", {
      method: "POST",
      headers: loadingHeaders,
      body: JSON.stringify({}),
    });
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBe(1);
    await expect(pendingStart.then((response) => response.json())).resolves.toMatchObject({
      id: 1,
      status: "running",
    });

    const report = await fetch("http://localhost/api/v1/migrations/1/report", {
      headers: {
        ...auth,
        [DEV_MOCK_SCENARIO_HEADER]: "normal",
        [DEV_MOCK_ROUTE_HEADER]: "/migrations/1",
      },
    });
    await expect(report.json()).resolves.toMatchObject({ taskId: 1, status: "running" });

    const emptyHeaders = {
      ...auth,
      [DEV_MOCK_SCENARIO_HEADER]: "empty",
      [DEV_MOCK_ROUTE_HEADER]: "/migrations/1",
    };
    const emptyDetail = await fetch("http://localhost/api/v1/migrations/1", {
      headers: emptyHeaders,
    });
    expect(emptyDetail.status).toBe(200);
    await expect(emptyDetail.json()).resolves.toMatchObject({
      id: 1,
      status: "planned",
      plan: { repositories: [] },
    });

    const emptyReport = await fetch("http://localhost/api/v1/migrations/1/report", {
      headers: emptyHeaders,
    });
    await expect(emptyReport.json()).resolves.toMatchObject({
      taskId: 1,
      status: "planned",
      totals: { copied: 0, skipped: 0, failed: 0 },
    });

    const missing = await fetch("http://localhost/api/v1/migrations/999", {
      headers: emptyHeaders,
    });
    expect(missing.status).toBe(404);
  });

  it("远程 Nexus 仓库索引接收直填地址和认证且不回显令牌", async () => {
    const token = "private-token";
    const res = await fetch("http://localhost/api/v1/migrations/remote-repositories", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({
        sourceConfig: { url: "https://nexus.example" },
        sourceAuth: { type: "bearer", token },
      }),
    });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { items: { name: string }[]; total: number };
    expect(body.total).toBe(body.items.length);
    expect(JSON.stringify(body)).not.toContain(token);
  });

  it("受保护端点缺令牌返回 401", async () => {
    const res = await fetch("http://localhost/api/v1/users");
    expect(res.status).toBe(401);
  });

  it("审计日志缺令牌 401、普通用户 403、管理员可按筛选与分页读取", async () => {
    expect((await fetch("http://localhost/api/v1/audit-logs")).status).toBe(401);
    expect((await fetch("http://localhost/api/v1/audit-logs", { headers: userAuth })).status).toBe(
      403,
    );

    const filtered = await fetch(
      "http://localhost/api/v1/audit-logs?actor=alice&action=asset.delete&repo=maven-releases&from=2026-01-01T00%3A00%3A00Z&to=2026-01-02T00%3A00%3A00Z&limit=1&offset=0",
      { headers: auth },
    );
    expect(filtered.status).toBe(200);
    await expect(filtered.json()).resolves.toEqual({
      total: 1,
      items: [
        expect.objectContaining({
          actor: "alice",
          action: "asset.delete",
          repo: "maven-releases",
        }),
      ],
    });

    const paged = await fetch("http://localhost/api/v1/audit-logs?limit=50&offset=0", {
      headers: auth,
    });
    expect(paged.status).toBe(200);
    const page = (await paged.json()) as { items: unknown[]; total: number };
    expect(page.total).toBe(51);
    expect(page.items).toHaveLength(50);
  });

  it("用户 CRUD 仅管理员可用，普通用户与未知令牌均不能读取", async () => {
    expect((await fetch("http://localhost/api/v1/users", { headers: userAuth })).status).toBe(403);
    expect(
      (
        await fetch("http://localhost/api/v1/users", {
          headers: { Authorization: "Bearer unknown-token" },
        })
      ).status,
    ).toBe(401);
  });

  it("登录成功返回会话令牌", async () => {
    const res = await fetch("http://localhost/api/v1/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: "admin", password: "whatever" }),
    });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { token: string };
    expect(body.token).toBe(MOCK_TOKEN);
  });

  it("创建用户后列表可见，重复用户名冲突 409", async () => {
    const create = await fetch("http://localhost/api/v1/users", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({ username: "alice", password: "password1" }),
    });
    expect(create.status).toBe(201);

    const dup = await fetch("http://localhost/api/v1/users", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({ username: "alice", password: "password1" }),
    });
    expect(dup.status).toBe(409);

    const list = await fetch("http://localhost/api/v1/users", { headers: auth });
    const body = (await list.json()) as { items: { username: string }[] };
    expect(body.items.some((u) => u.username === "alice")).toBe(true);
  });

  it("签发令牌返回一次性明文，列表不含明文", async () => {
    const created = await fetch("http://localhost/api/v1/tokens", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({ name: "deploy" }),
    });
    const body = (await created.json()) as { token: string };
    expect(body.token).toMatch(/^jat_/);

    const list = await fetch("http://localhost/api/v1/tokens", { headers: auth });
    const listBody = (await list.json()) as { items: Record<string, unknown>[] };
    expect(listBody.items.every((t) => !("token" in t))).toBe(true);
  });

  it("仓库 ACL 覆盖写入后可读回", async () => {
    const put = await fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({ items: [{ subjectId: 2, action: "write" }] }),
    });
    expect(put.status).toBe(200);

    const get = await fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
      headers: auth,
    });
    const body = (await get.json()) as { items: { action: string }[] };
    expect(body.items[0]?.action).toBe("write");
  });

  it("未知仓库 ACL 返回 404", async () => {
    const res = await fetch("http://localhost/api/v1/repositories/nope/acl", { headers: auth });
    expect(res.status).toBe(404);
  });

  it("仓库浏览缺失的搜索与清理端点均由 Mock 接住", async () => {
    const search = await fetch("http://localhost/api/v1/search?q=app&page=1&page_size=20", {
      headers: auth,
    });
    expect(search.status).toBe(200);
    await expect(search.json()).resolves.toEqual(
      expect.objectContaining({
        total: expect.any(Number),
        items: expect.arrayContaining([
          expect.objectContaining({
            repository: "maven-releases",
            path: expect.stringContaining("app"),
          }),
        ]),
        facets: expect.arrayContaining([
          expect.objectContaining({ repository: "maven-releases", count: expect.any(Number) }),
        ]),
      }),
    );

    const cleanup = await fetch("http://localhost/api/v1/repositories/maven-releases/cleanup", {
      method: "POST",
      headers: auth,
    });
    expect(cleanup.status).toBe(200);
    await expect(cleanup.json()).resolves.toEqual({ deleted: 0 });
  });

  it("仓库与公开浏览的空态仅返回同路由所需的空数据", async () => {
    const emptyHeaders = {
      ...auth,
      [DEV_MOCK_SCENARIO_HEADER]: "empty",
      [DEV_MOCK_ROUTE_HEADER]: "/repositories/maven-releases",
    };
    const repositories = await fetch("http://localhost/api/v1/repositories", {
      headers: emptyHeaders,
    });
    await expect(repositories.json()).resolves.toEqual({ items: [], total: 0 });

    const acl = await fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
      headers: emptyHeaders,
    });
    await expect(acl.json()).resolves.toEqual({ items: [] });

    const tree = await fetch("http://localhost/api/v1/repositories/maven-releases/tree", {
      headers: emptyHeaders,
    });
    await expect(tree.json()).resolves.toEqual({ directories: [], files: [] });

    const search = await fetch("http://localhost/api/v1/search?q=app", {
      headers: emptyHeaders,
    });
    await expect(search.json()).resolves.toEqual({ items: [], total: 0, facets: [] });

    const licenses = await fetch("http://localhost/api/v1/licenses", { headers: emptyHeaders });
    await expect(licenses.json()).resolves.toEqual(
      expect.objectContaining({ go: [], npm: [], generatedAt: expect.any(String) }),
    );
  });

  it("账户/服务/可观测性的空态仅返回同路由所需的空数据（FR-122 路由矩阵）", async () => {
    const emptyHeaders = {
      ...auth,
      [DEV_MOCK_SCENARIO_HEADER]: "empty",
      [DEV_MOCK_ROUTE_HEADER]: "/users",
    };
    const users = await fetch("http://localhost/api/v1/users", { headers: emptyHeaders });
    await expect(users.json()).resolves.toEqual({ items: [], total: 0 });

    const tokens = await fetch("http://localhost/api/v1/tokens", { headers: emptyHeaders });
    await expect(tokens.json()).resolves.toEqual({ items: [] });

    const publicRepositories = await fetch("http://localhost/api/v1/public/repositories", {
      headers: emptyHeaders,
    });
    await expect(publicRepositories.json()).resolves.toEqual({ items: [], total: 0 });

    const auditLogs = await fetch("http://localhost/api/v1/audit-logs", {
      headers: { ...emptyHeaders, [DEV_MOCK_ROUTE_HEADER]: "/audit-logs" },
    });
    await expect(auditLogs.json()).resolves.toEqual({ items: [], total: 0 });
  });

  it("Raw 协议上传和统一资产操作会更新 Mock 树，失败前不提交部分变更", async () => {
    const uploaded = await fetch("http://localhost/repository/raw-hosted/releases/demo.txt", {
      method: "PUT",
      headers: { ...auth, "Content-Type": "text/plain" },
      body: "demo",
    });
    expect(uploaded.status).toBe(200);

    const secondUpload = await fetch("http://localhost/repository/raw-hosted/releases/other.txt", {
      method: "PUT",
      headers: { ...auth, "Content-Type": "text/plain" },
      body: "other",
    });
    expect(secondUpload.status).toBe(200);

    const move = await fetch("http://localhost/api/v1/repositories/raw-hosted/assets/operations", {
      method: "POST",
      headers: { ...auth, "Content-Type": "application/json" },
      body: JSON.stringify({
        action: "move",
        targets: [
          { type: "raw_path", path: "releases/demo.txt" },
          { type: "raw_path", path: "releases/other.txt" },
        ],
        destinationPath: "archive",
        overrideReason: "整理发布制品",
      }),
    });
    expect(move.status).toBe(200);
    await expect(move.json()).resolves.toEqual(
      expect.objectContaining({ operationId: expect.any(String), affected: 2 }),
    );

    const failed = await fetch(
      "http://localhost/api/v1/repositories/raw-hosted/assets/operations",
      {
        method: "POST",
        headers: { ...auth, "Content-Type": "application/json" },
        body: JSON.stringify({
          action: "delete",
          targets: [
            { type: "raw_path", path: "archive/demo.txt" },
            { type: "raw_path", path: "missing.txt" },
          ],
          overrideReason: "验证原子删除",
        }),
      },
    );
    expect(failed.status).toBe(409);

    const assets = await fetch("http://localhost/api/v1/repositories/raw-hosted/assets", {
      headers: auth,
    });
    await expect(assets.json()).resolves.toEqual(
      expect.objectContaining({
        items: expect.arrayContaining([
          expect.objectContaining({ path: "archive/demo.txt" }),
          expect.objectContaining({ path: "archive/other.txt" }),
        ]),
      }),
    );
  });

  it("备用只读场景拒绝仓库、ACL 与资产写入，且不提交任何局部变更", async () => {
    const standbyHeaders = {
      ...auth,
      "Content-Type": "application/json",
      [DEV_MOCK_SCENARIO_HEADER]: "standby_read_only",
      [DEV_MOCK_ROUTE_HEADER]: "/repositories/maven-releases",
    };
    const beforeList = (await fetch("http://localhost/api/v1/repositories", {
      headers: auth,
    }).then((response) => response.json())) as { items: { name: string; description: string }[] };
    const before = beforeList.items.find((repository) => repository.name === "maven-releases")!;
    const aclBefore = (await fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
      headers: auth,
    }).then((response) => response.json())) as { items: unknown[] };

    const responses = await Promise.all([
      fetch("http://localhost/api/v1/repositories/maven-releases", {
        method: "PATCH",
        headers: standbyHeaders,
        body: JSON.stringify({ description: "不应写入" }),
      }),
      fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
        method: "PUT",
        headers: standbyHeaders,
        body: JSON.stringify({ items: [{ subjectId: 2, action: "write" }] }),
      }),
      fetch("http://localhost/api/v1/repositories/maven-releases/assets/operations", {
        method: "POST",
        headers: standbyHeaders,
        body: JSON.stringify({
          action: "delete",
          targets: [{ type: "maven_version", path: "com/example/app/1.0.0" }],
        }),
      }),
    ]);
    expect(responses.map((response) => response.status)).toEqual([503, 503, 503]);
    await expect(responses[0]!.json()).resolves.toEqual(
      expect.objectContaining({ error: expect.objectContaining({ code: "standby_read_only" }) }),
    );

    const afterList = (await fetch("http://localhost/api/v1/repositories", {
      headers: auth,
    }).then((response) => response.json())) as { items: { name: string; description: string }[] };
    const after = afterList.items.find((repository) => repository.name === "maven-releases")!;
    expect(after.description).toBe(before.description);

    const aclAfter = await fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
      headers: auth,
    }).then((response) => response.json());
    expect(aclAfter).toEqual(aclBefore);

    const assets = await fetch("http://localhost/api/v1/repositories/maven-releases/assets", {
      headers: auth,
    }).then(async (response) => response.json() as Promise<{ total: number }>);
    // 种子夹具：maven-releases 有 11 条可浏览制品样本（见 store.seed 的 assets）。
    expect(assets.total).toBe(11);
  });

  it("Maven 网页上传遵循仓库 write ACL，拒绝不改动制品", async () => {
    const form = () => {
      const data = new FormData();
      data.set("groupId", "com.example");
      data.set("artifactId", "restricted");
      data.set("version", "1.0.0");
      data.set("file", new File(["jar"], "restricted.jar", { type: "application/java-archive" }));
      return data;
    };
    const before = await fetch("http://localhost/api/v1/repositories/maven-releases/assets", {
      headers: auth,
    }).then((response) => response.json() as Promise<{ total: number }>);

    const denied = await fetch("http://localhost/api/v1/repositories/maven-releases/maven-upload", {
      method: "POST",
      headers: userAuth,
      body: form(),
    });
    expect(denied.status).toBe(403);

    await expect(
      fetch("http://localhost/api/v1/repositories/maven-releases/assets", { headers: auth }).then(
        (response) => response.json(),
      ),
    ).resolves.toEqual(expect.objectContaining({ total: before.total }));

    const grant = await fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({ items: [{ subjectId: 2, action: "write" }] }),
    });
    expect(grant.status).toBe(200);

    const allowed = await fetch(
      "http://localhost/api/v1/repositories/maven-releases/maven-upload",
      {
        method: "POST",
        headers: userAuth,
        body: form(),
      },
    );
    expect(allowed.status).toBe(201);
  });

  it("统一资产操作拒绝缺审计理由和非法枚举，且不改动制品", async () => {
    const paths = ["invalid/no-reason.txt", "invalid/bogus.txt", "invalid/type.txt"];
    for (const path of paths) {
      const uploaded = await fetch(`http://localhost/repository/raw-hosted/${path}`, {
        method: "PUT",
        headers: { "Content-Type": "text/plain", ...auth },
        body: path,
      });
      expect(uploaded.status).toBe(200);
    }

    const responses = await Promise.all([
      fetch("http://localhost/api/v1/repositories/raw-hosted/assets/operations", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...auth },
        body: JSON.stringify({
          action: "move",
          targets: [{ type: "raw_path", path: paths[0] }],
          destinationPath: "archive",
        }),
      }),
      fetch("http://localhost/api/v1/repositories/raw-hosted/assets/operations", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...auth },
        body: JSON.stringify({
          action: "unknown",
          targets: [{ type: "raw_path", path: paths[1] }],
          newPath: "archive/bogus.txt",
          overrideReason: "验证非法操作",
        }),
      }),
      fetch("http://localhost/api/v1/repositories/raw-hosted/assets/operations", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...auth },
        body: JSON.stringify({
          action: "delete",
          targets: [{ type: "unknown", path: paths[2] }],
          overrideReason: "验证非法目标",
        }),
      }),
    ]);
    expect(responses.map((response) => response.status)).toEqual([400, 400, 400]);

    const assets = await fetch("http://localhost/api/v1/repositories/raw-hosted/assets", {
      headers: auth,
    }).then((response) => response.json() as Promise<{ items: { path: string }[] }>);
    expect(assets.items.map((asset) => asset.path)).toEqual(expect.arrayContaining(paths));
  });

  it("制品浏览：分页与前缀过滤", async () => {
    const res = await fetch("http://localhost/api/v1/repositories/maven-releases/assets", {
      headers: auth,
    });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { items: { path: string }[]; total: number };
    expect(body.total).toBe(11);

    const filtered = await fetch(
      "http://localhost/api/v1/repositories/maven-releases/assets?prefix=com/example/app/1.0.0/app-1.0.0.jar",
      { headers: auth },
    );
    const fbody = (await filtered.json()) as { items: { path: string }[]; total: number };
    expect(fbody.total).toBe(1);
    expect(fbody.items[0]?.path).toContain(".jar");
  });

  it("制品浏览缺令牌 401、未知仓库 404", async () => {
    expect((await fetch("http://localhost/api/v1/repositories/maven-releases/assets")).status).toBe(
      401,
    );
    const nf = await fetch("http://localhost/api/v1/repositories/nope/assets", { headers: auth });
    expect(nf.status).toBe(404);
  });

  it("旧批量删除委托原子操作，任一目标缺失时不产生部分删除", async () => {
    const res = await fetch(
      "http://localhost/api/v1/repositories/maven-releases/assets/batch-delete",
      {
        method: "POST",
        headers: { "Content-Type": "application/json", ...auth },
        body: JSON.stringify({
          paths: ["com/example/app/1.0.0/app-1.0.0.jar", "no/such.txt"],
          overrideReason: "清理失效制品",
        }),
      },
    );
    expect(res.status).toBe(409);
    // 失败后原有制品仍保留，不能留下部分删除。
    const list = await fetch("http://localhost/api/v1/repositories/maven-releases/assets", {
      headers: auth,
    });
    const listBody = (await list.json()) as { items: { path: string }[]; total: number };
    expect(listBody.total).toBe(11);
    expect(listBody.items.some((a) => a.path.includes("app-1.0.0.jar"))).toBe(true);
  });

  it("批量删除：非管理员 403、缺令牌 401、空 paths 400、未知仓库 404", async () => {
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/maven-releases/assets/batch-delete", {
          method: "POST",
          headers: { "Content-Type": "application/json", ...userAuth },
          body: JSON.stringify({ paths: ["a.txt"], overrideReason: "权限测试" }),
        })
      ).status,
    ).toBe(403);
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/maven-releases/assets/batch-delete", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ paths: ["a.txt"], overrideReason: "权限测试" }),
        })
      ).status,
    ).toBe(401);
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/maven-releases/assets/batch-delete", {
          method: "POST",
          headers: { "Content-Type": "application/json", ...auth },
          body: JSON.stringify({ paths: [], overrideReason: "参数测试" }),
        })
      ).status,
    ).toBe(400);
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/nope/assets/batch-delete", {
          method: "POST",
          headers: { "Content-Type": "application/json", ...auth },
          body: JSON.stringify({ paths: ["a.txt"], overrideReason: "未知仓库" }),
        })
      ).status,
    ).toBe(404);
  });

  it("使用片段：据 format 返回接入命令", async () => {
    const res = await fetch("http://localhost/api/v1/repositories/npm-proxy/usage", {
      headers: auth,
    });
    expect(res.status).toBe(200);
    const body = (await res.json()) as {
      format: string;
      type: string;
      snippets: { code: string }[];
    };
    expect(body.format).toBe("npm");
    expect(body.snippets.length).toBeGreaterThan(0);
    expect(body.snippets.some((s) => s.code.includes("npm config set registry"))).toBe(true);
    // proxy 不可写：不应含 publish 片段。
    expect(body.snippets.some((s) => s.code.includes("npm publish"))).toBe(false);
  });

  it("仓库列表为 proxy 返回连接状态、hosted 不返回（FR-114）", async () => {
    const res = await fetch("http://localhost/api/v1/repositories?page_size=100", {
      headers: auth,
    });
    expect(res.status).toBe(200);
    const body = (await res.json()) as {
      items: { name: string; type: string; online?: boolean; connectionStatus?: unknown }[];
    };
    const npmProxy = body.items.find((r) => r.name === "npm-proxy");
    expect(npmProxy?.connectionStatus).toEqual(expect.objectContaining({ status: "AVAILABLE" }));
    const hosted = body.items.find((r) => r.name === "maven-releases");
    expect(hosted?.connectionStatus).toBeUndefined();
  });

  it("offline 仓库连接状态优先覆盖为 OFFLINE（MD-2）", async () => {
    const put = await fetch("http://localhost/api/v1/repositories/npm-proxy/online", {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({ online: false }),
    });
    expect(put.status).toBe(200);
    const off = (await put.json()) as { connectionStatus?: { status: string } };
    expect(off.connectionStatus?.status).toBe("OFFLINE");

    // 列表同样带 OFFLINE。
    const list = await fetch("http://localhost/api/v1/repositories?page_size=100", {
      headers: auth,
    });
    const listBody = (await list.json()) as {
      items: { name: string; connectionStatus?: { status: string } }[];
    };
    const npmProxy = listBody.items.find((r) => r.name === "npm-proxy");
    expect(npmProxy?.connectionStatus?.status).toBe("OFFLINE");
  });

  it("手动重测：仅管理员、仅 online proxy，重测后返回最新状态", async () => {
    // 未认证 → 401。
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/npm-proxy/recheck-connection", {
          method: "POST",
        })
      ).status,
    ).toBe(401);
    // 普通用户 → 403。
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/npm-proxy/recheck-connection", {
          method: "POST",
          headers: userAuth,
        })
      ).status,
    ).toBe(403);
    // hosted 仓库 → 400。
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/maven-releases/recheck-connection", {
          method: "POST",
          headers: auth,
        })
      ).status,
    ).toBe(400);
    // 未知仓库 → 404。
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/nope/recheck-connection", {
          method: "POST",
          headers: auth,
        })
      ).status,
    ).toBe(404);
    // 管理员重测 online proxy → 200 且 AVAILABLE。
    const ok = await fetch("http://localhost/api/v1/repositories/npm-proxy/recheck-connection", {
      method: "POST",
      headers: auth,
    });
    expect(ok.status).toBe(200);
    const body = (await ok.json()) as { status: string };
    expect(body.status).toBe("AVAILABLE");

    // offline 仓库 → 400。
    await fetch("http://localhost/api/v1/repositories/npm-proxy/online", {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({ online: false }),
    });
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/npm-proxy/recheck-connection", {
          method: "POST",
          headers: auth,
        })
      ).status,
    ).toBe(400);
  });

  it("用户 CRUD 仅管理员，改密仅允许普通用户修改本人且拒绝无副作用", async () => {
    const login = await fetch("http://localhost/api/v1/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: "developer", password: "password1" }),
    });
    await expect(login.json()).resolves.toEqual(
      expect.objectContaining({ token: "mock.jwt.token:user" }),
    );
    const before = await fetch("http://localhost/api/v1/users", { headers: auth }).then(
      (response) => response.json(),
    );

    const denied = await Promise.all([
      fetch("http://localhost/api/v1/users", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...userAuth },
        body: JSON.stringify({ username: "forbidden-user", password: "password1" }),
      }),
      fetch("http://localhost/api/v1/users/2", {
        method: "PATCH",
        headers: { "Content-Type": "application/json", ...userAuth },
        body: JSON.stringify({ status: "disabled" }),
      }),
      fetch("http://localhost/api/v1/users/2", { method: "DELETE", headers: userAuth }),
    ]);
    expect(denied.map((response) => response.status)).toEqual([403, 403, 403]);

    const after = await fetch("http://localhost/api/v1/users", { headers: auth }).then((response) =>
      response.json(),
    );
    expect(after).toEqual(before);

    expect(
      (
        await fetch("http://localhost/api/v1/users/2/password", {
          method: "POST",
          headers: { "Content-Type": "application/json", ...userAuth },
          body: JSON.stringify({ password: "new-password" }),
        })
      ).status,
    ).toBe(204);
    expect(
      (
        await fetch("http://localhost/api/v1/users/1/password", {
          method: "POST",
          headers: { "Content-Type": "application/json", ...userAuth },
          body: JSON.stringify({ password: "new-password" }),
        })
      ).status,
    ).toBe(403);
    expect(
      (
        await fetch("http://localhost/api/v1/users/2/password", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ password: "new-password" }),
        })
      ).status,
    ).toBe(401);
  });

  it("仓库 ACL 仅允许全局管理员或该仓库 admin 授权用户修改", async () => {
    const before = await fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
      headers: auth,
    }).then((response) => response.json());

    expect((await fetch("http://localhost/api/v1/repositories/maven-releases/acl")).status).toBe(
      401,
    );
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
          headers: userAuth,
        })
      ).status,
    ).toBe(403);
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
          method: "PUT",
          headers: { "Content-Type": "application/json", ...userAuth },
          body: JSON.stringify({ items: [{ subjectId: 2, action: "admin" }] }),
        })
      ).status,
    ).toBe(403);
    await expect(
      fetch("http://localhost/api/v1/repositories/maven-releases/acl", { headers: auth }).then(
        (response) => response.json(),
      ),
    ).resolves.toEqual(before);

    const granted = await fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({ items: [{ subjectId: 2, action: "admin" }] }),
    });
    expect(granted.status).toBe(200);
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
          headers: userAuth,
        })
      ).status,
    ).toBe(200);
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/maven-releases/acl", {
          method: "PUT",
          headers: { "Content-Type": "application/json", ...userAuth },
          body: JSON.stringify({ items: [{ subjectId: 2, action: "write" }] }),
        })
      ).status,
    ).toBe(200);
  });

  it("令牌仅对当前主体可见和吊销，跨主体操作不会改动令牌", async () => {
    expect((await fetch("http://localhost/api/v1/tokens", { headers: userAuth })).status).toBe(200);
    await expect(
      fetch("http://localhost/api/v1/tokens", { headers: userAuth }).then((response) =>
        response.json(),
      ),
    ).resolves.toEqual({ items: [] });
    expect((await fetch("http://localhost/api/v1/tokens")).status).toBe(401);

    const created = await fetch("http://localhost/api/v1/tokens", {
      method: "POST",
      headers: { "Content-Type": "application/json", ...userAuth },
      body: JSON.stringify({ name: "developer-token" }),
    });
    expect(created.status).toBe(201);
    const createdBody = (await created.json()) as { id: number };

    await expect(
      fetch("http://localhost/api/v1/tokens", { headers: userAuth }).then((response) =>
        response.json(),
      ),
    ).resolves.toEqual({
      items: [expect.objectContaining({ id: createdBody.id, name: "developer-token" })],
    });
    await expect(
      fetch("http://localhost/api/v1/tokens", { headers: auth }).then((response) =>
        response.json(),
      ),
    ).resolves.toEqual({
      // 管理员可见自己的令牌集合（种子含多条）；关键是"能看见自己那条"而非数量，
      // 数量会被后续种子扩充带偏，故用 arrayContaining 表达。
      items: expect.arrayContaining([expect.objectContaining({ id: 1, name: "ci" })]),
    });

    expect(
      (await fetch("http://localhost/api/v1/tokens/1", { method: "DELETE", headers: userAuth }))
        .status,
    ).toBe(404);
    await expect(
      fetch("http://localhost/api/v1/tokens", { headers: auth }).then((response) =>
        response.json(),
      ),
    ).resolves.toEqual({
      // 管理员可见自己的令牌集合（种子含多条）；关键是"能看见自己那条"而非数量，
      // 数量会被后续种子扩充带偏，故用 arrayContaining 表达。
      items: expect.arrayContaining([expect.objectContaining({ id: 1, name: "ci" })]),
    });
    expect(
      (
        await fetch(`http://localhost/api/v1/tokens/${createdBody.id}`, {
          method: "DELETE",
          headers: userAuth,
        })
      ).status,
    ).toBe(204);
  });

  it("Raw 上传按 write ACL，删除仍仅管理员，拒绝请求不改变制品", async () => {
    const path = "restricted/developer.txt";
    expect(
      (
        await fetch(`http://localhost/repository/raw-hosted/${path}`, {
          method: "PUT",
          headers: { "Content-Type": "text/plain" },
          body: "denied",
        })
      ).status,
    ).toBe(401);
    expect(
      (
        await fetch(`http://localhost/repository/raw-hosted/${path}`, {
          method: "PUT",
          headers: { "Content-Type": "text/plain", ...userAuth },
          body: "denied",
        })
      ).status,
    ).toBe(403);

    const grantWrite = await fetch("http://localhost/api/v1/repositories/raw-hosted/acl", {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...auth },
      body: JSON.stringify({ items: [{ subjectId: 2, action: "write" }] }),
    });
    expect(grantWrite.status).toBe(200);
    expect(
      (
        await fetch(`http://localhost/repository/raw-hosted/${path}`, {
          method: "PUT",
          headers: { "Content-Type": "text/plain", ...userAuth },
          body: "allowed",
        })
      ).status,
    ).toBe(200);
    expect(
      (
        await fetch(`http://localhost/repository/raw-hosted/${path}`, {
          method: "DELETE",
          headers: userAuth,
        })
      ).status,
    ).toBe(403);
    expect(
      (await fetch(`http://localhost/repository/raw-hosted/${path}`, { method: "DELETE" })).status,
    ).toBe(401);

    const assets = await fetch("http://localhost/api/v1/repositories/raw-hosted/assets", {
      headers: auth,
    }).then((response) => response.json() as Promise<{ items: { path: string }[] }>);
    expect(assets.items.some((asset) => asset.path === path)).toBe(true);
  });

  it("匿名仅能读取公开仓库的 usage 与搜索结果，不泄露私有仓库", async () => {
    expect((await fetch("http://localhost/api/v1/repositories/npm-proxy/usage")).status).toBe(200);
    expect((await fetch("http://localhost/api/v1/repositories/maven-releases/usage")).status).toBe(
      401,
    );

    const search = await fetch("http://localhost/api/v1/search?q=app");
    expect(search.status).toBe(200);
    await expect(search.json()).resolves.toEqual({ items: [], total: 0, facets: [] });
    expect(
      (await fetch("http://localhost/api/v1/search?q=app&repository=maven-releases")).status,
    ).toBe(401);
  });

  it("旧批量删除要求覆盖理由，成功时保持兼容响应", async () => {
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/maven-releases/assets/batch-delete", {
          method: "POST",
          headers: { "Content-Type": "application/json", ...auth },
          body: JSON.stringify({ paths: ["com/example/app/1.0.0/app-1.0.0.jar"] }),
        })
      ).status,
    ).toBe(400);

    const success = await fetch(
      "http://localhost/api/v1/repositories/maven-releases/assets/batch-delete",
      {
        method: "POST",
        headers: { "Content-Type": "application/json", ...auth },
        body: JSON.stringify({
          paths: ["com/example/app/1.0.0/app-1.0.0.jar"],
          overrideReason: "清理失效制品",
        }),
      },
    );
    expect(success.status).toBe(200);
    await expect(success.json()).resolves.toEqual({ deleted: 1, failed: [] });
  });

  it("普通用户只能看到可读仓库，仓库管理写入被拒绝且不改变状态", async () => {
    const before = await fetch("http://localhost/api/v1/repositories?page_size=100", {
      headers: auth,
    }).then((response) => response.json());

    const denied = await Promise.all([
      fetch("http://localhost/api/v1/repositories", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...userAuth },
        body: JSON.stringify({ name: "user-created", format: "raw", type: "hosted" }),
      }),
      fetch("http://localhost/api/v1/repositories/maven-releases", {
        method: "PATCH",
        headers: { "Content-Type": "application/json", ...userAuth },
        body: JSON.stringify({ description: "普通用户不得修改" }),
      }),
      fetch("http://localhost/api/v1/repositories/raw-hosted", {
        method: "DELETE",
        headers: userAuth,
      }),
    ]);
    expect(denied.map((response) => response.status)).toEqual([403, 403, 403]);

    const visible = (await fetch("http://localhost/api/v1/repositories?page_size=100", {
      headers: userAuth,
    }).then((response) => response.json())) as {
      items: { name: string; connectionStatus?: unknown }[];
    };
    expect(visible.items.map((repository) => repository.name)).toEqual([
      "maven-releases",
      "npm-proxy",
      "maven-papermc",
      "maven-central",
      "maven-airgame",
      "docker-hub",
      "pypi-mirror",
      "gomod-proxy",
    ]);
    expect(visible.items.every((repository) => repository.connectionStatus === undefined)).toBe(
      true,
    );

    await expect(
      fetch("http://localhost/api/v1/repositories?page_size=100", { headers: auth }).then(
        (response) => response.json(),
      ),
    ).resolves.toEqual(before);
  });

  it("私有仓库读取和全局管理端点均要求管理员，拒绝请求不改变设置或集群", async () => {
    const beforeSettings = await fetch("http://localhost/api/v1/settings", { headers: auth }).then(
      (response) => response.json(),
    );

    const denied = await Promise.all([
      fetch("http://localhost/api/v1/repositories/raw-hosted/assets", { headers: userAuth }),
      fetch("http://localhost/api/v1/repositories/raw-hosted/tree", { headers: userAuth }),
      fetch("http://localhost/api/v1/repositories/raw-hosted/usage", { headers: userAuth }),
      fetch("http://localhost/api/v1/licenses", { headers: userAuth }),
      fetch("http://localhost/api/v1/settings/anonymous-access", { headers: userAuth }),
      fetch("http://localhost/api/v1/settings/anonymous-access", {
        method: "PUT",
        headers: { "Content-Type": "application/json", ...userAuth },
        body: JSON.stringify({ enabled: false }),
      }),
      fetch("http://localhost/api/v1/settings", { headers: userAuth }),
      fetch("http://localhost/api/v1/settings", {
        method: "PUT",
        headers: { "Content-Type": "application/json", ...userAuth },
        body: JSON.stringify({ anonymousAccess: false, publicUrl: "", upstreamTimeout: 10 }),
      }),
    ]);
    expect(denied.map((response) => response.status)).toEqual(Array(denied.length).fill(403));

    await expect(
      fetch("http://localhost/api/v1/settings", { headers: auth }).then((response) =>
        response.json(),
      ),
    ).resolves.toEqual(beforeSettings);
    expect((await fetch("http://localhost/api/v1/repositories/raw-hosted/assets")).status).toBe(
      401,
    );
    expect((await fetch("http://localhost/api/v1/repositories/raw-hosted/tree")).status).toBe(401);
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/raw-hosted/acl", {
          method: "PUT",
          headers: { "Content-Type": "application/json", ...auth },
          body: JSON.stringify({ items: [{ subjectId: 2, action: "read" }] }),
        })
      ).status,
    ).toBe(200);
    const readable = await Promise.all([
      fetch("http://localhost/api/v1/repositories/raw-hosted/assets", { headers: userAuth }),
      fetch("http://localhost/api/v1/repositories/raw-hosted/tree", { headers: userAuth }),
      fetch("http://localhost/api/v1/repositories/raw-hosted/usage", { headers: userAuth }),
    ]);
    expect(readable.map((response) => response.status)).toEqual([200, 200, 200]);
  });

  it("递归资产操作按展开后的实际制品数限制为 500 条且不改动仓库", async () => {
    const maxAssets = 500;
    // 以当前种子数为基线（raw-hosted 也有可浏览样本），不把种子规模写死进断言。
    const baseline = await fetch(
      "http://localhost/api/v1/repositories/raw-hosted/assets?page_size=1",
      { headers: auth },
    )
      .then((result) => result.json() as Promise<{ total: number }>)
      .then((body) => body.total);
    for (let index = 0; index < maxAssets + 1; index += 1) {
      store.putRawAsset("raw-hosted", `overflow/${index}.txt`, 1, "text/plain");
    }

    const response = await fetch(
      "http://localhost/api/v1/repositories/raw-hosted/assets/operations",
      {
        method: "POST",
        headers: { "Content-Type": "application/json", ...auth },
        body: JSON.stringify({
          action: "delete",
          targets: [{ type: "raw_path", path: "overflow" }],
          overrideReason: "验证数量限制",
        }),
      },
    );
    expect(response.status).toBe(400);

    await expect(
      fetch("http://localhost/api/v1/repositories/raw-hosted/assets?page_size=600", {
        headers: auth,
      }).then((result) => result.json()),
    ).resolves.toEqual(expect.objectContaining({ total: baseline + maxAssets + 1 }));
  });
});
