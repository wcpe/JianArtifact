// Mock 处理器：产出符合契约的响应体。类型绑定到 openapi-typescript 生成的
// schema.gen.ts（编译期），运行期再由 contract 测试用 ajv 对同一契约做校验。
import type { components } from "./schema.gen";
import { MOCK_APP_VERSION } from "./version";

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
export type ConnectionStatus = Schemas["ConnectionStatus"];
export type AclList = Schemas["AclList"];
export type AssetList = Schemas["AssetList"];
export type BatchDeleteAssetsRequest = Schemas["BatchDeleteAssetsRequest"];
export type BatchDeleteAssetFailure = Schemas["BatchDeleteAssetFailure"];
export type BatchDeleteAssetsResponse = Schemas["BatchDeleteAssetsResponse"];
export type UsageInfo = Schemas["UsageInfo"];

/** GET /api/v1/audit-logs 的非 OpenAPI 管理面响应。 */
export interface MockAuditLogEntry {
  id: number;
  ts: string;
  actor: string;
  action: string;
  entityType: string;
  entityKey: string;
  repo: string;
  detail: string;
  result: string;
  ip: string;
  userId?: number;
  authSource?: string;
  tokenId?: number;
  tokenName?: string;
  userAgent?: string;
  requestId?: string;
}

export interface MockAuditLogList {
  items: MockAuditLogEntry[];
  total: number;
}

const MOCK_TIME = "2026-01-01T00:00:00Z";

/** GET /healthz 的契约响应。 */
export function mockHealthz(version = MOCK_APP_VERSION): HealthStatus {
  return { status: "ok", version };
}

/** GET /readyz 就绪时的契约响应。 */
export function mockReadyz(version = MOCK_APP_VERSION): HealthStatus {
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
export function mockStatus(version = MOCK_APP_VERSION): StatusInfo {
  return {
    version,
    ready: true,
    initialized: true,
    migrationVersion: "0001_init",
    userCount: 1,
    bootstrapAllowed: false,
  };
}

/** 单个用户的契约响应。 */
export function mockUser(): User {
  return {
    id: 1,
    username: "admin",
    role: "admin",
    status: "active",
    webLoginDisabled: false,
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

/** FR-114：仓库连接状态的契约响应（AUTO_BLOCKED 示例，含阻止窗口）。 */
export function mockConnectionStatus(): ConnectionStatus {
  return {
    status: "AUTO_BLOCKED",
    blockedUntil: "2026-01-05T00:00:00Z",
    description: "上游不可用，自动阻止中",
  };
}

/** 仓库 ACL 的契约响应。 */
export function mockAclList(): AclList {
  return { items: [{ subjectId: 1, action: "read" }] };
}

/** GET /api/v1/repositories/{name}/assets 的契约响应。 */
export function mockAssetList(): AssetList {
  return {
    items: [
      {
        path: "com/example/app/1.0.0/app-1.0.0.jar",
        size: 20480,
        hash: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
        contentType: "application/java-archive",
        updatedAt: MOCK_TIME,
      },
    ],
    total: 1,
  };
}

/** POST /api/v1/repositories/{name}/assets/batch-delete 的契约响应（全成功）。 */
export function mockBatchDeleteAssets(): BatchDeleteAssetsResponse {
  return {
    deleted: 2,
    failed: [{ path: "no/such.txt", error: "资源不存在" }],
  };
}

/** GET /api/v1/audit-logs 的稳定样本（该端点尚未纳入 OpenAPI）。 */
export function mockAuditLogList(): MockAuditLogList {
  const items: MockAuditLogEntry[] = [
    {
      id: 3,
      ts: "2026-01-03T00:00:00Z",
      actor: "admin",
      action: "user.create",
      entityType: "user",
      entityKey: "bob",
      repo: "",
      detail: "role=user",
      result: "ok",
      ip: "127.0.0.1",
    },
    {
      id: 2,
      ts: "2026-01-02T00:00:00Z",
      actor: "alice",
      action: "asset.delete",
      entityType: "asset",
      entityKey: "maven-releases/app.jar",
      repo: "maven-releases",
      detail: "",
      result: "ok",
      ip: "127.0.0.1",
    },
    {
      id: 1,
      ts: "2025-12-31T00:00:00Z",
      actor: "admin",
      action: "repo.create",
      entityType: "repository",
      entityKey: "npm-proxy",
      repo: "npm-proxy",
      detail: "format=npm",
      result: "ok",
      ip: "127.0.0.1",
    },
  ];
  items.push(
    ...Array.from({ length: 48 }, (_, index) => ({
      id: index + 4,
      ts: `2026-01-${String((index % 28) + 4).padStart(2, "0")}T00:00:00Z`,
      actor: `user-${index + 4}`,
      action: "asset.read",
      entityType: "asset",
      entityKey: `maven-releases/app-${index + 4}.jar`,
      repo: "maven-releases",
      detail: "",
      result: "ok",
      ip: "127.0.0.1",
    })),
  );
  return { items, total: items.length };
}

/** GET /api/v1/repositories/{name}/usage 的契约响应。 */
export function mockUsageInfo(): UsageInfo {
  return {
    format: "maven",
    type: "hosted",
    snippets: [
      {
        title: "解析依赖（pom.xml）",
        description: "在 <repositories> 中声明该仓库。",
        code: "<repository>...</repository>",
      },
    ],
  };
}
