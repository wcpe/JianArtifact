// 后端管理 API 的数据契约类型。
// 严格对齐 src/api/** 各 handler 的真实返回结构（非理想化文档）：
// 列表端点（users/repositories/artifacts/tokens/acl）返回裸数组，仅 /search 返回分页结构。

/** 全局角色（后端以小写返回）。 */
export type Role = 'admin' | 'user';

/** 仓库格式。 */
export type RepoFormat = 'maven' | 'npm' | 'docker' | 'raw' | 'cargo' | 'go' | 'pypi' | 'nuget';

/** 仓库类型。 */
export type RepoType = 'hosted' | 'proxy';

/** 仓库可见性。 */
export type Visibility = 'public' | 'private';

/** 仓库 ACL 权限动作（四级，高动作蕴含低动作；FR-48）。 */
export type Permission = 'read' | 'write' | 'delete' | 'admin';

/** 登录 / 刷新 / /me 返回的当前用户信息。 */
export interface UserInfo {
  id: string;
  username: string;
  role: Role;
}

/** 登录 / 刷新成功返回体。 */
export interface LoginResponse {
  access_token: string;
  token_type: string;
  expires_in: number;
  user: UserInfo;
}

/** 用户管理视图（不含口令哈希）。 */
export interface UserView {
  id: string;
  username: string;
  role: Role;
  disabled: boolean;
  created_at: string;
}

/** 仓库视图。 */
export interface RepositoryDto {
  id: string;
  name: string;
  format: RepoFormat;
  type: RepoType;
  visibility: Visibility;
  upstream_url: string | null;
  created_at: string;
}

/** 仓库制品浏览索引项。 */
export interface ArtifactDto {
  path: string;
  size: number;
  sha256: string;
  content_type: string | null;
  cached: boolean;
  created_at: string;
}

/** 四校验和分组。 */
export interface Checksums {
  sha256: string;
  sha1: string;
  md5: string;
  sha512: string;
}

/** 使用方式片段。 */
export interface UsageSnippet {
  title: string;
  language: string;
  content: string;
}

/** 制品详情视图（含四校验和与使用方式片段）。 */
export interface ArtifactDetailDto {
  repo_id: string;
  repo_name: string;
  format: RepoFormat;
  path: string;
  size: number;
  content_type: string | null;
  cached: boolean;
  created_at: string;
  checksums: Checksums;
  usage: UsageSnippet[];
}

/** ACL 条目视图。 */
export interface AclDto {
  id: string;
  user_id: string;
  permission: Permission;
}

/** 用户组视图（FR-49）。 */
export interface GroupView {
  id: string;
  name: string;
  created_at: string;
}

/** 组成员视图（FR-49）。 */
export interface GroupMemberView {
  user_id: string;
  username: string;
}

/** 组 ACL 条目视图（FR-49）。 */
export interface GroupAclView {
  id: string;
  group_id: string;
  permission: Permission;
}

/** API Token 元数据视图。 */
export interface TokenView {
  id: string;
  name: string;
  created_at: string;
  last_used_at: string | null;
  revoked: boolean;
}

/** 签发 Token 返回体（含仅本次可见的明文）。 */
export interface CreateTokenResponse {
  id: string;
  name: string;
  created_at: string;
  token: string;
}

/** 单条搜索命中。 */
export interface SearchHit {
  repo_id: string;
  repo_name: string;
  format: RepoFormat;
  path: string;
  sha256: string;
  size: number;
  created_at: string;
}

/** 统一分页响应结构（仅 /search 使用）。 */
export interface Paginated<T> {
  items: T[];
  total: number;
  offset: number;
  limit: number;
  has_more: boolean;
}

/** 使用分析：制品级聚合（热门制品）。 */
export interface ArtifactUsageDto {
  repo_name: string;
  repo_path: string;
  count: number;
  last_at: string;
}

/** 使用分析：仓库级聚合（仓库用量）。 */
export interface RepoUsageDto {
  repo_name: string;
  count: number;
}

/** 使用分析聚合总览（数据面板，仅管理员）。 */
export interface UsageAnalyticsDto {
  total_access: number;
  total_download: number;
  top_downloads: ArtifactUsageDto[];
  repo_usage: RepoUsageDto[];
}

/** 创建用户请求体。 */
export interface CreateUserRequest {
  username: string;
  password: string;
  role: Role;
}

/** 更新用户请求体。 */
export interface UpdateUserRequest {
  role?: Role;
  disabled?: boolean;
}

/** 创建仓库请求体。 */
export interface CreateRepositoryRequest {
  name: string;
  format: RepoFormat;
  type: RepoType;
  visibility: Visibility;
  upstream_url?: string | null;
  upstream_auth_ref?: string | null;
}

/** 更新仓库请求体。 */
export interface UpdateRepositoryRequest {
  visibility?: Visibility;
  upstream_url?: string | null;
  upstream_auth_ref?: string | null;
}
