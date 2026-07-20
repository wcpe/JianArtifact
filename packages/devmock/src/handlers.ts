// Mock 处理器：产出符合契约的响应体。类型绑定到 openapi-typescript 生成的
// schema.gen.ts（编译期），运行期再由 contract 测试用 ajv 对同一契约做校验。
import type { components } from "./schema.gen";

type Schemas = components["schemas"];

export type HealthStatus = Schemas["HealthStatus"];
export type ApiError = Schemas["Error"];
export type StatusInfo = Schemas["StatusInfo"];
export type LoginResponse = Schemas["LoginResponse"];
export type User = Schemas["User"];
export type UserList = Schemas["UserList"];
export type Token = Schemas["Token"];
export type TokenList = Schemas["TokenList"];
export type TokenCreated = Schemas["TokenCreated"];
export type Repository = Schemas["Repository"];
export type RepositoryList = Schemas["RepositoryList"];
export type AclList = Schemas["AclList"];

const MOCK_VERSION = "0.2.0-mock";
const MOCK_TIME = "2026-01-01T00:00:00Z";

/** GET /healthz 的契约响应。 */
export function mockHealthz(version = MOCK_VERSION): HealthStatus {
  return { status: "ok", version };
}

/** GET /readyz 就绪时的契约响应。 */
export function mockReadyz(version = MOCK_VERSION): HealthStatus {
  return { status: "ok", version };
}

/** GET /readyz 未就绪（503）及其余错误的契约信封 `{error:{code,message}}`。 */
export function mockUnavailable(): ApiError {
  return { error: { code: "dependency_unavailable", message: "依赖未就绪" } };
}

/** 通用错误信封构造器（供各错误码复用）。 */
export function mockError(code: string, message: string): ApiError {
  return { error: { code, message } };
}

/** GET /api/v1/status 的契约响应。 */
export function mockStatus(version = MOCK_VERSION): StatusInfo {
  return {
    version,
    ready: true,
    initialized: true,
    migrationVersion: "0001_init",
    userCount: 1,
  };
}

/** 单个用户的契约响应。 */
export function mockUser(): User {
  return {
    id: 1,
    username: "admin",
    role: "admin",
    status: "active",
    createdAt: MOCK_TIME,
  };
}

/** POST /auth/{bootstrap,login} 的契约响应。 */
export function mockLoginResponse(): LoginResponse {
  return { token: "mock.jwt.token", user: mockUser() };
}

/** GET /api/v1/users 的契约响应。 */
export function mockUserList(): UserList {
  return { items: [mockUser()], total: 1 };
}

/** GET /api/v1/tokens 的契约响应。 */
export function mockTokenList(): TokenList {
  return { items: [{ id: 1, name: "ci", createdAt: MOCK_TIME }] };
}

/** POST /api/v1/tokens 的契约响应（含一次性明文）。 */
export function mockTokenCreated(): TokenCreated {
  return { id: 1, name: "ci", token: "jat_mockplaintext", createdAt: MOCK_TIME };
}

/** 单个仓库的契约响应。 */
export function mockRepository(): Repository {
  return {
    id: 1,
    name: "maven-releases",
    format: "maven",
    type: "hosted",
    visibility: "private",
    createdAt: MOCK_TIME,
  };
}

/** GET /api/v1/repositories 的契约响应。 */
export function mockRepositoryList(): RepositoryList {
  return { items: [mockRepository()], total: 1 };
}

/** 仓库 ACL 的契约响应。 */
export function mockAclList(): AclList {
  return { items: [{ subjectId: 1, action: "read" }] };
}
