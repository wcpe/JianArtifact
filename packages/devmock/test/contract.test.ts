// 契约一致性测试（对齐 AC-06 / AC-0.1.0-3）：
// 用 ajv 以 api/openapi.yaml 为唯一真源校验 mock 处理器输出；
// 任何 mock 偏离契约（漂移）都会使断言失败。
import Ajv from "ajv";
import { describe, expect, it } from "vitest";

import { mockHealthz, mockReadyz, mockUnavailable } from "../src/handlers";
import { schemaFor } from "../src/openapi";

const ajv = new Ajv({ strict: false, allErrors: true });

describe("devmock ↔ OpenAPI 契约一致性", () => {
  it("mockHealthz 满足 HealthStatus 契约", () => {
    const validate = ajv.compile(schemaFor("HealthStatus"));
    expect(validate(mockHealthz())).toBe(true);
  });

  it("mockReadyz 满足 HealthStatus 契约", () => {
    const validate = ajv.compile(schemaFor("HealthStatus"));
    expect(validate(mockReadyz())).toBe(true);
  });

  it("mockUnavailable 满足 Error 契约", () => {
    const validate = ajv.compile(schemaFor("Error"));
    expect(validate(mockUnavailable())).toBe(true);
  });

  it("契约漂移可被检出：缺 required 字段或非法枚举的响应校验失败", () => {
    const validate = ajv.compile(schemaFor("HealthStatus"));
    // 故意构造漂移响应：缺 version（required）且 status 非契约枚举
    expect(validate({ status: "healthy" })).toBe(false);
  });
});
