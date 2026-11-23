import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { server } from "../src/node";
import { DEV_MOCK_ROUTE_HEADER, DEV_MOCK_SCENARIO_HEADER } from "../src/scenario";
import { MOCK_TOKEN, MOCK_SECOND_ADMIN_TOKEN, resetStore, store } from "../src/store";

const base = "http://localhost/api/v1/observability/audit";
const range = "from=2026-01-01T00%3A00%3A00Z&to=2026-01-02T00%3A00%3A00Z";
const auth = (token: string) => ({ Authorization: `Bearer ${token}` });
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterAll(() => server.close());

describe("安全审计管理查询", () => {
  it("所有接口先判断身份与角色", async () => {
    for (const path of [
      "security-events?broken=1",
      "security-events/missing?bad=1",
      "security-events/missing/objects?bad=1",
      "collection-health?bad=1",
    ]) {
      for (const [token, status] of [
        ["", 401],
        ["unknown", 401],
        ["mock.jwt.token:user", 403],
      ] as const) {
        const response = await fetch(`${base}/${path}`, { headers: token ? auth(token) : {} });
        expect(response.status).toBe(status);
      }
    }
  });
  it("账号状态、交互资格和已知凭据决定状态与业务码", async () => {
    try {
      const machine = store.createToken(1, "身份决策测试");
      const basic = `Basic ${btoa(`admin:${machine.token}`)}`;
      for (const [authorization, status, code] of [
        ["Bearer unknown", 401, "unauthenticated"],
        ["Bearer mock.jwt.token:user", 403, "forbidden"],
        [`Bearer ${machine.token}`, 403, "forbidden"],
        [basic, 403, "forbidden"],
      ] as const) {
        for (const path of [
          "security-events?bad=1",
          "security-events/missing?bad=1",
          "security-events/missing/objects?bad=1",
          "collection-health?bad=1",
        ]) {
          const response = await fetch(`${base}/${path}`, {
            headers: { Authorization: authorization },
          });
          expect(response.status).toBe(status);
          expect((await response.json()).error.code).toBe(code);
        }
      }
      expect(
        (
          await fetch(`${base}/collection-health`, {
            headers: { Authorization: `bEaReR  ${MOCK_TOKEN} ` },
          })
        ).status,
      ).toBe(200);
      store.updateUser(1, { role: "user" });
      const demoted = await fetch(`${base}/collection-health?bad=1`, { headers: auth(MOCK_TOKEN) });
      expect(demoted.status).toBe(403);
      expect((await demoted.json()).error.code).toBe("forbidden");
      store.updateUser(1, { role: "admin", webLoginDisabled: true });
      for (const path of [
        "security-events?bad=1",
        "security-events/missing?bad=1",
        "security-events/missing/objects?bad=1",
        "collection-health?bad=1",
      ]) {
        const response = await fetch(`${base}/${path}`, { headers: auth(MOCK_TOKEN) });
        expect(response.status).toBe(403);
        expect((await response.json()).error.code).toBe("forbidden");
      }
      store.updateUser(1, { status: "disabled" });
      for (const path of [
        "security-events?bad=1",
        "security-events/missing?bad=1",
        "security-events/missing/objects?bad=1",
        "collection-health?bad=1",
      ]) {
        const response = await fetch(`${base}/${path}`, { headers: auth(MOCK_TOKEN) });
        expect(response.status).toBe(401);
        expect((await response.json()).error.code).toBe("unauthenticated");
      }
      const response = await fetch(`${base}/collection-health`, { headers: auth(machine.token) });
      expect(response.status).toBe(401);
      expect((await response.json()).error.code).toBe("unauthenticated");
    } finally {
      resetStore();
    }
  });
  it("时间按合法时差和无损毫秒解释", async () => {
    for (const from of ["2026-01-01T08:00:00+08:00", "2026-01-01T00:00:00.12300000000Z"]) {
      const query = new URLSearchParams({ from, to: "2026-01-02T00:00:00Z" });
      expect(
        (await fetch(`${base}/security-events?${query}`, { headers: auth(MOCK_TOKEN) })).status,
      ).toBe(200);
    }
    for (const [from, to] of [
      ["2026-01-01T00:00:00.12300000001Z", "2026-01-02T00:00:00Z"],
      ["2026-01-01T00:00:00Z", "2026-01-02T00:00:00.0001Z"],
    ] as const) {
      const query = new URLSearchParams({ from, to });
      expect(
        (await fetch(`${base}/security-events?${query}`, { headers: auth(MOCK_TOKEN) })).status,
      ).toBe(400);
    }
    for (const from of [
      "2026-01-01T00:00:00.12300000001Z",
      "2026-02-30T00:00:00Z",
      "2026-01-01T00:00:00+24:00",
    ]) {
      const query = new URLSearchParams({ from, to: "2026-03-01T00:00:00Z" });
      expect(
        (await fetch(`${base}/security-events?${query}`, { headers: auth(MOCK_TOKEN) })).status,
      ).toBe(400);
    }
  });
  it("原始查询与映射网段使用严格语义", async () => {
    for (const query of [
      `${range}&&limit=1`,
      `${range}&`,
      `${range}&requestId=%FF`,
      `${range}&requestId=%`,
      `${range}&requestId=%00`,
      `${range}&cidr=%3A%3Affff%3Ac000%3A201%2F95`,
      `${range}&cidr=%3A%3Affff%3A192.0.2.1%2F95`,
    ]) {
      const response = await fetch(`${base}/security-events?${query}`, {
        headers: auth(MOCK_TOKEN),
      });
      expect(response.status).toBe(400);
      expect((await response.json()).error.code).toBe("invalid_security_filter");
    }
    for (const path of [
      "security-events/missing",
      "security-events/missing/objects",
      "collection-health",
    ]) {
      for (const query of ["&&", "bad=%FF", `bad=${"a".repeat(16400)}`]) {
        const response = await fetch(`${base}/${path}?${query}`, { headers: auth(MOCK_TOKEN) });
        expect(response.status).toBe(400);
        expect((await response.json()).error.code).toBe("invalid_security_filter");
      }
    }
    for (const cidr of ["::ffff:c000:201/96", "::ffff:192.0.2.1/128"]) {
      const query = `${range}&cidr=${encodeURIComponent(cidr)}`;
      expect(
        (await fetch(`${base}/security-events?${query}`, { headers: auth(MOCK_TOKEN) })).status,
      ).toBe(200);
    }
  });
  it("Basic 两个槽位遵循凭据选择顺序", async () => {
    const token = store.createToken(1, "双槽测试");
    try {
      for (const [user, pass, status, code] of [
        [token.token, "", 403, "forbidden"],
        ["", token.token, 403, "forbidden"],
        ["jat_unknown", token.token, 403, "forbidden"],
        [token.token, "jat_unknown", 401, "unauthenticated"],
        [token.token, "unknown", 401, "unauthenticated"],
        ["", "unknown", 401, "unauthenticated"],
        ["", "", 401, "unauthenticated"],
      ] as const) {
        const authorization = `Basic ${btoa(`${user}:${pass}`)}`;
        for (const path of [
          "security-events?bad=1",
          "security-events/missing?bad=1",
          "security-events/missing/objects?bad=1",
          "collection-health?bad=1",
        ]) {
          const response = await fetch(`${base}/${path}`, {
            headers: { Authorization: authorization },
          });
          expect(response.status).toBe(status);
          expect((await response.json()).error.code).toBe(code);
        }
      }
    } finally {
      store.deleteToken(token.id, 1);
    }
  });
  it("普通 IPv6 与真正 IPv4 映射地址按二进制分类", async () => {
    for (const [cidr, status] of [
      ["2001:db8::/32", 200],
      ["::ffff:0:0:0/32", 200],
      ["0:0:0:0:0:ffff:c000:201/95", 400],
      ["::ffff:192.0.2.1/95", 400],
      ["::ffff:192.0.2.1/96", 200],
    ] as const) {
      const response = await fetch(
        `${base}/security-events?${range}&cidr=${encodeURIComponent(cidr)}`,
        { headers: auth(MOCK_TOKEN) },
      );
      expect(response.status).toBe(status);
    }
  });
  it("网络前缀位数字必须为规范十进制", async () => {
    for (const [cidr, status] of [
      ["192.0.2.0/0", 200],
      ["192.0.2.0/00", 400],
      ["192.0.2.0/024", 400],
      ["2001:db8::/032", 400],
    ] as const) {
      const response = await fetch(
        `${base}/security-events?${range}&cidr=${encodeURIComponent(cidr)}`,
        { headers: auth(MOCK_TOKEN) },
      );
      expect(response.status).toBe(status);
    }
  });
  it("通用故障场景不能覆盖安全路由的鉴权顺序", async () => {
    const response = await fetch(`${base}/collection-health?bad=1`, {
      headers: { [DEV_MOCK_ROUTE_HEADER]: "/security", [DEV_MOCK_SCENARIO_HEADER]: "error" },
    });
    expect(response.status).toBe(401);
  });
  it("已签发机器凭据不能冒充交互管理员，撤销后为未认证", async () => {
    const token = store.createToken(1, "安全查询测试");
    for (const path of [
      "security-events?broken=1",
      "security-events/missing",
      "security-events/missing/objects",
      "collection-health",
    ]) {
      expect((await fetch(`${base}/${path}`, { headers: auth(token.token) })).status).toBe(403);
    }
    store.deleteToken(token.id, 1);
    expect((await fetch(`${base}/collection-health`, { headers: auth(token.token) })).status).toBe(
      401,
    );
  });
  it("管理员得到未知覆盖的空快照和健康视图", async () => {
    for (const token of [MOCK_TOKEN, MOCK_SECOND_ADMIN_TOKEN]) {
      const response = await fetch(`${base}/security-events?${range}`, { headers: auth(token) });
      expect(response.status).toBe(200);
      expect(await response.json()).toEqual({
        items: [],
        hasMore: false,
        snapshotWatermark: 0,
        projectionWatermark: 0,
        projectionHealth: "healthy",
        coverage: "unknown",
      });
      const health = await fetch(`${base}/collection-health`, { headers: auth(token) });
      expect(await health.json()).toEqual({ state: "starting", coverage: "unknown" });
    }
  });
  it("改密事件可按类型和账号目标筛选，模拟库不编造已提交事件", async () => {
    const response = await fetch(
      `${base}/security-events?${range}&eventType=auth.password_changed&objectId=2`,
      {
        headers: auth(MOCK_TOKEN),
      },
    );
    expect(response.status).toBe(200);
    expect(await response.json()).toMatchObject({ items: [], hasMore: false, coverage: "unknown" });
  });
  it("候选与资格收窄模拟先验证通道且不编造候选", async () => {
    const candidate =
      "http://localhost/api/v1/security/admission/candidates/f4f6df7b-a5d9-4f8a-9e90-8e92237cb582";
    const machine = store.createToken(1, "候选查询测试");
    try {
      for (const [headers, status, code] of [
        [{ [DEV_MOCK_SCENARIO_HEADER]: "error" }, 401, "unauthenticated"],
        [
          { ...auth(machine.token), [DEV_MOCK_SCENARIO_HEADER]: "error" },
          403,
          "admission_forbidden",
        ],
        [auth(MOCK_TOKEN), 404, "admission_not_found"],
        [auth(MOCK_SECOND_ADMIN_TOKEN), 404, "admission_not_found"],
        [
          { ...auth(MOCK_TOKEN), [DEV_MOCK_SCENARIO_HEADER]: "error" },
          503,
          "admission_unavailable",
        ],
      ] as const) {
        const response = await fetch(candidate, { headers });
        expect(response.status).toBe(status);
        expect(await response.json()).toEqual({ error: { code, message: expect.any(String) } });
      }
    } finally {
      store.deleteToken(machine.id, 1);
    }
  });

  it("候选与资格收窄模拟接受新增的事件类型筛选", async () => {
    for (const type of ["auth.account_access_changed", "admission.candidate_staged"]) {
      const response = await fetch(`${base}/security-events?${range}&eventType=${type}`, {
        headers: auth(MOCK_TOKEN),
      });
      expect(response.status).toBe(200);
      expect(await response.json()).toMatchObject({ items: [], coverage: "unknown" });
    }
  });

  it("筛选边界与无下一页语义", async () => {
    for (const query of [
      `${range}&limit=100&ip=192.0.2.1`,
      `${range}&cidr=2001%3Adb8%3A%3A%2F32&eventType=auth.attempt&outcome=denied`,
      `${range}&canonicalPath=org%2Fartifact&actorUserId=1`,
    ]) {
      const response = await fetch(`${base}/security-events?${query}`, {
        headers: auth(MOCK_TOKEN),
      });
      expect(response.status).toBe(200);
      const body = await response.json();
      expect(body.hasMore).toBe(false);
      expect(body).not.toHaveProperty("nextCursor");
    }
    for (const query of [
      `${range}&ip=192.0.2.1&cidr=192.0.2.0%2F24`,
      `${range}&cidr=192.0.2.0%2F33`,
      `${range}&canonicalPath=..%2Fsecret`,
      `${range}&digest=sha256%3Abad`,
      `${range}&from=2026-01-01T00%3A00%3A00Z`,
      `from=2026-01-01T00%3A00%3A00Z&to=2026-02-01T00%3A00%3A00Z`,
    ]) {
      expect(
        (await fetch(`${base}/security-events?${query}`, { headers: auth(MOCK_TOKEN) })).status,
      ).toBe(400);
    }
    expect(
      (
        await fetch(`${base}/security-events/missing/objects?limit=101`, {
          headers: auth(MOCK_TOKEN),
        })
      ).status,
    ).toBe(400);
    expect(
      (await fetch(`${base}/security-events/missing?cursor=fake`, { headers: auth(MOCK_TOKEN) }))
        .status,
    ).toBe(400);
    const health = await fetch("http://localhost/healthz");
    expect((await health.json()).auditCollection).toBe("unknown");
  });
  it("非法筛选和伪造游标为 400，未知事件为 404", async () => {
    for (const query of [
      "",
      `${range}&limit=0`,
      `${range}&limit=2&limit=3`,
      `${range}&cursor=`,
      `${range}&cursor=fake`,
      `${range}&offset=1`,
      `${range}&ip=not-ip`,
      `${range}&eventType=bogus`,
      `${range}&actorUserId=-1`,
    ]) {
      const response = await fetch(`${base}/security-events?${query}`, {
        headers: auth(MOCK_TOKEN),
      });
      expect(response.status).toBe(400);
      expect((await response.json()).error.code).toBe("invalid_security_filter");
    }
    for (const path of ["security-events/missing", "security-events/missing/objects"]) {
      const response = await fetch(`${base}/${path}`, { headers: auth(MOCK_TOKEN) });
      expect(response.status).toBe(404);
      expect((await response.json()).error.code).toBe("security_event_not_found");
    }
    expect(
      (await fetch(`${base}/collection-health?bad=1`, { headers: auth(MOCK_TOKEN) })).status,
    ).toBe(400);
  });
});
