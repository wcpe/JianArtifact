import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { server } from "../src/node";
import { resetDevMockScenario } from "../src/scenario";
import { MOCK_TOKEN, resetStore } from "../src/store";

const admin = { Authorization: `Bearer ${MOCK_TOKEN}` };

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  resetDevMockScenario();
  resetStore();
});
afterAll(() => server.close());

describe("业务与当前主机观测 Mock", () => {
  it("仅管理员读取真实页面所需的业务 KPI 与当前主机样本形状", async () => {
    for (const path of ["dashboard", "host"]) {
      const denied = await fetch(`http://localhost/api/v1/observability/${path}`);
      expect(denied.status).toBe(401);
      const response = await fetch(`http://localhost/api/v1/observability/${path}`, {
        headers: admin,
      });
      expect(response.status).toBe(200);
      const serialized = JSON.stringify(await response.json());
      expect(serialized).not.toContain("token");
      expect(serialized).not.toContain("peerUrl");
    }
  });
});
