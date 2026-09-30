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
export type PinnedRepositoriesResponse = Schemas["PinnedRepositoriesResponse"];
export type PutPinnedRepositoriesRequest = Schemas["PutPinnedRepositoriesRequest"];
export type PublicRepositoryList = Schemas["PublicRepositoryList"];
export type ConnectionStatus = Schemas["ConnectionStatus"];
export type AclList = Schemas["AclList"];
export type AssetList = Schemas["AssetList"];
export type BatchDeleteAssetsRequest = Schemas["BatchDeleteAssetsRequest"];
export type BatchDeleteAssetFailure = Schemas["BatchDeleteAssetFailure"];
export type BatchDeleteAssetsResponse = Schemas["BatchDeleteAssetsResponse"];
export type PublishPoliciesBatchRequest = Schemas["PublishPoliciesBatchRequest"];
export type PublishPolicyBatchResult = Schemas["PublishPolicyBatchResult"];
export type PublishPoliciesBatchResponse = Schemas["PublishPoliciesBatchResponse"];
export type UsageInfo = Schemas["UsageInfo"];
/** FR-41：存储治理运维作业面。 */
export type MaintenanceJob = Schemas["MaintenanceJob"];
export type MaintenanceJobList = Schemas["MaintenanceJobList"];
export type MaintenanceJobRunResult = Schemas["MaintenanceJobRunResult"];

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

/** GET /metrics 的 Prometheus 文本暴露格式样本（与后端指标口径一致的最小集合）。 */
export function mockMetrics(): string {
  return [
    "# HELP jianartifact_protocol_requests_total 制品协议请求完成计数（按方法、状态码与缓存结果分区）",
    "# TYPE jianartifact_protocol_requests_total counter",
    'jianartifact_protocol_requests_total{cache_result="hit",method="GET",status="200"} 12',
    "# HELP jianartifact_scheduler_job_runs_total 定时任务作业累计执行次数",
    "# TYPE jianartifact_scheduler_job_runs_total counter",
    'jianartifact_scheduler_job_runs_total{job="blob-gc"} 3',
    "# HELP jianartifact_runtime_goroutines 当前 goroutine 数量",
    "# TYPE jianartifact_runtime_goroutines gauge",
    "jianartifact_runtime_goroutines 24",
    "",
  ].join("\n");
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
    // FR-34：开发环境不接真实 IdP，登录入口保持隐藏。
    oidcEnabled: false,
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
    aliases: ["maven-legacy"],
    // FR-41：仓库存储配额（0/缺省 = 不限）；契约校验覆盖这两个新字段。
    quotaBytes: 10737418240,
    quotaAssets: 2000,
  };
}

/** FR-41：GET /api/v1/maintenance/jobs 的契约响应。 */
export function mockMaintenanceJobList(): MaintenanceJobList {
  return {
    jobs: [
      // 执行过且最近一次失败：lastError 有值。
      {
        name: "blob-gc",
        intervalSeconds: 86400,
        running: false,
        runs: 3,
        failures: 1,
        lastStartedAt: "2026-01-05T02:00:00Z",
        lastFinishedAt: "2026-01-05T02:00:03Z",
        lastError: "扫描活动目录失败：磁盘不可读",
      },
      // 从未执行过：三个可空字段为 null（与后端"未执行过则为空"的语义对齐）。
      {
        name: "storage-cleanup",
        intervalSeconds: 86400,
        running: false,
        runs: 0,
        failures: 0,
        lastStartedAt: null,
        lastFinishedAt: null,
        lastError: null,
      },
    ],
  };
}

/** FR-41：POST /api/v1/maintenance/jobs/{name}/run 的契约响应（202 已受理）。 */
export function mockMaintenanceJobRunResult(name = "blob-gc"): MaintenanceJobRunResult {
  return { name, started: true };
}

/** GET /api/v1/repositories 的契约响应。 */
export function mockRepositoryList(): RepositoryList {
  return { items: [mockRepository()], total: 1 };
}

/** GET /api/v1/me/pinned-repositories 与 /settings/pinned-repositories 的契约响应。 */
export function mockPinnedRepositories(): PinnedRepositoriesResponse {
  return { repositoryIds: [1, 2], names: ["maven-releases", "npm-proxy"] };
}

/** PUT /api/v1/{me,settings}/pinned-repositories 的契约请求体。 */
export function mockPutPinnedRepositories(): PutPinnedRepositoriesRequest {
  return { repositoryIds: [1, 2] };
}

/** GET /api/v1/public/repositories 的契约响应（携带全局置顶名）。 */
export function mockPublicRepositoryList(): PublicRepositoryList {
  return { items: [mockRepository()], total: 1, pinnedNames: ["npm-proxy"] };
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

/** PUT /api/v1/users/{id}/publish-policies 的契约请求体（批量应用到两个 Hosted 仓库）。 */
export function mockPublishPoliciesBatchRequest(): PublishPoliciesBatchRequest {
  return {
    repositories: ["maven-releases", "raw-hosted"],
    webLoginDisabled: false,
    allowedPrefixes: ["releases"],
    maxAssetsHour: 0,
    maxBytesDay: 0,
    maxFileBytes: 0,
  };
}

/** PUT /api/v1/users/{id}/publish-policies 的契约响应：逐仓库结果，含一条失败样例。 */
export function mockPublishPoliciesBatchResponse(): PublishPoliciesBatchResponse {
  const results: PublishPolicyBatchResult[] = [
    { repository: "maven-releases", ok: true },
    { repository: "raw-hosted", ok: false, error: "内部错误" },
  ];
  return { results };
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
        group: "resolve",
        tool: "maven",
      },
    ],
  };
}
