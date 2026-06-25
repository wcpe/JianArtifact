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

// —— 防护配置（FR-79，对齐后端 src/config.rs 的 ProtectionConfig 子树）——

/** 多维速率限制与并发上限配置。 */
export interface RateLimitConfig {
  enabled: boolean;
  window_secs: number;
  ip_max_requests: number;
  identity_max_requests: number;
  repo_max_requests: number;
  ip_max_concurrent: number;
  user_max_concurrent: number;
  repo_max_concurrent: number;
}

/** IP 黑 / 白名单配置（单 IP 或 CIDR）。 */
export interface IpListConfig {
  allow: string[];
  deny: string[];
}

/** 访问异常检测与自动封禁配置。 */
export interface BanConfig {
  enabled: boolean;
  window_secs: number;
  threshold: number;
  duration_secs: number;
}

/** 慢速攻击防护与通用请求体大小上限配置。 */
export interface SlowlorisConfig {
  enabled: boolean;
  body_read_timeout_secs: number;
  header_timeout_secs: number;
  max_body_bytes: number;
}

/** CC 挑战（工作量证明 PoW）配置。 */
export interface CcChallengeConfig {
  enabled: boolean;
  difficulty: number;
  ttl_secs: number;
  exempt_authenticated: boolean;
}

/** 单条 WAF 规则配置。 */
export interface WafRuleConfig {
  field: string;
  header_name?: string | null;
  pattern: string;
  match_type: string;
  action: string;
}

/** 可配置 WAF 规则引擎配置。 */
export interface WafConfig {
  enabled: boolean;
  rules: WafRuleConfig[];
}

/** 防护监控与阈值告警配置。 */
export interface AlertsConfig {
  enabled: boolean;
  window_secs: number;
  rate_limit_warn_threshold: number;
  ban_warn_threshold: number;
  cc_challenge_fail_warn_threshold: number;
  waf_block_warn_threshold: number;
  slowloris_warn_threshold: number;
  max_rows: number;
}

/** 防护配置全量（七个维度），GET / PATCH /api/v1/protection/config 的载荷。 */
export interface ProtectionConfig {
  rate_limit: RateLimitConfig;
  ip_list: IpListConfig;
  ban: BanConfig;
  slowloris: SlowlorisConfig;
  cc_challenge: CcChallengeConfig;
  waf: WafConfig;
  alerts: AlertsConfig;
}

/** 单条审计日志视图（FR-77，对齐 GET /api/v1/audit 的 AuditEntryDto）。 */
export interface AuditEntryDto {
  id: number;
  ts: string;
  actor: string;
  actor_kind: string;
  request_id: string | null;
  source_ip: string | null;
  action: string;
  target_repo: string | null;
  target: string | null;
  result: string;
  detail: string | null;
}

/** 审计日志查询过滤参数（FR-77，均可选；分页用 offset/limit）。 */
export interface AuditListParams {
  action?: string;
  target_repo?: string;
  actor?: string;
  offset?: number;
  limit?: number;
}
