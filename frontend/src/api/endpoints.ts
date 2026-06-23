// 类型化的管理 API 端点封装：每个函数对应 docs/API.md / src/api 的一个端点。
// 路径与字段严格对齐后端真实契约。

import { request } from './client';
import type {
  AclDto,
  ArtifactDetailDto,
  ArtifactDto,
  CreateRepositoryRequest,
  CreateTokenResponse,
  CreateUserRequest,
  LoginResponse,
  Paginated,
  Permission,
  RepoFormat,
  RepositoryDto,
  SearchHit,
  TokenView,
  UpdateRepositoryRequest,
  UpdateUserRequest,
  UserInfo,
  UserView,
} from './types';

// —— 认证 ——

/** 登录：用户名 + 口令换取 JWT 会话。 */
export function login(username: string, password: string): Promise<LoginResponse> {
  return request<LoginResponse>('/auth/login', {
    method: 'POST',
    body: { username, password },
  });
}

/** 登出：无状态 JWT 下服务端返回成功，由客户端丢弃令牌。 */
export function logout(): Promise<void> {
  return request<void>('/auth/logout', { method: 'POST' });
}

/** 刷新会话：换发新的 JWT。 */
export function refresh(): Promise<LoginResponse> {
  return request<LoginResponse>('/auth/refresh', { method: 'POST' });
}

/** 当前用户：判定登录态与角色。 */
export function me(): Promise<UserInfo> {
  return request<UserInfo>('/me');
}

// —— 用户管理（仅管理员） ——

/** 列出全部用户。 */
export function listUsers(): Promise<UserView[]> {
  return request<UserView[]>('/users');
}

/** 创建用户。 */
export function createUser(req: CreateUserRequest): Promise<UserView> {
  return request<UserView>('/users', { method: 'POST', body: req });
}

/** 更新用户（角色 / 禁用）。 */
export function updateUser(id: string, req: UpdateUserRequest): Promise<UserView> {
  return request<UserView>(`/users/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: req,
  });
}

/** 删除用户。 */
export function deleteUser(id: string): Promise<void> {
  return request<void>(`/users/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

// —— 仓库管理 ——

/** 列出可见仓库（按身份过滤）。 */
export function listRepositories(): Promise<RepositoryDto[]> {
  return request<RepositoryDto[]>('/repositories');
}

/** 获取仓库详情。 */
export function getRepository(id: string): Promise<RepositoryDto> {
  return request<RepositoryDto>(`/repositories/${encodeURIComponent(id)}`);
}

/** 创建仓库（仅管理员）。 */
export function createRepository(req: CreateRepositoryRequest): Promise<RepositoryDto> {
  return request<RepositoryDto>('/repositories', { method: 'POST', body: req });
}

/** 更新仓库（仅管理员）。 */
export function updateRepository(id: string, req: UpdateRepositoryRequest): Promise<RepositoryDto> {
  return request<RepositoryDto>(`/repositories/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: req,
  });
}

/** 删除仓库（仅管理员）。 */
export function deleteRepository(id: string): Promise<void> {
  return request<void>(`/repositories/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

/** 浏览仓库制品索引。 */
export function listArtifacts(repoId: string): Promise<ArtifactDto[]> {
  return request<ArtifactDto[]>(`/repositories/${encodeURIComponent(repoId)}/artifacts`);
}

/** 制品详情（含四校验和与使用方式片段）。后端路径为 catch-all，path 不整体编码。 */
export function getArtifactDetail(repoId: string, path: string): Promise<ArtifactDetailDto> {
  return request<ArtifactDetailDto>(
    `/repositories/${encodeURIComponent(repoId)}/artifacts/${encodePath(path)}`,
  );
}

/** 删除制品（需写权限或管理员）。 */
export function deleteArtifact(repoId: string, path: string): Promise<void> {
  return request<void>(
    `/repositories/${encodeURIComponent(repoId)}/artifacts/${encodePath(path)}`,
    { method: 'DELETE' },
  );
}

// —— 仓库 ACL（仅管理员） ——

/** 列出某仓库 ACL。 */
export function listAcl(repoId: string): Promise<AclDto[]> {
  return request<AclDto[]>(`/repositories/${encodeURIComponent(repoId)}/acl`);
}

/** 新增一条 ACL。 */
export function createAcl(repoId: string, userId: string, permission: Permission): Promise<AclDto> {
  return request<AclDto>(`/repositories/${encodeURIComponent(repoId)}/acl`, {
    method: 'POST',
    body: { user_id: userId, permission },
  });
}

/** 移除一条 ACL。 */
export function deleteAcl(repoId: string, aclId: string): Promise<void> {
  return request<void>(
    `/repositories/${encodeURIComponent(repoId)}/acl/${encodeURIComponent(aclId)}`,
    { method: 'DELETE' },
  );
}

// —— Token 管理（自助） ——

/** 列出当前用户的 Token。 */
export function listTokens(): Promise<TokenView[]> {
  return request<TokenView[]>('/tokens');
}

/** 签发一枚 Token（返回仅本次可见的明文）。 */
export function createToken(name: string): Promise<CreateTokenResponse> {
  return request<CreateTokenResponse>('/tokens', { method: 'POST', body: { name } });
}

/** 吊销一枚 Token。 */
export function revokeToken(id: string): Promise<void> {
  return request<void>(`/tokens/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

// —— 跨仓库搜索 ——

/** 跨仓库搜索制品（结果按读权限过滤）。 */
export function search(
  q: string,
  options: { format?: RepoFormat; offset?: number; limit?: number } = {},
): Promise<Paginated<SearchHit>> {
  return request<Paginated<SearchHit>>('/search', {
    query: {
      q,
      format: options.format,
      offset: options.offset,
      limit: options.limit,
    },
  });
}

/** 对制品路径逐段编码（保留 `/` 分隔，避免破坏 catch-all 路径语义）。 */
function encodePath(path: string): string {
  return path
    .split('/')
    .map((seg) => encodeURIComponent(seg))
    .join('/');
}
