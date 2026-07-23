// 端点封装：按 api/openapi.yaml 的 0.2.0 管理面路径提供 typed 调用。
// 页面与数据钩子仅依赖此模块，不直接拼 URL。
import { request } from "./client";
import type {
  AclEntry,
  AclList,
  AssetList,
  LoginResponse,
  MigrationConflictPolicy,
  MigrationDiscoverResponse,
  MigrationPlan,
  MigrationReport,
  MigrationSourceType,
  MigrationTask,
  MigrationTaskList,
  Repository,
  RepositoryList,
  RepoFormat,
  RepoType,
  RepoVisibility,
  StatusInfo,
  TokenCreated,
  TokenList,
  UsageInfo,
  User,
  UserList,
  UserRole,
  UserStatus,
} from "./types";

export interface Pagination {
  page?: number;
  page_size?: number;
}

/** 实例状态：版本、就绪、是否已初始化、用户数（自举判定依据）。 */
export function getStatus(): Promise<StatusInfo> {
  return request<StatusInfo>("/status");
}

/** 空库自举：创建首个管理员并返回会话令牌。 */
export function bootstrap(username: string, password: string): Promise<LoginResponse> {
  return request<LoginResponse>("/auth/bootstrap", {
    method: "POST",
    body: { username, password },
  });
}

/** 登录：校验凭据并返回会话令牌与用户。 */
export function login(username: string, password: string): Promise<LoginResponse> {
  return request<LoginResponse>("/auth/login", {
    method: "POST",
    body: { username, password },
  });
}

/** 登出：使当前会话失效。 */
export function logout(): Promise<void> {
  return request<void>("/auth/logout", { method: "POST" });
}

export function listUsers(params: Pagination = {}): Promise<UserList> {
  return request<UserList>("/users", { query: { page: params.page, page_size: params.page_size } });
}

export function createUser(input: {
  username: string;
  password: string;
  role: UserRole;
}): Promise<User> {
  return request<User>("/users", { method: "POST", body: input });
}

export function updateUser(
  id: number,
  patch: { role?: UserRole; status?: UserStatus },
): Promise<User> {
  return request<User>(`/users/${id}`, { method: "PATCH", body: patch });
}

export function deleteUser(id: number): Promise<void> {
  return request<void>(`/users/${id}`, { method: "DELETE" });
}

export function changePassword(id: number, password: string): Promise<void> {
  return request<void>(`/users/${id}/password`, { method: "POST", body: { password } });
}

export function listTokens(): Promise<TokenList> {
  return request<TokenList>("/tokens");
}

export function createToken(name: string): Promise<TokenCreated> {
  return request<TokenCreated>("/tokens", { method: "POST", body: { name } });
}

export function deleteToken(id: number): Promise<void> {
  return request<void>(`/tokens/${id}`, { method: "DELETE" });
}

export function listRepositories(params: Pagination = {}): Promise<RepositoryList> {
  return request<RepositoryList>("/repositories", {
    query: { page: params.page, page_size: params.page_size },
  });
}

export function createRepository(input: {
  name: string;
  format: RepoFormat;
  type: RepoType;
  visibility: RepoVisibility;
  remoteUrl?: string;
  members?: string[];
}): Promise<Repository> {
  return request<Repository>("/repositories", { method: "POST", body: input });
}

export function updateRepository(
  name: string,
  patch: { visibility?: RepoVisibility; remoteUrl?: string; members?: string[] },
): Promise<Repository> {
  return request<Repository>(`/repositories/${name}`, { method: "PATCH", body: patch });
}

export function deleteRepository(name: string): Promise<void> {
  return request<void>(`/repositories/${name}`, { method: "DELETE" });
}

export function getAcl(name: string): Promise<AclList> {
  return request<AclList>(`/repositories/${name}/acl`);
}

export function setAcl(name: string, items: AclEntry[]): Promise<AclList> {
  return request<AclList>(`/repositories/${name}/acl`, { method: "PUT", body: { items } });
}

export interface AssetQuery extends Pagination {
  prefix?: string;
}

/** 制品浏览：分页列出仓库内 asset 路径/大小/hash/更新时间，支持前缀过滤。 */
export function listRepositoryAssets(name: string, params: AssetQuery = {}): Promise<AssetList> {
  return request<AssetList>(`/repositories/${name}/assets`, {
    query: { page: params.page, page_size: params.page_size, prefix: params.prefix },
  });
}

/** 使用说明：据 format/type 返回 curl/mvn/npm 客户端配置片段。 */
export function getRepositoryUsage(name: string): Promise<UsageInfo> {
  return request<UsageInfo>(`/repositories/${name}/usage`);
}

// —— Nexus 迁移（0.4.0）——

export function listMigrations(params: Pagination = {}): Promise<MigrationTaskList> {
  return request<MigrationTaskList>("/migrations", {
    query: { page: params.page, page_size: params.page_size },
  });
}

export function getMigration(id: number): Promise<MigrationTask> {
  return request<MigrationTask>(`/migrations/${id}`);
}

export function createMigration(input: {
  sourceType: MigrationSourceType;
  sourceConfig?: Record<string, unknown>;
  credentialRef?: string;
  conflictPolicy?: MigrationConflictPolicy;
  plan?: MigrationPlan;
}): Promise<MigrationTask> {
  return request<MigrationTask>("/migrations", { method: "POST", body: input });
}

export function discoverMigrations(
  input: {
    sourceType: MigrationSourceType;
    sourceConfig?: Record<string, unknown>;
    credentialRef?: string;
    conflictPolicy?: MigrationConflictPolicy;
  },
  opts?: { signal?: AbortSignal },
): Promise<MigrationDiscoverResponse> {
  return request<MigrationDiscoverResponse>("/migrations/discover", {
    method: "POST",
    body: input,
    signal: opts?.signal,
  });
}

/** 从在线 Nexus 仅拉仓库索引（不创建迁移任务、不扫资产）。 */
export function listRemoteNexusRepositories(
  input: { url: string; credentialRef?: string },
  opts?: { signal?: AbortSignal },
): Promise<{ items: { name: string; format: string; type: string }[]; total: number }> {
  return request("/migrations/remote-repositories", {
    method: "POST",
    body: {
      url: input.url,
      credentialRef: input.credentialRef || undefined,
    },
    signal: opts?.signal,
  });
}

/** 离线目录持久化索引状态。 */
export interface OfflineDirIndexStatus {
  path?: string;
  status: "idle" | "scanning" | "ready" | "failed" | string;
  mode?: string;
  totalEntries?: number;
  scannedProps?: number;
  repoCount?: number;
  message?: string;
  errorMessage?: string;
  startedAt?: string;
  finishedAt?: string;
  updatedAt?: string;
  repositories?: { name: string; assets: number }[];
}

/** 启动离线目录前置扫描（full / update / rebuild）。 */
export function startOfflineDirIndex(input: {
  path: string;
  mode?: "full" | "update" | "rebuild";
}): Promise<OfflineDirIndexStatus> {
  return request("/migrations/offline-index/scan", {
    method: "POST",
    body: { path: input.path, mode: input.mode ?? "full" },
  });
}

/** 查询离线目录索引状态。 */
export function getOfflineDirIndex(
  path: string,
  opts?: { signal?: AbortSignal },
): Promise<OfflineDirIndexStatus> {
  return request("/migrations/offline-index", {
    query: { path },
    signal: opts?.signal,
  });
}

/** 取消离线目录索引扫描。 */
export function cancelOfflineDirIndex(path: string): Promise<{ ok: boolean }> {
  return request("/migrations/offline-index/cancel", {
    method: "POST",
    body: { path },
  });
}

export function startMigration(
  id: number,
  body?: { includeRepositories?: string[] },
): Promise<MigrationTask> {
  return request<MigrationTask>(`/migrations/${id}/start`, {
    method: "POST",
    body: body ?? {},
  });
}

export function resumeMigration(id: number): Promise<MigrationTask> {
  return request<MigrationTask>(`/migrations/${id}/resume`, { method: "POST" });
}

export function cancelMigration(id: number): Promise<MigrationTask> {
  return request<MigrationTask>(`/migrations/${id}/cancel`, { method: "POST" });
}

export function getMigrationReport(id: number): Promise<MigrationReport> {
  return request<MigrationReport>(`/migrations/${id}/report`);
}

export function finalizeMigration(id: number): Promise<MigrationTask> {
  return request<MigrationTask>(`/migrations/${id}/finalize`, { method: "POST" });
}
