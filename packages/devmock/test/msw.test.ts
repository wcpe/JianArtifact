// MSW 双端拦截层测试：经 Node server 拦截全局 fetch，校验 0.2.0 端点的契约行为。
// 与 contract.test.ts（纯响应函数 ↔ 契约）互补：此处覆盖状态流转、鉴权与错误码。
import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { server } from "../src/node";
import { MOCK_TOKEN, resetStore } from "../src/store";

const auth = { Authorization: `Bearer ${MOCK_TOKEN}` };
const userAuth = { Authorization: "Bearer mock.jwt.token:user" };

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

  it("复制应用日志缺令牌返回 401", async () => {
    const res = await fetch("http://localhost/api/v1/replication-apply-logs");
    expect(res.status).toBe(401);
  });

  it("复制应用日志管理员鉴权区分 401 与 403", async () => {
    expect((await fetch("http://localhost/api/v1/replication-apply-logs")).status).toBe(401);
    expect(
      (
        await fetch("http://localhost/api/v1/replication-apply-logs", {
          headers: { Authorization: "Bearer " },
        })
      ).status,
    ).toBe(401);
    expect(
      (
        await fetch("http://localhost/api/v1/replication-apply-logs", {
          headers: { Authorization: "Bearer unknown-token" },
        })
      ).status,
    ).toBe(401);
    expect(
      (await fetch("http://localhost/api/v1/replication-apply-logs", { headers: userAuth })).status,
    ).toBe(403);
    expect(
      (await fetch("http://localhost/api/v1/replication-apply-logs", { headers: auth })).status,
    ).toBe(200);
  });

  it("普通受保护端点接受普通用户令牌并拒绝未知令牌", async () => {
    expect((await fetch("http://localhost/api/v1/users", { headers: userAuth })).status).toBe(200);
    expect(
      (
        await fetch("http://localhost/api/v1/users", {
          headers: { Authorization: "Bearer unknown-token" },
        })
      ).status,
    ).toBe(401);
  });

  it("复制应用日志支持契约筛选参数", async () => {
    const filters: [string, string][] = [
      ["sourceNode", "node-b"],
      ["sourceSeq", "2"],
      ["peerURL", "https://other.example"],
      ["entityType", "repository"],
      ["entityKey", "asset:other"],
      ["op", "delete"],
      ["result", "failed"],
    ];
    for (const [key, value] of filters) {
      const res = await fetch(
        `http://localhost/api/v1/replication-apply-logs?${key}=${encodeURIComponent(value)}`,
        { headers: auth },
      );
      expect(res.status).toBe(200);
      const body = (await res.json()) as { items: unknown[]; total: number };
      expect(body.total, `${key} 应参与筛选`).toBe(0);
      expect(body.items).toHaveLength(0);
    }

    const res = await fetch(
      "http://localhost/api/v1/replication-apply-logs?sourceNode=node-a&sourceSeq=1&peerURL=https%3A%2F%2Fpeer.example&entityType=asset&entityKey=asset%3Araw%2Fa.txt&op=put&result=applied",
      { headers: auth },
    );
    expect(res.status).toBe(200);
    const body = (await res.json()) as {
      items: { sourceSeq: number; peerUrl: string }[];
      total: number;
    };
    expect(body.total).toBe(1);
    expect(body.items[0]).toEqual(
      expect.objectContaining({ sourceSeq: 1, peerUrl: "https://peer.example" }),
    );
  });

  it("复制应用日志非法整数查询参数返回 400", async () => {
    const invalidParams: [string, string][] = [
      ["limit", "1.5"],
      ["limit", ""],
      ["offset", "1.5"],
      ["offset", ""],
      ["sourceSeq", "-1"],
      ["sourceSeq", "1.5"],
      ["sourceSeq", ""],
      ["sourceSeq", "not-a-number"],
    ];
    for (const [key, value] of invalidParams) {
      const res = await fetch(
        `http://localhost/api/v1/replication-apply-logs?${key}=${encodeURIComponent(value)}`,
        { headers: auth },
      );
      expect(res.status, `${key}=${value} 应返回 400`).toBe(400);
    }
  });

  it("复制应用日志分页参数按后端默认值与范围钳制", async () => {
    for (const query of ["limit=0", "limit=-1", "limit=201", "offset=-1"]) {
      const res = await fetch(`http://localhost/api/v1/replication-apply-logs?${query}`, {
        headers: auth,
      });
      expect(res.status).toBe(200);
      const body = (await res.json()) as { items: unknown[]; total: number };
      expect(body.total).toBe(1);
      expect(body.items).toHaveLength(1);
    }
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

  it("制品浏览：分页与前缀过滤", async () => {
    const res = await fetch("http://localhost/api/v1/repositories/maven-releases/assets", {
      headers: auth,
    });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { items: { path: string }[]; total: number };
    expect(body.total).toBe(2);

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

  it("批量删除：管理员逐条删除并返回失败明细", async () => {
    const res = await fetch(
      "http://localhost/api/v1/repositories/maven-releases/assets/batch-delete",
      {
        method: "POST",
        headers: { "Content-Type": "application/json", ...auth },
        body: JSON.stringify({ paths: ["com/example/app/1.0.0/app-1.0.0.jar", "no/such.txt"] }),
      },
    );
    expect(res.status).toBe(200);
    const body = (await res.json()) as { deleted: number; failed: { path: string }[] };
    expect(body.deleted).toBe(1);
    expect(body.failed).toHaveLength(1);
    expect(body.failed[0]?.path).toBe("no/such.txt");
    // 已删除的路径不再出现在制品列表中。
    const list = await fetch("http://localhost/api/v1/repositories/maven-releases/assets", {
      headers: auth,
    });
    const listBody = (await list.json()) as { items: { path: string }[]; total: number };
    expect(listBody.total).toBe(1);
    expect(listBody.items.some((a) => a.path.includes("app-1.0.0.jar"))).toBe(false);
  });

  it("批量删除：非管理员 403、缺令牌 401、空 paths 400、未知仓库 404", async () => {
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/maven-releases/assets/batch-delete", {
          method: "POST",
          headers: { "Content-Type": "application/json", ...userAuth },
          body: JSON.stringify({ paths: ["a.txt"] }),
        })
      ).status,
    ).toBe(403);
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/maven-releases/assets/batch-delete", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ paths: ["a.txt"] }),
        })
      ).status,
    ).toBe(401);
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/maven-releases/assets/batch-delete", {
          method: "POST",
          headers: { "Content-Type": "application/json", ...auth },
          body: JSON.stringify({ paths: [] }),
        })
      ).status,
    ).toBe(400);
    expect(
      (
        await fetch("http://localhost/api/v1/repositories/nope/assets/batch-delete", {
          method: "POST",
          headers: { "Content-Type": "application/json", ...auth },
          body: JSON.stringify({ paths: ["a.txt"] }),
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
});
