// 端点封装：按 api/openapi.yaml 的 0.2.0 管理面路径提供 typed 调用。
// 页面与数据钩子仅依赖此模块，不直接拼 URL。
import { request } from "./client";
import type {
  AclEntry,
  AclList,
  AssetList,
  LoginResponse,
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
