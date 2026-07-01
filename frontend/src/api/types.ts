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

/** 单条系统运行日志视图（FR-107，对齐 GET /api/v1/system-logs 的 LogEntry）。 */
export interface SystemLogEntryDto {
  /** RFC3339 时间戳；无法解析为 null。 */
  timestamp: string | null;
  /** 级别规范大写串（ERROR/WARN/INFO/DEBUG/TRACE）；无法解析为 null。 */
  level: string | null;
  /** 消息正文（含 target 与字段）。 */
  message: string;
}

/** 系统日志查询参数（FR-107，均可选；分页用 offset/limit，tail 最新在前）。 */
export interface SystemLogListParams {
  /** 按级别过滤（ERROR/WARN/INFO/DEBUG/TRACE）。 */
  level?: string;
  offset?: number;
  limit?: number;
}

/** 防护维度（与后端 ProtectionDimension 入库字符串一致；FR-78）。 */
export type ProtectionDimension = 'rate_limit' | 'ban' | 'cc_challenge' | 'waf' | 'slowloris';

/** 告警严重度（后端以小写返回）。 */
export type AlertSeverity = 'warn' | 'error';

/** 单维度窗内计数（状态快照项，FR-78）。 */
export interface DimensionCountDto {
  dimension: string;
  count: number;
}

/** 单条防护告警视图（对齐 protection_alerts 字段，FR-78）。 */
export interface ProtectionAlertDto {
  id: number;
  ts: string;
  dimension: string;
  severity: string;
  observed_value: number;
  threshold: number;
  window_secs: number;
  detail: string | null;
}

/** 防护状态快照（数据面板总览，仅管理员，FR-78）。 */
export interface ProtectionStatusDto {
  alerts_enabled: boolean;
  window_secs: number;
  window_counts: DimensionCountDto[];
  active_banned_ips: number;
  dropped_alerts: number;
  recent_alerts: ProtectionAlertDto[];
}

// —— Nexus 迁移（FR-81，对接 ADR-0006 的已有迁移端点） ——

/**
 * 在线预览：从源 Nexus 枚举出的单个仓库元数据。
 * 严格对齐后端 NexusRepoSummary（src/migrate/mod.rs）。
 * `type` 为 Nexus 原样值（hosted / proxy / group）；`upstream_url` 仅 proxy 有值。
 */
export interface NexusRepoSummary {
  name: string;
  format: string;
  type: string;
  upstream_url: string | null;
}

/**
 * 离线预览：blob store 中单个 blob 的基本元数据。
 * 严格对齐后端 OfflineBlobSummary（src/migrate/offline.rs）。
 */
export interface OfflineBlobSummary {
  blob_name: string;
  sha1: string | null;
  size: number | null;
}

/**
 * 离线预览：按 repo 聚合的分组结果。
 * 严格对齐后端 OfflineRepoSummary（src/migrate/offline.rs）。
 */
export interface OfflineRepoSummary {
  repo_name: string;
  blob_count: number;
  blobs: OfflineBlobSummary[];
}

/**
 * 单个仓库的迁移结果（proxy / hosted 报告共用同构结构）。
 * 严格对齐后端 RepoMigrationOutcome / HostedRepoMigrationOutcome。
 */
export interface RepoMigrationOutcome {
  repo_name: string;
  format: string;
  created: boolean;
  migrated_artifacts: number;
  skipped_artifacts: number;
}

/**
 * 迁移报告（proxy / hosted 报告共用同构结构）。
 * 严格对齐后端 ProxyMigrationReport / HostedMigrationReport。
 */
export interface MigrationReport {
  repos: RepoMigrationOutcome[];
  skipped_repos: string[];
}

/** 在线预览请求体（auth_ref 仅引用，凭据真值走后端 env，不入库不回显）。 */
export interface NexusPreviewRequest {
  base_url: string;
  auth_ref?: string | null;
}

/** 离线预览请求体（本地 blob store 根目录路径）。 */
export interface NexusOfflinePreviewRequest {
  path: string;
}

/** proxy / hosted 搬运请求体（在线枚举配置 + 离线 blob store 提供制品本体）。 */
export interface NexusMigrateRequest {
  base_url: string;
  auth_ref?: string | null;
  offline_path: string;
}

// —— Nexus 在线拉取迁移（FR-82，对接后端 /migrate/nexus/online/migrate） ——

/**
 * 在线拉取：单个待迁移仓库的选择项。
 * `source` 为源仓库名；`target` 省略 / 为空则与源同名（允许改名）。
 * 严格对齐后端 OnlineRepoSelection。
 */
export interface OnlineRepoSelection {
  source: string;
  target?: string | null;
}

/**
 * 在线拉取迁移请求体（经 REST 枚举 + HTTP 下载，无需离线目录）。
 * 凭据仅以引用名 auth_ref 提供，真值走后端 env，不入库、不回显。
 * 严格对齐后端 OnlineMigrateRequest。
 */
export interface OnlineMigrateRequest {
  base_url: string;
  auth_ref?: string | null;
  repositories: OnlineRepoSelection[];
}

/**
 * 在线拉取：单个仓库的迁移结果。
 * 严格对齐后端 OnlineRepoMigrationOutcome。
 */
export interface OnlineRepoMigrationOutcome {
  source_repo: string;
  target_repo: string;
  format: string;
  created: boolean;
  migrated_artifacts: number;
  skipped_artifacts: number;
}

/**
 * 在线拉取迁移报告。
 * 严格对齐后端 OnlineMigrationReport。
 */
export interface OnlineMigrationReport {
  repos: OnlineRepoMigrationOutcome[];
  skipped_repos: string[];
}

// —— Nexus 在线拉取异步任务（FR-83，对接后端异步任务 + 进度查询） ——

/**
 * 发起在线拉取迁移后立即返回的任务句柄（202 Accepted）。
 * 严格对齐后端 POST /migrate/nexus/online/migrate 的 202 响应体。
 */
export interface MigrationJobCreated {
  job_id: string;
}

/**
 * 在线拉取任务的阶段。
 * - enumerating：经 REST 枚举待迁移资产；
 * - downloading：逐个 HTTP 下载并搬运；
 * - paused：已被运维暂停，后台循环挂起等待继续（FR-91）；
 * - cancelled：已被运维取消，停止后续搬运（不算失败，FR-91）；
 * - done：全部完成；
 * - failed：任务失败（详见 error）。
 * 严格对齐后端 phase 字段取值。
 */
export type OnlinePullPhase =
  | 'enumerating'
  | 'downloading'
  | 'paused'
  | 'cancelled'
  | 'done'
  | 'failed';

/**
 * 在线拉取任务进度快照（GET /migrate/jobs/{id}）。
 * `repos` / `skipped_repos` / `error` 在终态（done / failed）时填充。
 * 严格对齐后端任务进度快照结构。
 */
export interface OnlinePullJob {
  job_id: string;
  phase: OnlinePullPhase;
  total_assets: number;
  done_assets: number;
  migrated: number;
  skipped: number;
  current_repo: string | null;
  current_path: string | null;
  /** 是否处于暂停态（FR-91）：暂停期间为真，继续后置假。 */
  paused: boolean;
  repos: OnlineRepoMigrationOutcome[];
  skipped_repos: string[];
  error: string | null;
  /**
   * 离线 blob store 预览枚举结果（FR-124）：仅离线预览任务在 `phase === 'done'` 时填充；
   * 在线拉取任务无此字段。严格对齐后端 OnlinePullProgress.offline_preview。
   */
  offline_preview?: OfflineRepoSummary[];
}

/**
 * 活动 / 近期任务列表项（GET /migrate/jobs，供客户端重连点选）。
 * 严格对齐后端任务列表项结构。
 */
export interface MigrationJobSummary {
  job_id: string;
  phase: OnlinePullPhase;
  total_assets: number;
  done_assets: number;
  migrated: number;
  skipped: number;
  current_repo: string | null;
  /** 是否处于暂停态（FR-91）。 */
  paused: boolean;
}

// —— 仪表盘全局概览（FR-108，仅管理员；对齐后端 src/api/dashboard.rs 的 DashboardSummaryDto） ——

/**
 * 仪表盘 KPI 概览（GET /api/v1/dashboard/summary，仅管理员）。
 * `artifact_count` 为制品索引条目数（不去重）；`total_bytes` 为按 sha256 去重的占盘字节。
 */
export interface DashboardSummary {
  repo_count: number;
  artifact_count: number;
  total_bytes: number;
  user_count: number;
}

// —— 主机 / 系统监控（FR-98，仅管理员；对齐后端 src/monitor/mod.rs 的 HostMetrics DTO） ——

/** CPU 指标（usage_percent 首样可能为 0，属后端已知取舍）。 */
export interface CpuMetrics {
  usage_percent: number;
  logical_cores: number;
}

/** 内存与交换分区指标（单位：字节）。 */
export interface MemoryMetrics {
  total_bytes: number;
  used_bytes: number;
  swap_total_bytes: number;
  swap_used_bytes: number;
}

/** 单块磁盘明细（单位：字节）。 */
export interface DiskEntry {
  mount_point: string;
  total_bytes: number;
  available_bytes: number;
}

/** 磁盘指标：逐盘明细 + 总量 / 可用汇总（单位：字节）。 */
export interface DiskMetrics {
  total_bytes: number;
  available_bytes: number;
  disks: DiskEntry[];
}

/** 主机指标快照（GET /api/v1/monitor/host，仅管理员，按请求采样）。 */
export interface HostMetrics {
  cpu: CpuMetrics;
  memory: MemoryMetrics;
  disk: DiskMetrics;
  uptime_secs: number;
}

// —— 指标时序（FR-105，仅管理员；对齐后端 GET /api/v1/monitor/metrics 契约） ——

/** 单个时序点（ts 为 Unix 毫秒 UTC，value 为标量取值）。 */
export interface MetricPoint {
  ts: number;
  value: number;
}

/** 某指标键的时序响应（points 按 ts 升序）。 */
export interface MetricSeries {
  metric: string;
  points: MetricPoint[];
}

// —— 设置页（FR-87，仅管理员） ——

/** 单代理视图（脱敏后：URL 去 userinfo；用户名回显、密码仅以 has_password 暴露，FR-100）。 */
export interface ProxyEntryView {
  url: string | null;
  username: string | null;
  has_password: boolean;
}

/** 网络代理视图（http / https / all 三槽，均脱敏不回显密码，FR-100）。 */
export interface NetworkProxyView {
  http: ProxyEntryView;
  https: ProxyEntryView;
  all: ProxyEntryView;
  no_proxy: string | null;
}

/** 在线更新视图（脱敏后：token 仅以 has_token 布尔暴露，不回显本体）。 */
export interface UpdateView {
  enabled: boolean;
  repo: string;
  api_base_url: string;
  restart_mode: string;
  /** 更新通道（stable / prerelease，FR-89）。 */
  channel: string;
  has_token: boolean;
  /** 是否有可回滚的上一版本备份（FR-104）：true 时启用回滚按钮。 */
  rollback_available: boolean;
}

/** 设置页聚合视图（GET /api/v1/settings，仅管理员）。 */
export interface SettingsView {
  current_version: string;
  network_proxy: NetworkProxyView;
  update: UpdateView;
}

// —— 设置编辑（FR-88，仅管理员，运行时热替换） ——

/** 单代理编辑项（FR-100）。url 空 / 缺省=清除该代理；password 三态：缺省=保留现有 / ""=清空 / 非空=设置。 */
export interface ProxyEntryPatch {
  url?: string;
  username?: string;
  password?: string;
}

/** 网络代理编辑项（http / https / all 三槽 + no_proxy；凭据只入内存槽不回显，FR-100）。 */
export interface NetworkProxyPatch {
  http: ProxyEntryPatch;
  https: ProxyEntryPatch;
  all: ProxyEntryPatch;
  no_proxy?: string;
}

/** 在线更新编辑项。token 三态：缺省/null=保留现有，空串=清空，非空=设置（不回显）。 */
export interface UpdatePatch {
  enabled: boolean;
  repo: string;
  api_base_url: string;
  restart_mode: string;
  /** 更新通道（stable / prerelease，FR-89）。 */
  channel: string;
  token?: string | null;
}

/**
 * 设置编辑请求体（PATCH /api/v1/settings，支持部分更新）。
 * network_proxy 与 update 两块均可选：只发哪块就只改哪块（设置页只发 network_proxy、系统页只发 update）。
 */
export interface SettingsPatch {
  network_proxy?: NetworkProxyPatch;
  update?: UpdatePatch;
}

// —— 出站代理连通性测试（FR-128，仅管理员） ——

/** 出站代理连通性测试请求体。 */
export interface ProxyTestRequest {
  /** 目标测试 URL（仅接受 http/https scheme）。 */
  url: string;
}

/** 出站代理连通性测试结果。 */
export interface ProxyTestResult {
  /** 是否连通：能收到响应即为 true，连接失败 / 超时为 false。 */
  ok: boolean;
  /** HTTP 响应状态码（仅 ok=true 时有值）。 */
  status?: number;
  /** 往返耗时（毫秒）。 */
  elapsed_ms: number;
  /** 失败原因（仅 ok=false 时有值）。 */
  error?: string;
}

// —— 动态配置面板（FR-106，仅管理员，保存后重启生效；对齐后端 src/api/dynamic_config.rs） ——

/** 上传等限制（limits 节）。max_artifact_size 为 null 表示不额外限制。 */
export interface LimitsConfig {
  max_artifact_size?: number | null;
}

/** 审计日志保留（observability.audit 节）。 */
export interface AuditConfig {
  retention_days: number;
  max_rows: number;
}

/** 使用分析采集（observability.usage 节）。 */
export interface UsageConfig {
  detail_enabled: boolean;
  max_detail_rows: number;
}

/** Prometheus 指标端点（observability.metrics 节）。 */
export interface MetricsConfig {
  enabled: boolean;
  allow_anonymous: boolean;
}

/** 指标时序采集（observability.metrics_timeseries 节）。 */
export interface MetricsTimeseriesConfig {
  enabled: boolean;
  sample_interval_secs: number;
  retention_days: number;
  max_rows: number;
}

/** 漏洞库离线镜像（vuln 节）。 */
export interface VulnConfig {
  enabled: boolean;
  source_base_url: string;
  ecosystems: string[];
  refresh_interval_secs: number;
  download_timeout_secs: number;
}

/** 认证可调标量（auth 节非密钥视图）：会话 TTL / 登录失败阈值 / 锁定时长。绝不含 OIDC / LDAP 密钥。 */
export interface AuthTunables {
  session_ttl_secs: number;
  login_max_failures: number;
  login_lockout_secs: number;
}

/**
 * 动态配置面板载荷（GET / PATCH /api/v1/settings/dynamic，仅管理员）。
 * 各节均为**非密钥**项；改动落库 SQLite、**保存后重启生效**（无现成热替换槽）。
 */
export interface DynamicConfig {
  limits: LimitsConfig;
  audit: AuditConfig;
  usage: UsageConfig;
  metrics: MetricsConfig;
  metrics_timeseries: MetricsTimeseriesConfig;
  vuln: VulnConfig;
  auth: AuthTunables;
}

/** 更新检查结果（对齐 FR-85 既有契约，FR-126 经检查 job 产出 / 留存）。 */
export interface UpdateCheck {
  current_version: string;
  latest_version: string;
  update_available: boolean;
  asset_name: string;
  notes: string;
}

/** 留存的检查结果（GET /api/v1/update/check 只读，不联网，FR-126）。无留存时字段为 null。 */
export interface CachedCheck {
  result: UpdateCheck | null;
  checked_at: number | null;
}

/** 异步更新任务触发响应（POST /update/check·/apply·/rollback，FR-126）：返回 job_id（202）。 */
export interface UpdateJobCreated {
  job_id: string;
}

/** 更新任务类别（FR-126）。 */
export type UpdateKind = 'check' | 'apply' | 'rollback';

/** 更新任务阶段（FR-126）：按阶段反馈，不做字节级假百分比。 */
export type UpdatePhase =
  | 'checking'
  | 'downloading'
  | 'verifying'
  | 'replacing'
  | 'restarting'
  | 'done'
  | 'failed';

/** 更新任务进度快照（GET /api/v1/update/jobs/{id}，FR-126）。 */
export interface UpdateJob {
  job_id: string;
  kind: UpdateKind;
  phase: UpdatePhase;
  current_version: string;
  latest_version?: string;
  check?: UpdateCheck;
  new_version?: string;
  error?: string;
  /** 是否为重启后从状态文件回填的历史终态（重启续看用）。 */
  restarted?: boolean;
}

/** 健康检查响应（GET /health，公开 / 匿名可读；version 为构建版本串，FR-101）。 */
export interface HealthInfo {
  status: string;
  version: string;
  port: number;
}

/** 系统操作响应（POST /api/v1/system/restart、/system/shutdown，仅 Admin，FR-109）。 */
export interface SystemActionResponse {
  status: string;
}

// —— 开源许可（FR-102，公开） ——

/** 依赖类别：运行时 / 开发。 */
export type LicenseKind = 'runtime' | 'dev';

/** 依赖来源生态：Rust crate / 前端 npm 包。 */
export type LicenseSource = 'rust' | 'frontend';

/** 单条依赖的许可归因（对齐后端 src/licenses DTO）。 */
export interface LicenseEntry {
  name: string;
  version: string;
  license: string;
  author: string;
  kind: LicenseKind;
  source: LicenseSource;
}

/** 许可清单汇总（供统计卡）。 */
export interface LicenseSummary {
  total: number;
  runtime: number;
  dev: number;
  licenses: number;
}

/** 开源许可清单（GET /api/v1/licenses，公开）。 */
export interface LicenseManifest {
  /** 是否已由构建期脚本生成；false 表示未生成，页面显空态。 */
  generated: boolean;
  entries: LicenseEntry[];
  summary: LicenseSummary;
}

// —— 统一任务注册表（FR-132，消费 FR-131 后端，仅 Admin） ——

/** 任务类别（与后端 TaskKind 的 snake_case 序列化对齐）。 */
export type TaskKind = 'migration' | 'update' | 'vuln';

/** 任务统一状态（与后端 TaskState 的 snake_case 序列化对齐）。 */
export type TaskState = 'running' | 'paused' | 'succeeded' | 'failed' | 'cancelled';

/**
 * 统一任务记录（GET /api/v1/tasks 列表项；GET /api/v1/tasks/{id} 展平字段，FR-131）。
 * 字段严格对齐后端 task_registry.rs 的 TaskRecord serde 输出。
 */
export interface TaskRecord {
  id: string;
  kind: TaskKind;
  state: TaskState;
  /** 人类可读标签（如「在线拉取迁移」「应用更新」「漏洞库刷新」）；可选。 */
  label?: string;
  /** 登记时刻（Unix 秒）。 */
  started_at: number;
  /** 最近一次状态更新时刻（Unix 秒）。 */
  updated_at: number;
  /** 终态时刻（Unix 秒，未结束为 undefined）。 */
  finished_at?: number;
  /** 失败原因（state === 'failed' 时）。 */
  error?: string;
}

/**
 * 单任务详情（GET /api/v1/tasks/{id}，FR-131）：统一记录展平 + 据 kind 附进度明细。
 * migration / update 进度字段在对应 kind 专表仍有记录时填充；vuln 任务两字段均缺省。
 */
export interface TaskDetailDto extends TaskRecord {
  /** 迁移进度明细（仅 kind==='migration' 且专表仍有记录时填）。 */
  migration?: OnlinePullJob;
  /** 更新进度明细（仅 kind==='update' 且专表仍有记录时填）。 */
  update?: UpdateJob;
}
