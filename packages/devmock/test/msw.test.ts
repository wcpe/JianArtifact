// MSW 双端拦截层测试：经 Node server 拦截全局 fetch，校验 0.2.0 端点的契约行为。
// 与 contract.test.ts（纯响应函数 ↔ 契约）互补：此处覆盖状态流转、鉴权与错误码。
import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { server } from "../src/node";
import { MOCK_TOKEN, resetStore } from "../src/store";

const auth = { Authorization: `Bearer ${MOCK_TOKEN}` };

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  resetStore();
});
afterAll(() => server.close());

describe("devmock MSW 端点行为", () => {
  it("公开状态端点无需鉴权", async () => {
    const res = await fetch("http://localhost/api/v1/status");
    expect(res.status).toBe(200);
    const body = (await res.json()) as { initialized: boolean; userCount: number };
    expect(body.initialized).toBe(true);
    expect(body.userCount).toBeGreaterThan(0);
  });

  it("受保护端点缺令牌返回 401", async () => {
    const res = await fetch("http://localhost/api/v1/users");
    expect(res.status).toBe(401);
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

    const get = await fetch("http://localhost/api/v1/repositories/maven-releases/acl", { headers: auth });
    const body = (await get.json()) as { items: { action: string }[] };
    expect(body.items[0]?.action).toBe("write");
  });

  it("未知仓库 ACL 返回 404", async () => {
    const res = await fetch("http://localhost/api/v1/repositories/nope/acl", { headers: auth });
    expect(res.status).toBe(404);
  });
});
