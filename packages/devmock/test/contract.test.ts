// 契约一致性测试（对齐 AC-06 / AC-0.1.0-3）：
// 用 ajv 以 api/openapi.yaml 为唯一真源校验 mock 处理器输出；
// 任何 mock 偏离契约（漂移）都会使断言失败。
import Ajv from "ajv";
import { describe, expect, it } from "vitest";

import {
  mockAclList,
  mockHealthz,
  mockLoginResponse,
  mockReadyz,
  mockRepository,
  mockRepositoryList,
  mockStatus,
  mockTokenCreated,
  mockTokenList,
  mockUnavailable,
  mockUser,
  mockUserList,
} from "../src/handlers";
import { allSchemas, schemaFor } from "../src/openapi";

const ajv = new Ajv({ strict: false, allErrors: true });

// 预注册全部组件 schema，令交叉 $ref（如 UserList → User）可被 ajv 解析。
for (const [id, schema] of Object.entries(allSchemas())) {
  if (!ajv.getSchema(id)) {
    ajv.addSchema(schema, id);
  }
}

// 用契约中的组件 schema 校验一个 mock 值；漂移即失败。
function expectValid(schemaName: string, value: unknown): void {
  const validate = ajv.compile(schemaFor(schemaName));
  const ok = validate(value);
  if (!ok) {
    throw new Error(`${schemaName} 校验失败：${ajv.errorsText(validate.errors)}`);
  }
  expect(ok).toBe(true);
}

describe("devmock ↔ OpenAPI 契约一致性", () => {
  it("健康 / 状态类响应满足契约", () => {
    expectValid("HealthStatus", mockHealthz());
    expectValid("HealthStatus", mockReadyz());
    expectValid("Error", mockUnavailable());
    expectValid("StatusInfo", mockStatus());
  });

  it("认证 / 用户类响应满足契约", () => {
    expectValid("LoginResponse", mockLoginResponse());
    expectValid("User", mockUser());
    expectValid("UserList", mockUserList());
  });

  it("令牌类响应满足契约", () => {
    expectValid("TokenList", mockTokenList());
    expectValid("TokenCreated", mockTokenCreated());
  });

  it("仓库 / ACL 类响应满足契约", () => {
    expectValid("Repository", mockRepository());
    expectValid("RepositoryList", mockRepositoryList());
    expectValid("AclList", mockAclList());
  });

  it("契约漂移可被检出：缺 required 字段或非法枚举的响应校验失败", () => {
    const validate = ajv.compile(schemaFor("HealthStatus"));
    // 故意构造漂移响应：缺 version（required）且 status 非契约枚举
    expect(validate({ status: "healthy" })).toBe(false);

    // 错误信封漂移：扁平 {code,message} 不再满足嵌套 Error 契约
    const validateErr = ajv.compile(schemaFor("Error"));
    expect(validateErr({ code: "x", message: "y" })).toBe(false);
  });
});
