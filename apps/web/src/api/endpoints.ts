// 端点封装：按 api/openapi.yaml 的 0.2.0 管理面路径提供 typed 调用。
// 页面与数据钩子仅依赖此模块，不直接拼 URL。
import {
  deleteProtocolAsset,
  postProtocolForm,
  putProtocolAsset,
  request,
  requestBinary,
} from "./client";
import type {
  AssetOperationInput,
  AssetOperationResponse,
  AcknowledgeAuditAttentionResponse,
  AclEntry,
  AclList,
  AuditAttentionDetail,
  AuditAttentionPage,
  AuditAttentionNotificationList,
  AuditCategory,
  AuditEventDetail,
  AuditEventPage,
  AuditNotificationStatus,
  AuditObservabilitySummary,
  HostMonitoring,
  AuditResult,
  AssetList,
  BatchDeleteAssetsResponse,
  PublishPoliciesBatchResponse,
  BackupLink,
  BackupPackage,
  BackupPackageList,
  BackupPackageMode,
  BackupVerification,
  // 待重启生效的恢复状态（解释"为什么现在导入不了"）。
  BackupRestoreStatus,
  // 恢复演练的受理结果、级别与落库报告。
  BackupDrillAccepted,
  BackupVerificationLevel,
  BackupVerificationList,
  BackupImport,
  BackupPreview,
  BackupDiff,
  // 服务器上"磁盘有包体、登记无记录"的备份包。
  UnmanagedBackupList,
  AdoptBackupsResult,
  BackupImportList,
  BackupUploadSession,
  WriteFreezeState,
  ConnectionStatus,
  EnabledFormats,
  LoginResponse,
  MigrationConflictPolicy,
  OperationsDashboard,
  DownloadClientRanking,
  DownloadGroupedTrendResponse,
  RepositoryDownloadTrendResponse,
  MigrationDiscoverResponse,
  MigrationSourceAuth,
  MigrationPlan,
  MigrationReport,
  MigrationSourceType,
  MigrationTask,
  MigrationTaskList,
  RemoteNexusRepositoryList,
  RemoteNexusRepositoryRequest,
  Repository,
  RepositoryList,
  PinnedRepositoriesResponse,
  PublicRepositoryList,
  RepoFormat,
  RepoType,
  RepoVisibility,
  StatusInfo,
  UpstreamHealthRecent,
  UpstreamHealthSeries,
  UpstreamHealthSourceValue,
  TokenCreated,
  TokenList,
  UsageInfo,
  User,
  UserGroup,
  UserGroupList,
  UserGroupMember,
  UserGroupMemberList,
  CreateUserGroupRequest,
  UpdateUserGroupRequest,
  AddUserGroupMemberRequest,
  UserList,
  UserRole,
  UserStatus,
  MaintenanceRestartResult,
} from "./types";

export interface OperationsObservabilityQuery {
  from?: string;
  to?: string;
}

function operationsObservabilityPath(path: string, query: OperationsObservabilityQuery): string {
  const params = new URLSearchParams();
  if (query.from) params.set("from", query.from);
  if (query.to) params.set("to", query.to);
  const suffix = params.toString();
  return suffix ? `${path}?${suffix}` : path;
}

/** 读取当前节点真实业务指标，不含主机或其他节点数据。 */
export function getOperationsDashboard(
  query: OperationsObservabilityQuery = {},
): Promise<OperationsDashboard> {
  return request<OperationsDashboard>(
    operationsObservabilityPath("/observability/dashboard", query),
  );
}

/** 分组下载趋势的查询参数（契约端点 A）。 */
export interface DownloadTrendQuery extends OperationsObservabilityQuery {
  /** 分组维度：family（UA 归类，默认）或 ip（来源 IP 明文，仅管理员）。 */
  groupBy?: "family" | "ip";
  /** 按仓库名精确过滤；省略为全部仓库。 */
  repo?: string;
}

/**
 * 分组下载趋势与分组总计（契约端点，仅管理员）。
 * points 为稀疏「桶 × 组」时序（缺失桶由前端对齐桶轴补 0），
 * totals 为全窗口按组降序（饼图数据源）。
 */
export function getDownloadTrend(
  query: DownloadTrendQuery = {},
): Promise<DownloadGroupedTrendResponse> {
  const params = new URLSearchParams();
  if (query.from) params.set("from", query.from);
  if (query.to) params.set("to", query.to);
  if (query.groupBy) params.set("groupBy", query.groupBy);
  if (query.repo) params.set("repo", query.repo);
  const suffix = params.toString();
  return request<DownloadGroupedTrendResponse>(
    suffix ? `/observability/downloads/trend?${suffix}` : "/observability/downloads/trend",
  );
}

/**
 * 仓库下载趋势 + 全时段累计下载（当前端点已在服务端使用，但尚未纳入 OpenAPI 路径定义，
 * 因此前端继续自行拼 URL；类型来自 components.schemas.RepositoryDownloadTrendResponse）。
 * 权限与仓库树同级（匿名 public 200 / 匿名 private 401 / 无授权 403）；
 * trend 为服务端补零后的连续桶序列，缺省窗口最近 24 小时。
 */
export function getRepositoryDownloadTrend(
  repoName: string,
  query: OperationsObservabilityQuery = {},
): Promise<RepositoryDownloadTrendResponse> {
  return request<RepositoryDownloadTrendResponse>(
    operationsObservabilityPath(
      `/repositories/${encodeURIComponent(repoName)}/download-trend`,
      query,
    ),
  );
}

/** 下载来源聚合（仅管理员；独立来源口径，IP 明文与 UA 归类可见）。 */
export function getDownloadByClient(
  query: OperationsObservabilityQuery = {},
): Promise<DownloadClientRanking> {
  return request<DownloadClientRanking>(
    operationsObservabilityPath("/observability/downloads/by-client", query),
  );
}

/**
 * 主机监控查询：可选 interfaceName 指定单个网卡。
 * `interface` 是 JS 保留字，故类型里命名为 interfaceName，拼查询串时再映射回契约的 `interface`；
 * 省略（或空串）表示按全部非回环网卡聚合。
 */
export interface HostMonitoringQuery extends OperationsObservabilityQuery {
  /** 网卡名（来自响应 networkInterfaces.name）；省略时按全部非回环网卡聚合。 */
  interfaceName?: string;
}

/** 读取当前实例所在主机的持久化分钟样本；可指定单个网卡（速率图与累计总量都按该网卡）。 */
export function getHostMonitoring(query: HostMonitoringQuery = {}): Promise<HostMonitoring> {
  const params = new URLSearchParams();
  if (query.from) params.set("from", query.from);
  if (query.to) params.set("to", query.to);
  if (query.interfaceName) params.set("interface", query.interfaceName);
  const suffix = params.toString();
  return request<HostMonitoring>(`/observability/host${suffix ? `?${suffix}` : ""}`);
}

export interface Pagination {
  page?: number;
  page_size?: number;
}

/** 实例状态：版本、就绪、是否已初始化、用户数与自举许可。 */
export function getStatus(): Promise<StatusInfo> {
  return request<StatusInfo>("/status");
}

/** 取当前会话对应的用户（OIDC 回调只带回令牌，身份快照需另行取回）。 */
export function currentUser(): Promise<User> {
  return request<User>("/auth/me");
}

/** 读取本进程启动时启用的格式（仅管理员）。 */
export function getEnabledFormats(): Promise<EnabledFormats> {
  return request<EnabledFormats>("/formats/enabled");
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
  patch: { role?: UserRole; status?: UserStatus; webLoginDisabled?: boolean },
): Promise<User> {
  return request<User>(`/users/${id}`, { method: "PATCH", body: patch });
}

export function deleteUser(id: number): Promise<void> {
  return request<void>(`/users/${id}`, { method: "DELETE" });
}

export function changePassword(id: number, password: string): Promise<void> {
  return request<void>(`/users/${id}/password`, { method: "POST", body: { password } });
}

// —— 用户组——
// 八个端点均仅管理员可用；组是 ACL 的主体之一，成员关系由组 × 用户承载。

/** 用户组列表（分页窗口 + total 总数）。 */
export function listUserGroups(params: Pagination = {}): Promise<UserGroupList> {
  return request<UserGroupList>("/user-groups", {
    query: { page: params.page, page_size: params.page_size },
  });
}

/** 新建用户组；组名全局唯一。 */
export function createUserGroup(input: CreateUserGroupRequest): Promise<UserGroup> {
  return request<UserGroup>("/user-groups", { method: "POST", body: input });
}

/** 单个用户组详情。 */
export function getUserGroup(id: number): Promise<UserGroup> {
  return request<UserGroup>(`/user-groups/${id}`);
}

/**
 * 更新用户组；空串表示「不改该字段」。
 *
 * 注意 description 允许改成空串（清掉说明）在语义上与「不改」冲突：契约把空串定义为不改，
 * 故这里不提供清空说明的能力——如需清空应在契约层区分 null 与空串，前端不擅自发明语义。
 */
export function updateUserGroup(id: number, patch: UpdateUserGroupRequest): Promise<UserGroup> {
  return request<UserGroup>(`/user-groups/${id}`, { method: "PATCH", body: patch });
}

/** 删除用户组；组内成员关系与该组在 ACL 中的授权由服务端一并处理。 */
export function deleteUserGroup(id: number): Promise<void> {
  return request<void>(`/user-groups/${id}`, { method: "DELETE" });
}

/** 组成员列表。 */
export function listUserGroupMembers(id: number): Promise<UserGroupMemberList> {
  return request<UserGroupMemberList>(`/user-groups/${id}/members`);
}

/** 把用户加入该组。 */
export function addUserGroupMember(
  id: number,
  input: AddUserGroupMemberRequest,
): Promise<UserGroupMember> {
  return request<UserGroupMember>(`/user-groups/${id}/members`, { method: "POST", body: input });
}

/** 把用户移出该组。 */
export function removeUserGroupMember(id: number, userId: number): Promise<void> {
  return request<void>(`/user-groups/${id}/members/${userId}`, { method: "DELETE" });
}

/** 管理员读取指定用户在 hosted 仓库的发布策略。 */
export interface PublishPolicy {
  userId: number;
  username: string;
  webLoginDisabled: boolean;
  repository: string;
  allowedPrefixes: string[];
  maxAssetsHour: number;
  maxBytesDay: number;
  maxFileBytes: number;
  immutableRelease: boolean;
}

/** 读取发布账号策略与 hosted Release 不可变开关。 */
export function getPublishPolicy(userId: number, repository: string): Promise<PublishPolicy> {
  return request<PublishPolicy>(
    `/users/${userId}/publish-policies/${encodeURIComponent(repository)}`,
  );
}

/**
 * 全量保存发布账号策略；空前缀数组表示允许全部路径。
 *
 * 注意 payload 不含 immutableRelease：该字段是仓库级配置（只读展示），单仓库与批量端点
 * 都对写入返回 400 immutable_release_moved。
 */
export function updatePublishPolicy(
  userId: number,
  repository: string,
  policy: PublishPolicyBatchPayload,
): Promise<PublishPolicy> {
  return request<PublishPolicy>(
    `/users/${userId}/publish-policies/${encodeURIComponent(repository)}`,
    { method: "PUT", body: policy },
  );
}

/**
 * 可写入的发布策略字段（单仓库与批量共用）。
 * 不含 immutableRelease——它由仓库配置决定，端点对它的写入一律拒绝。
 */
export type PublishPolicyBatchPayload = Omit<
  PublishPolicy,
  "userId" | "username" | "repository" | "immutableRelease"
>;

/**
 * 把同一份发布策略批量应用到多个 Hosted 仓库。
 *
 * 服务端先对各仓库做统一预校验，任一仓库不存在或非 hosted 会整体拒绝（HTTP 4xx），
 * 因此抛错即表示「没有任何仓库被写入」；返回结果里的逐仓库条目的 ok=false 表示个别仓库
 * 落库失败，调用方应逐条展示原因而不是当成整体成功。
 */
export function updatePublishPolicies(
  userId: number,
  repositories: string[],
  policy: PublishPolicyBatchPayload,
): Promise<PublishPoliciesBatchResponse> {
  return request<PublishPoliciesBatchResponse>(`/users/${userId}/publish-policies`, {
    method: "PUT",
    body: { repositories, ...policy },
  });
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
  description?: string;
  remoteUrl?: string;
  members?: string[];
  /** 仓库别名（可选；与主名共享命名空间、全局唯一，不得等于主名或与他仓主名/别名冲突）。 */
  aliases?: string[];
  /** 仓库存储配额上限（字节）；0 / 缺省表示不限。 */
  quotaBytes?: number;
  /** 仓库制品数配额上限；0 / 缺省表示不限。 */
  quotaAssets?: number;
  /** 代理缓存保留天数（仅 type=proxy 可设）；0 / 缺省表示关闭。 */
  cacheRetentionDays?: number;
}): Promise<Repository> {
  return request<Repository>("/repositories", { method: "POST", body: input });
}

export function updateRepository(
  name: string,
  patch: {
    visibility?: RepoVisibility;
    description?: string;
    remoteUrl?: string;
    members?: string[];
    /** 覆盖式更新别名集合（空数组表示清空；与主名共享命名空间、全局唯一）。 */
    aliases?: string[];
    /** 存储配额上限（字节）；传 0 表示改为不限，缺省表示不修改。 */
    quotaBytes?: number;
    /** 制品数配额上限；传 0 表示改为不限，缺省表示不修改。 */
    quotaAssets?: number;
    /** 代理缓存保留天数（仅 type=proxy 可设）；传 0 表示关闭，缺省表示不修改。 */
    cacheRetentionDays?: number;
  },
): Promise<Repository> {
  return request<Repository>(`/repositories/${name}`, { method: "PATCH", body: patch });
}

/**
 * 重命名仓库（仅管理员）。
 * 成功后旧名自动转为别名——旧链接 / 旧坐标仍可解析到该仓库；返回更新后的仓库。
 * 新名与任何主名或别名冲突时后端返回 409（ApiError.code = "conflict"）。
 */
export function renameRepository(name: string, newName: string): Promise<Repository> {
  return request<Repository>(`/repositories/${encodeURIComponent(name)}/rename`, {
    method: "POST",
    body: { newName },
  });
}

export function deleteRepository(name: string): Promise<void> {
  return request<void>(`/repositories/${name}`, { method: "DELETE" });
}

/** 设置仓库 online/offline 状态（仅管理员；本地运维状态，不参与复制）。 */
export function setRepositoryOnline(name: string, online: boolean): Promise<Repository> {
  return request<Repository>(`/repositories/${name}/online`, { method: "PUT", body: { online } });
}

/** 手动重测仓库上游连接（仅管理员；仅 online proxy 可重测），返回最新状态。 */
export function recheckConnection(name: string): Promise<ConnectionStatus> {
  return request<ConnectionStatus>(`/repositories/${name}/recheck-connection`, {
    method: "POST",
  });
}

/** 上游健康历史的查询参数。from/to 省略时服务端取最近 24 小时。 */
export interface UpstreamHealthQuery {
  from?: string;
  to?: string;
  /** 来源过滤：省略表示 request 与 probe 两路都取；前端应分别展示而不是合并平均。 */
  source?: UpstreamHealthSourceValue;
}

/**
 * 读取仓库上游健康历史（时间序列 + 按来源分组的汇总），仅管理员。
 *
 * `request` 与 `probe` 是两条独立序列：前者反映真实用户可用性，后者反映无流量时的补齐探测。
 * 汇总按来源分别给出，因此「探测 100% 成功但真实请求 60%」这种分化不会被平均掉。
 * 窗口内没有采样时 `points` 为空、`summaries` 为空——调用方必须显示「尚无采样」，
 * 绝不能把它当作 100% 连通。
 */
export function getUpstreamHealth(
  name: string,
  query: UpstreamHealthQuery = {},
): Promise<UpstreamHealthSeries> {
  const params = new URLSearchParams();
  if (query.from) params.set("from", query.from);
  if (query.to) params.set("to", query.to);
  if (query.source) params.set("source", query.source);
  const suffix = params.toString();
  return request<UpstreamHealthSeries>(
    suffix
      ? `/repositories/${encodeURIComponent(name)}/upstream-health?${suffix}`
      : `/repositories/${encodeURIComponent(name)}/upstream-health`,
  );
}

/**
 * 读取最近 N 个分钟桶的上游健康明细摘要，仅管理员。
 *
 * **口径限制**：明细来自分钟桶聚合而非逐请求日志，同一分钟内的多次请求只有一行
 * （`samples` 是该分钟的样本数）。界面文案必须交代这一点，否则会被误读成丢数据。
 */
export function getUpstreamHealthRecent(name: string, limit = 50): Promise<UpstreamHealthRecent> {
  return request<UpstreamHealthRecent>(
    `/repositories/${encodeURIComponent(name)}/upstream-health/recent?limit=${limit}`,
  );
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

/**
 * 拉取仓库全部资产（分页拼合，用于前端拼树）。
 * 单页上限 100（与后端 pageOffset 一致）；超过 maxItems 截断并返回 truncated。
 */
export async function listAllRepositoryAssets(
  name: string,
  opts?: { prefix?: string; maxItems?: number },
): Promise<{ items: AssetList["items"]; total: number; truncated: boolean }> {
  const maxItems = opts?.maxItems ?? 5000;
  const pageSize = 100;
  const all: AssetList["items"] = [];
  let page = 1;
  // total 由循环内每次响应赋值，且 break 之前必定已赋值；初值永远不会被读到，
  // 故不写初始化（写 0 会被 no-useless-assignment 判为无用赋值）。
  let total: number;
  for (;;) {
    const res = await listRepositoryAssets(name, {
      page,
      page_size: pageSize,
      prefix: opts?.prefix,
    });
    total = res.total;
    all.push(...res.items);
    if (all.length >= total || res.items.length === 0 || all.length >= maxItems) {
      break;
    }
    page += 1;
  }
  const items = all.slice(0, maxItems);
  return {
    items,
    total,
    truncated: items.length < total,
  };
}

/** 使用说明：据 format/type 返回 curl/mvn/npm 客户端配置片段。 */
export function getRepositoryUsage(name: string): Promise<UsageInfo> {
  return request<UsageInfo>(`/repositories/${name}/usage`);
}

/** Raw hosted 协议上传（PUT /repository/{name}/{path}）。 */
export function uploadRawAsset(
  repo: string,
  path: string,
  file: File,
): Promise<{ repository: string; path: string; hash: string; size: number; contentType: string }> {
  const enc = path
    .split("/")
    .filter(Boolean)
    .map((s) => encodeURIComponent(s))
    .join("/");
  const url = `/repository/${encodeURIComponent(repo)}/${enc}`;
  return putProtocolAsset(url, file, file.type || "application/octet-stream");
}

/** 协议层删除制品（DELETE /repository/{name}/{path}；元数据删除，blob 内容保留）。 */
export function deleteAsset(repo: string, path: string): Promise<void> {
  const enc = path
    .split("/")
    .filter(Boolean)
    .map((s) => encodeURIComponent(s))
    .join("/");
  const url = `/repository/${encodeURIComponent(repo)}/${enc}`;
  return deleteProtocolAsset(url);
}

/** 管理端批量删除制品（仅管理员；逐条尽力，返回成功数与失败明细）。 */
export function batchDeleteAssets(
  repo: string,
  paths: string[],
): Promise<BatchDeleteAssetsResponse> {
  return request<BatchDeleteAssetsResponse>(
    `/repositories/${encodeURIComponent(repo)}/assets/batch-delete`,
    {
      method: "POST",
      body: { paths, overrideReason: "管理员批量删除制品" },
    },
  );
}

/** 调用统一资产操作事务（删除、移动、重命名）。 */
export function applyAssetOperation(
  repo: string,
  input: AssetOperationInput,
): Promise<AssetOperationResponse> {
  return request<AssetOperationResponse>(
    `/repositories/${encodeURIComponent(repo)}/assets/operations`,
    {
      method: "POST",
      body: {
        ...input,
        overrideReason: input.overrideReason?.trim() || "管理员资产操作",
      },
    },
  );
}

/** Maven 网页上传表单字段。 */
export interface MavenUploadForm {
  groupId: string;
  artifactId: string;
  version: string;
  packaging: string;
  file: File;
}

/** Maven 网页上传（POST multipart，服务端生成 pom.xml/.md5/.sha1/maven-metadata.xml）。 */
export function uploadMavenArtifact(
  repo: string,
  form: MavenUploadForm,
): Promise<{
  repository: string;
  groupId: string;
  artifactId: string;
  version: string;
  files: string[];
}> {
  const fd = new FormData();
  fd.set("groupId", form.groupId);
  fd.set("artifactId", form.artifactId);
  fd.set("version", form.version);
  fd.set("packaging", form.packaging);
  fd.set("file", form.file);
  return postProtocolForm(`/api/v1/repositories/${encodeURIComponent(repo)}/maven-upload`, fd);
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
    sourceAuth?: MigrationSourceAuth;
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
  input: RemoteNexusRepositoryRequest,
  opts?: { signal?: AbortSignal },
): Promise<RemoteNexusRepositoryList> {
  return request<RemoteNexusRepositoryList>("/migrations/remote-repositories", {
    method: "POST",
    body: {
      sourceConfig: input.sourceConfig,
      credentialRef: input.credentialRef || undefined,
      sourceAuth: input.sourceAuth,
    },
    signal: opts?.signal,
  });
}

/** 修改迁移任务来源配置（仅非终态）：allowPrivateSource 用于放行可信内网/本机来源。 */
export function updateMigrationSourceConfig(
  id: number,
  input: { allowPrivateSource: boolean },
): Promise<MigrationTask> {
  return request<MigrationTask>(`/migrations/${id}/source-config`, {
    method: "PATCH",
    body: input,
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

// —— 节点备份与搬迁——
// 备份包是搬迁的传输单位：生成 → 取签名链接 → 新机拉取 → 导入。

/** 备份包列表（分页）。 */
export function listBackups(params: Pagination = {}): Promise<BackupPackageList> {
  return request<BackupPackageList>("/backups", {
    query: { page: params.page, page_size: params.page_size },
  });
}

export function getBackup(packageId: string): Promise<BackupPackage> {
  return request<BackupPackage>(`/backups/${encodeURIComponent(packageId)}`);
}

/** 发起备份生成。生成耗时与包体积同阶，返回后需轮询列表看进度。 */
export function createBackup(input: {
  mode: BackupPackageMode;
  label?: string;
}): Promise<BackupPackage> {
  return request<BackupPackage>("/backups", { method: "POST", body: input });
}

export function deleteBackup(packageId: string): Promise<void> {
  return request<void>(`/backups/${encodeURIComponent(packageId)}`, { method: "DELETE" });
}

/** 签发带时效的下载链接，供新机器直接拉取。 */
export function createBackupLink(packageId: string, ttlSeconds?: number): Promise<BackupLink> {
  return request<BackupLink>(`/backups/${encodeURIComponent(packageId)}/link`, {
    method: "POST",
    body: ttlSeconds ? { ttlSeconds } : {},
  });
}

/** 校验备份包完整性；deep 会逐 blob 比对内容摘要，耗时与包体积同阶。 */
export function verifyBackup(packageId: string, deep = false): Promise<BackupVerification> {
  return request<BackupVerification>(`/backups/${encodeURIComponent(packageId)}/verify`, {
    method: "POST",
    query: { deep: deep ? "true" : "false" },
    // 深度校验可能持续数分钟，需放宽请求超时。
    timeoutMs: deep ? 30 * 60 * 1000 : undefined,
  });
}

// —— 恢复演练与可查报告——
// 与 verifyBackup 的分工：verify 只校验归档自身、结论一次性返回；
// drill 额外真解包 + 真查 SQLite，回答"这份包真的能恢复吗"，并留下可查历史报告。

/**
 * 发起一次恢复演练，返回 202 受理结果（**不代表已完成**）。
 *
 * 深校验要逐 blob 读取，耗时与包体积同阶，故服务端受理后异步执行；
 * 调用方随后轮询 listBackupVerifications 直到新报告出现。
 * 演练未通过不算请求失败——失败同样会落库为一份 ok=false 的报告。
 */
export function drillBackup(
  packageId: string,
  level: BackupVerificationLevel = "deep",
): Promise<BackupDrillAccepted> {
  return request<BackupDrillAccepted>(`/backups/${encodeURIComponent(packageId)}/drill`, {
    method: "POST",
    body: { level },
  });
}

/** 某包的恢复演练报告列表（分页，最近优先）。演练发起后从这里轮询结果。 */
export function listBackupVerifications(
  packageId: string,
  params: Pagination = {},
): Promise<BackupVerificationList> {
  return request<BackupVerificationList>(
    `/backups/${encodeURIComponent(packageId)}/verifications`,
    { query: { page: params.page, page_size: params.page_size } },
  );
}

// —— 写入冻结窗口——
// 搬迁切换用：冻结 → 生成差包/导入 → 起服 → 解冻。窗口必须有界，不允许无限期。

/** 查询当前写入冻结状态。 */
export function getWriteFreeze(): Promise<WriteFreezeState> {
  return request<WriteFreezeState>("/maintenance/freeze");
}

/** 冻结节点写入；传 until（绝对时间）或 ttlSeconds（相对秒数），二者都缺省时用默认 7200。 */
export function freezeWrites(input: {
  until?: string;
  ttlSeconds?: number;
  reason?: string;
}): Promise<WriteFreezeState> {
  return request<WriteFreezeState>("/maintenance/freeze", { method: "POST", body: input });
}

/** 解冻节点写入（幂等）。 */
export function unfreezeWrites(): Promise<WriteFreezeState> {
  return request<WriteFreezeState>("/maintenance/freeze", { method: "DELETE" });
}

/**
 * 一键重启服务。
 *
 * 响应**先于**重启发出（服务端延迟约 2 秒才开始重启），所以这里能正常拿到 202；
 * 之后连接会断，调用方需自行探活。
 *
 * 服务端的执行路径是**进程自替换**（`syscall.Exec` 换新映像），不执行任何外部命令、
 * 不需要任何额外权限；`command` 是**手工重启的等效命令**（给用户复制执行），不是本进程会跑的命令。
 *
 * `supported:false` 不是异常：它表示本节点无法自动重启（平台不支持自替换 / argv[0] 不可执行 /
 * 容器编排形态 / 配置非法），HTTP 仍是 200，`command` 给出可复制手动执行的等效命令。
 */
export function restartService(): Promise<MaintenanceRestartResult> {
  return request<MaintenanceRestartResult>("/maintenance/restart", { method: "POST" });
}

/**
 * 只读探测重启能力（**绝不触发重启**）。
 *
 * 为什么不能用 `restartService` 代替：那个 POST 在 `supported:true` 时会**当场排定重启**
 * （服务端 `RestartAfter`），拿它探测等于"打开页面就重启"。本端点只跑同步预检并回包，
 * 因此可安全用于挂载即取——界面据此显示**本机真正可用**的那条命令。
 *
 * 这条**手工重启**命令随部署形态与运行身份而变（systemd-user / 系统级 root / 系统级非 root /
 * docker），只有后端知道本机该敲哪条；界面硬编码一条会在部分部署上给出照做必失败的指引。
 */
export function getRestartCapability(): Promise<MaintenanceRestartResult> {
  return request<MaintenanceRestartResult>("/maintenance/restart", { method: "GET" });
}

// —— 从 URL 导入备份包——
// 仅接受 http/https，由服务端拉取；校验/暂存完成后需重启服务替换。

/** 从 URL 拉取并导入备份包；返回受理记录（202），导入进度随后由列表轮询可见。 */
export function importBackupFromURL(input: {
  sourceUrl: string;
  overwrite?: boolean;
  deep?: boolean;
  expectedSha256?: string;
}): Promise<BackupImport> {
  return request<BackupImport>("/backups/import", { method: "POST", body: input });
}

/** 导入记录列表（分页，最近优先）。 */
export function listBackupImports(params: Pagination = {}): Promise<BackupImportList> {
  return request<BackupImportList>("/backups/imports", {
    query: { page: params.page, page_size: params.page_size },
  });
}

/** 导入记录详情；id 为 importId。 */
export function getBackupImport(id: string): Promise<BackupImport> {
  return request<BackupImport>(`/backups/imports/${encodeURIComponent(id)}`);
}

/**
 * 是否存在待重启生效的备份恢复。
 *
 * 导入/上传成功后只写 restore.pending，真正的数据库替换在下次启动完成；在那之前
 * 任何后续导入都会被 409 restore_pending 拒绝。这个查询让界面能解释"为什么导入不了"——
 * 此前该状态只体现在导入记录行的徽章上，徽章被更近的事件挤出视野后就再也看不出原因。
 */
export function getBackupRestoreStatus(): Promise<BackupRestoreStatus> {
  return request<BackupRestoreStatus>("/backups/restore-status");
}

/**
 * 用本机已有的备份包恢复本机数据。
 *
 * 补的是"页面上明明列着可用的包，却没有任何按钮能用它恢复"这个缺口——另外三条通道
 * （URL 拉取 / 上传 / CLI）都要求包从外部来，用户因此会得出"只能备份、不能恢复"的结论。
 * 语义与它们完全一致：202 受理 → 轮询导入记录 → `pending_restart` 表示需重启服务生效。
 */
export function restoreFromLocalPackage(
  packageId: string,
  opts: { overwrite?: boolean; deep?: boolean } = {},
): Promise<BackupImport> {
  return request<BackupImport>(`/backups/${encodeURIComponent(packageId)}/restore`, {
    method: "POST",
    body: { overwrite: opts.overwrite, deep: opts.deep },
  });
}

// —— 分片上传备份包（第三通道）——
// 浏览器内传 GB 级包：init 取服务端 chunkSize → 按 File.slice(chunkSize) 逐片 PUT → complete 触发导入。
// 支持续传（GET 拉回 uploadedChunks，只补缺失片）与取消（abort 置 aborted 并清分片）。

/** 发起分片上传会话；返回服务端决定的 chunkSize，前端据此切分。 */
export function createBackupUpload(input: {
  fileName: string;
  totalBytes: number;
  sha256?: string;
}): Promise<BackupUploadSession> {
  return request<BackupUploadSession>("/backups/uploads", { method: "POST", body: input });
}

/** 上传单个分片：请求体为原始字节（octet-stream），经 requestBinary 绕过 JSON 序列化。 */
export function uploadBackupChunk(
  uploadId: string,
  index: number,
  body: Blob | File,
  opts?: { signal?: AbortSignal },
): Promise<BackupUploadSession> {
  return requestBinary<BackupUploadSession>(
    `/backups/uploads/${encodeURIComponent(uploadId)}/chunks/${index}`,
    { method: "PUT", body, signal: opts?.signal },
  );
}

/** 查询上传会话（续传用）；abort 后仍返回 200 + status=aborted（审计保留，非 404）。 */
export function getBackupUpload(uploadId: string): Promise<BackupUploadSession> {
  return request<BackupUploadSession>(`/backups/uploads/${encodeURIComponent(uploadId)}`);
}

/**
 * 拼装归档并存入备份列表；返回一条已完成回执（200）。
 *
 * 追加八起语义已改变：上传**不再**直接发起恢复，只把包收进列表。
 * 恢复是之后的独立动作（走 restoreFromLocalPackage），因此不再需要 overwrite；
 * deep 同样不再在此时生效——校验发生在用户显式发起恢复或深度校验时。
 */
export function completeBackupUpload(
  uploadId: string,
  input?: { sha256?: string },
): Promise<BackupImport> {
  return request<BackupImport>(`/backups/uploads/${encodeURIComponent(uploadId)}/complete`, {
    method: "POST",
    body: input ?? {},
  });
}

/** 取消上传会话：置 aborted 并清分片；返回 204。 */
export function abortBackupUpload(uploadId: string): Promise<void> {
  return request<void>(`/backups/uploads/${encodeURIComponent(uploadId)}/abort`, {
    method: "POST",
  });
}

/**
 * 查看备份包内容概要。
 *
 * 恢复不可撤销，而此前只能看到"多少制品"这类抽象计数；本接口让用户先看清
 * 包里到底有哪些仓库、账号与制品路径，再决定要不要用它覆盖本机数据。
 *
 * 纯只读：服务端把包内快照解到临时目录、以只读方式查询后立即删除。
 */
export function previewBackup(packageId: string): Promise<BackupPreview> {
  return request<BackupPreview>(`/backups/${encodeURIComponent(packageId)}/preview`);
}

/**
 * 对比包内容与当前系统。
 *
 * 不含 scope/kind 时返回四类计数（summary 模式，服务端只跑聚合查询）；
 * 指定 scope+kind 时返回该分类的分页明细。
 *
 * 注意：只对比**元数据**（仓库/路径/大小/哈希），不比对文件内容——
 * 逐字节验证 20GB 不现实，那是「深度校验」的职责。
 * 服务端会把包内快照缓存在临时目录，因此翻页不会重复触发解包。
 */
export function getBackupDiff(
  packageId: string,
  opts: {
    scope?: "summary" | "repositories" | "assets" | "users";
    kind?: string;
    page?: number;
    pageSize?: number;
  } = {},
): Promise<BackupDiff> {
  const q = new URLSearchParams();
  if (opts.scope) q.set("scope", opts.scope);
  if (opts.kind) q.set("kind", opts.kind);
  if (opts.page) q.set("page", String(opts.page));
  if (opts.pageSize) q.set("pageSize", String(opts.pageSize));
  const qs = q.toString();
  return request<BackupDiff>(`/backups/${encodeURIComponent(packageId)}/diff${qs ? `?${qs}` : ""}`);
}

// —— 未管理备份包——
// 服务器 backups/ 下"磁盘有包体、登记表无记录"的包（历史残留、手工拷入）。
// 它们在常规列表里不可见，既不能用也不能删；这里补上"列出 + 导入"。

/** 列出服务器上未被管理的备份包（按文件名升序）。 */
export function listUnmanagedBackups(): Promise<UnmanagedBackupList> {
  return request<UnmanagedBackupList>("/backups/unmanaged");
}

/**
 * 把选中的未管理包导入管理列表（支持单选与多选）。
 *
 * 返回 200 与逐项结果：`adopted` 成功、`skipped` 未执行（已登记/非法包）、
 * 二者皆 false 为失败。单个包失败不影响其余。
 */
export function adoptBackups(fileIds: string[]): Promise<AdoptBackupsResult> {
  return request<AdoptBackupsResult>("/backups/adopt", {
    method: "POST",
    body: { fileIds },
  });
}

// —— 运维端点（非契约）——

/** 清理 Maven 仓库中无 jar 的 GAV 目录（仅 maven hosted）。 */
export function cleanupEmptyArtifacts(repoName: string): Promise<{ deleted: number }> {
  return request<{ deleted: number }>(`/repositories/${repoName}/cleanup`, { method: "POST" });
}

/**
 * 公开仓库列表（无需认证，仅返回 visibility=public 的仓库）。
 * 响应携带 `pinnedNames`（全局置顶且匿名可读的仓库名），使公开页也能展示置顶。
 */
export function listPublicRepositories(): Promise<PublicRepositoryList> {
  return request<PublicRepositoryList>("/public/repositories");
}

// —— 置顶仓库（用户级 + 全局兜底，见迁移 0043）——

/**
 * 当前用户的置顶仓库：登录用户读自己的（服务端持久化，跨设备一致）；
 * 匿名请求回退全局置顶，使未登录浏览也有稳定的置顶顺序。
 */
export function getMyPinnedRepositories(): Promise<PinnedRepositoriesResponse> {
  return request<PinnedRepositoriesResponse>("/me/pinned-repositories");
}

/** 覆盖式写入当前用户的置顶集合（仅登录用户；空数组即取消全部置顶）。 */
export function putMyPinnedRepositories(
  repositoryIds: number[],
): Promise<PinnedRepositoriesResponse> {
  return request<PinnedRepositoriesResponse>("/me/pinned-repositories", {
    method: "PUT",
    body: { repositoryIds },
  });
}

/** 读取全局置顶仓库（仅管理员）。 */
export function getGlobalPinnedRepositories(): Promise<PinnedRepositoriesResponse> {
  return request<PinnedRepositoriesResponse>("/settings/pinned-repositories");
}

/** 覆盖式写入全局置顶仓库（仅管理员）。 */
export function putGlobalPinnedRepositories(
  repositoryIds: number[],
): Promise<PinnedRepositoriesResponse> {
  return request<PinnedRepositoriesResponse>("/settings/pinned-repositories", {
    method: "PUT",
    body: { repositoryIds },
  });
}

// —— : 匿名访问全局开关（admin）——

/** 查询实例级匿名访问开关。 */
export function getAnonymousAccessSetting(): Promise<{ enabled: boolean }> {
  return request<{ enabled: boolean }>("/settings/anonymous-access");
}

/** 更新实例级匿名访问开关（仅 admin）。 */
export function putAnonymousAccessSetting(enabled: boolean): Promise<{ enabled: boolean }> {
  return request<{ enabled: boolean }>("/settings/anonymous-access", {
    method: "PUT",
    body: { enabled },
  });
}

// —— : 基础设置（admin，非契约）——

export interface SettingsConfig {
  /** 匿名访问全局开关。 */
  anonymousAccess: boolean;
  /** 对外基础 URL（对外访问入口，空 = 未配置，回退请求推断）。 */
  publicUrl: string;
  /** 回源整体超时（秒）。 */
  upstreamTimeout: number;
  /** 同步轮询间隔（秒）。 */
  syncInterval: number;
  /** 允许访问的域名白名单（空数组 = 不限制；仅白名单内的 Host 可访问，节点本地）。 */
  allowedHosts: string[];
  /** 回源 Token 校验开关（节点本地）。 */
  originTokenEnabled: boolean;
  /** 回源 Token 请求头名（CDN 回源时注入）。 */
  originTokenHeader: string;
  /** 回源 Token 值（粘贴到 CDN 回源请求头规则）。 */
  originTokenValue: string;
  /** 自动备份间隔（秒），0 = 关闭（运行时可改，保存后作业下一轮生效）。 */
  backupAutoInterval: number;
  /** 保留最近 N 份自动备份，0 = 不按份数限制。 */
  backupKeepLast: number;
  /** 保留最近 N 天的自动备份，0 = 不限天数。 */
  backupRetentionDays: number;
  /**
   * 间隔的人类表达，如「1 天」「5 分钟」。
   * 由后端格式化，界面直接展示，避免前端重复实现一套渲染规则。
   */
  backupAutoIntervalExpr: string;
  /** 间隔拆解后的数值部分，供"数值 + 单位"控件回填。 */
  backupAutoIntervalValue: number;
  /** 间隔拆解后的单位标识（seconds/minutes/hours/days/weeks/months/years）。 */
  backupAutoIntervalUnit: string;
  /**
   * 最近一次成功备份包的大小（字节）。
   *
   * 备份是**全量**包，每份约等于数据目录大小，因此「份数 × 本值」就是最坏情况的
   * 磁盘占用。界面据此提示容量风险——没有它，用户无法判断"保留 7 份"会不会写满盘。
   * 0 表示暂无已完成备份，界面不显示估算。
   */
  backupLatestPackageBytes: number;
}

/**
 * 归一化设置响应：旧后端 / 旧 devmock 场景可能缺少
 * allowedHosts 与 originToken* 字段（/130 后新增），补齐默认值，
 * 避免表单层对 undefined 调 join 等方法崩溃。
 */
function normalizeSettings(raw: SettingsConfig): SettingsConfig {
  return {
    anonymousAccess: raw.anonymousAccess ?? false,
    publicUrl: raw.publicUrl ?? "",
    upstreamTimeout: raw.upstreamTimeout ?? 30,
    syncInterval: raw.syncInterval ?? 5,
    allowedHosts: Array.isArray(raw.allowedHosts) ? raw.allowedHosts : [],
    originTokenEnabled: raw.originTokenEnabled ?? false,
    originTokenHeader: raw.originTokenHeader ?? "",
    originTokenValue: raw.originTokenValue ?? "",
    // 三项备份配置同样做兜底：0 是有意义的取值（关闭 / 不限制），
    // 因此只能用 ?? 判 undefined，不能写成 `|| 7` 之类会把 0 变成默认值的写法。
    backupAutoInterval: raw.backupAutoInterval ?? 0,
    backupKeepLast: raw.backupKeepLast ?? 7,
    backupRetentionDays: raw.backupRetentionDays ?? 30,
    // 旧后端可能不返回这三个派生字段：按秒回落，界面仍可用（只是不体现单位换算）。
    backupAutoIntervalExpr: raw.backupAutoIntervalExpr ?? "",
    backupAutoIntervalValue: raw.backupAutoIntervalValue ?? raw.backupAutoInterval ?? 0,
    backupAutoIntervalUnit: raw.backupAutoIntervalUnit ?? "seconds",
    backupLatestPackageBytes: raw.backupLatestPackageBytes ?? 0,
  };
}

/** 读取实例级基础配置（匿名开关 / 对外 URL / 回源超时 / 同步间隔 / 域名白名单 / 回源 Token，仅 admin）。 */
export function getSettings(): Promise<SettingsConfig> {
  return request<SettingsConfig>("/settings").then(normalizeSettings);
}

/** 部分更新实例级基础配置（仅 admin，传哪个改哪个，写后运行时生效）。 */
export function putSettings(patch: Partial<SettingsConfig>): Promise<SettingsConfig> {
  return request<SettingsConfig>("/settings", { method: "PUT", body: patch }).then(
    normalizeSettings,
  );
}

// —— 开源协议清单（admin 专属，非契约）——

export interface LicenseEntry {
  name: string;
  version: string;
  license: string;
  author: string;
}

export interface LicenseManifest {
  generatedAt: string;
  go: LicenseEntry[];
  npm: LicenseEntry[];
}

/** 依赖协议清单：由后端内嵌 JSON 返回（仅管理员；不再打进前端 bundle）。 */
export function getLicenses(): Promise<LicenseManifest> {
  return request<LicenseManifest>("/licenses");
}

export interface AuditLogEntry {
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
  sourceNode?: string;
}

export interface AuditLogList {
  items: AuditLogEntry[];
  total: number;
}

/** 审计日志查询参数（全可选）。 */
export interface AuditLogQuery {
  actor?: string;
  userId?: string;
  authSource?: string;
  tokenId?: string;
  action?: string;
  repo?: string;
  result?: string;
  ip?: string;
  from?: string;
  to?: string;
  limit?: number;
  offset?: number;
}

/** 审计日志（分页 + 筛选，仅管理员）。 */
export function getAuditLogs(q: AuditLogQuery = {}): Promise<AuditLogList> {
  const params = new URLSearchParams();
  if (q.actor) params.set("actor", q.actor);
  if (q.userId) params.set("userId", q.userId);
  if (q.authSource) params.set("authSource", q.authSource);
  if (q.tokenId) params.set("tokenId", q.tokenId);
  if (q.action) params.set("action", q.action);
  if (q.repo) params.set("repo", q.repo);
  if (q.result) params.set("result", q.result);
  if (q.ip) params.set("ip", q.ip);
  if (q.from) params.set("from", q.from);
  if (q.to) params.set("to", q.to);
  params.set("limit", String(q.limit ?? 50));
  params.set("offset", String(q.offset ?? 0));
  return request<AuditLogList>(`/audit-logs?${params.toString()}`);
}

/** 统一审计观测筛选。category/result 可多选；时间为空时由服务端固定为最近 24 小时。 */
export interface AuditObservabilityQuery {
  from?: string;
  to?: string;
  category?: AuditCategory[];
  result?: AuditResult[];
  actor?: string;
  repository?: string;
  /** 关键字：服务端按事件 ID / 动作 / 摘要 / 目标 / 操作者匹配。 */
  q?: string;
  /** 风险状态：pending（未确认批次）/ acknowledged（已确认批次）。 */
  attention?: "pending" | "acknowledged";
  /** HTTP 请求方法（GET/POST/...）。 */
  method?: string;
  /** 操作名称（精确匹配）。 */
  action?: string;
  /** 客户端 IP 前缀。 */
  clientIp?: string;
  /** 认证方式（jwt / api_key / web / system）。 */
  authSource?: string;
  /** 显式偏移：分页器直达指定页（优先级高于 cursor）。 */
  offset?: number;
}

function auditObservabilityPath(
  path: string,
  query: AuditObservabilityQuery & {
    snapshot?: string;
    cursor?: string;
    limit?: number;
    status?: string;
  },
): string {
  const params = new URLSearchParams();
  if (query.from) params.set("from", query.from);
  if (query.to) params.set("to", query.to);
  for (const category of query.category ?? []) params.append("category", category);
  for (const result of query.result ?? []) params.append("result", result);
  if (query.actor) params.set("actor", query.actor);
  if (query.repository) params.set("repository", query.repository);
  if (query.q) params.set("q", query.q);
  if (query.attention) params.set("attention", query.attention);
  if (query.method) params.set("method", query.method);
  if (query.action) params.set("action", query.action);
  if (query.clientIp) params.set("clientIp", query.clientIp);
  if (query.authSource) params.set("authSource", query.authSource);
  if (query.offset !== undefined) params.set("offset", String(query.offset));
  if (query.snapshot) params.set("snapshot", query.snapshot);
  if (query.cursor) params.set("cursor", query.cursor);
  if (query.limit !== undefined) params.set("limit", String(query.limit));
  if (query.status) params.set("status", query.status);
  const suffix = params.toString();
  return suffix ? `${path}?${suffix}` : path;
}

/** 读取当前节点统一审计概览，并取得与事件页共享的稳定快照。 */
export function getAuditSummary(
  query: AuditObservabilityQuery = {},
): Promise<AuditObservabilitySummary> {
  return request<AuditObservabilitySummary>(
    auditObservabilityPath("/observability/audit/summary", query),
  );
}

/** 按服务端快照分页读取安全审计事件，客户端不得解析 snapshot 或 cursor。 */
export function listAuditEvents(
  query: AuditObservabilityQuery & {
    snapshot?: string;
    cursor?: string;
    limit?: number;
    /** 是否包含 replication 同步记录（默认 false：只列审计事件）。 */
    includeReplication?: boolean;
  } = {},
): Promise<AuditEventPage> {
  return request<AuditEventPage>(auditObservabilityPath("/observability/audit/events", query));
}

/** 按稳定审计快照分页读取服务端权威的风险关注批次。 */
export function listAuditAttentions(
  query: AuditObservabilityQuery & { snapshot?: string; cursor?: string; limit?: number } = {},
): Promise<AuditAttentionPage> {
  return request<AuditAttentionPage>(
    auditObservabilityPath("/observability/audit/attentions", query),
  );
}

/** 读取一条只含脱敏详情的统一审计事件。 */
export function getAuditEvent(eventId: string): Promise<AuditEventDetail> {
  return request<AuditEventDetail>(`/observability/audit/events/${encodeURIComponent(eventId)}`);
}

/** 读取服务端签发的风险关注批次详情；attentionId 不由客户端拼装。 */
export function getAuditAttention(
  attentionId: string,
  query: { cursor?: string; limit?: number } = {},
): Promise<AuditAttentionDetail> {
  return request<AuditAttentionDetail>(
    auditObservabilityPath(
      `/observability/audit/attention/${encodeURIComponent(attentionId)}`,
      query,
    ),
  );
}

/** 原子确认整个服务端风险批次；重复确认保留第一次的确认身份快照。 */
export function acknowledgeAuditAttention(
  attentionId: string,
): Promise<AcknowledgeAuditAttentionResponse> {
  return request<AcknowledgeAuditAttentionResponse>(
    "/observability/audit/attention-acknowledgements",
    { method: "PUT", body: { attentionId } },
  );
}

/** 通知查询参数。缺省为页眉口径（最近 24 小时未确认风险预览）。 */
export interface AuditNotificationQuery {
  from?: string;
  to?: string;
  status?: AuditNotificationStatus;
  limit?: number;
  cursor?: string;
}

/** 读取风险通知批次分页：缺省为页眉徽标口径；携带筛选时返回与 status 同口径的分页列表。 */
export function getAuditAttentionNotifications(
  query: AuditNotificationQuery = {},
): Promise<AuditAttentionNotificationList> {
  return request<AuditAttentionNotificationList>(
    auditObservabilityPath("/observability/audit/notifications", query),
  );
}

// ---- Tree API ----

export interface TreeEntry {
  directories: string[];
  files: {
    path: string;
    size: number;
    hash: string;
    sha1?: string;
    md5?: string;
    contentType?: string;
    createdAt?: string;
    updatedAt: string;
    /** 累计完整下载次数（原始口径，公开） */
    downloadCount?: number;
  }[];
}

/** 目录懒加载：获取仓库指定前缀下的目录和文件。 */
export function getRepositoryTree(name: string, prefix?: string): Promise<TreeEntry> {
  return request<TreeEntry>(`/repositories/${encodeURIComponent(name)}/tree`, {
    query: { prefix },
  });
}

// ---- Search API ----

/**
 * 命中精度分层：后端比较「查询里的版本串 T」与「结果里的版本段 V」得出。
 * `none` 表示没有版本信号（既不精确也不近似），UI 不显示精度徽标。
 */
export type SearchPrecision = "exact" | "prerelease" | "substring" | "none";

/** 包节点的协议归类：`file` 是「无坐标」伪包（按仓库归组，没有版本层）。 */
export type SearchPackageKind = "maven" | "npm" | "file";

/** 聚合结果里的单个文件行（`packages[].versions[].files[]` / 伪包的 `files[]`）。 */
export interface SearchPackageFile {
  name: string;
  path: string;
  repository: string;
  size: number;
  /** 新增：详情面板展示的 SHA-1（可能为空串）。 */
  sha1: string;
  updatedAt: string;
  /** 附录 A 的封闭集合：jar / war / tgz / sources / javadoc / tests / pom / packument / metadata / other / checksum。 */
  fileKind: string;
}

/** 包节点下的版本组。 */
export interface SearchPackageVersion {
  version: string;
  precision: SearchPrecision;
  fileCount: number;
  /** 组内文件体积合计。 */
  size: number;
  latestUpdatedAt: string;
  files: SearchPackageFile[];
}

/** 包级聚合节点（`group_by=package` 才有）。 */
export interface SearchPackage {
  /** 展示坐标；无坐标伪包为仓库名。 */
  coordinate: string;
  kind: SearchPackageKind;
  repositories: string[];
  versionCount: number;
  fileCount: number;
  bestPrecision: SearchPrecision;
  latestUpdatedAt: string;
  topVersion: string;
  versions: SearchPackageVersion[];
  /**
   * 仅 `kind=file` 的「无坐标」伪包携带该键：这类条目跳过版本层，文件直接挂在包下。
   * 有坐标的包（maven / npm）后端不输出此键（`omitempty`）。
   */
  files?: SearchPackageFile[];
}

export interface SearchResult {
  items: {
    repository: string;
    path: string;
    size: number;
    hash: string;
    /** 新增的纯追加键：详情面板展示的 SHA-1。 */
    sha1?: string;
    updatedAt: string;
  }[];
  total: number;
  /** 按仓库聚合的命中数（钻取导航用，按命中数降序）。 */
  facets?: { repository: string; count: number }[];
  /**
   * 包节点数（含「无坐标」伪包）；`aggregation.truncated` 时为下界，且排序靠后的包
   * 可能整包缺失。仅 `group_by=package` 有。
   */
  packageTotal?: number;
  /** 包级聚合结果，仅 `group_by=package` 有。 */
  packages?: SearchPackage[];
  /** 聚合扫描口径，仅 `group_by=package` 有；`truncated` 为真表示只聚合了前 `scanned` 条文件。 */
  aggregation?: { scanned: number; truncated: boolean };
  /** 后端回报的实际生效排序（relevance 回落 / 缺省时用来"不猜"）。 */
  appliedOrder?: string;
}

/** 搜索结果的分组维度：缺省 `file`（文件级扁平列表），`package` 走包级聚合。 */
export type SearchGroupBy = "file" | "package";

/** 搜索结果排序方向；`relevance` 仅在 `group_by=package` 下生效（其余情况后端静默回落）。 */
export type SearchOrder = "asc" | "desc" | "relevance";

/** 全局跨仓库制品搜索。 */
export function searchAssets(params: {
  q: string;
  repository?: string;
  sort?: string;
  order?: SearchOrder;
  group_by?: SearchGroupBy;
  page?: number;
  page_size?: number;
  /** 外部中止信号（取消 / 换口径时中止上一次），透传给客户端由 fetch 真实中断。 */
  signal?: AbortSignal;
}): Promise<SearchResult> {
  return request<SearchResult>("/search", {
    query: {
      q: params.q,
      repository: params.repository,
      sort: params.sort,
      order: params.order,
      group_by: params.group_by,
      page: params.page,
      page_size: params.page_size,
    },
    signal: params.signal,
  });
}

// ---- 搜索建议（搜索框下拉候选） ----

/**
 * 候选类型决定确认语义（规格 §3.9）：
 * - `package` → 写入输入框并**直接发起搜索**；
 * - `repository` → 写成 `repo:` 限定、**不立即搜索**（用户可继续追加关键词）。
 */
export type SearchSuggestionType = "package" | "repository";

export interface SearchSuggestion {
  type: SearchSuggestionType;
  /** 候选值：仓库名，或可直接作为搜索词命中该包的包名。 */
  value: string;
}

export interface SearchSuggestResult {
  items: SearchSuggestion[];
}

/**
 * 取搜索框下拉候选（非契约端点 `/api/v1/search/suggest`）。
 *
 * 空 `q`（trim 后）后端返回空 `items` 而不是 400；`limit` 由后端夹取到 [1, 20]。
 * `signal` 供调用方在每次新请求前中止上一次，避免旧响应覆盖新候选（规格 §3.9）。
 */
export function suggestSearch(params: {
  q: string;
  limit?: number;
  signal?: AbortSignal;
}): Promise<SearchSuggestResult> {
  return request<SearchSuggestResult>("/search/suggest", {
    query: { q: params.q, limit: params.limit },
    signal: params.signal,
  });
}

// ---- Sorted repo list ----

export interface RepoListParams extends Pagination {
  sort?: string;
  order?: string;
}

/** 仓库列表（支持排序）。 */
export function listRepositoriesSorted(params: RepoListParams = {}): Promise<RepositoryList> {
  return request<RepositoryList>("/repositories", {
    query: {
      page: params.page,
      page_size: params.page_size,
      sort: params.sort,
      order: params.order,
    },
  });
}

/** 契约允许的单页上限（`page_size` maximum: 100）。 */
export const REPO_PAGE_SIZE_MAX = 100;

/**
 * 拉取**全量**仓库（单页上限 100，超出按页拼接）。
 *
 * 为什么需要它：置顶排序在前端做（要跨页生效）、名称筛选是客户端条件，两者都要求前端持有全量集合；
 * 仪表盘「仓库状态」面板的环形图与「共 N 个仓库」也要按全量统计。各页面各写一遍分页拼接
 * 迟早会漂移（曾经仪表盘写死 `page_size: 50`，大量档下面板说 50、列表说 144），
 * 这里收敛成唯一入口。
 */
export async function listAllRepositories(
  params: { sort?: string; order?: string } = {},
): Promise<RepositoryList> {
  const first = await listRepositoriesSorted({ ...params, page: 1, page_size: REPO_PAGE_SIZE_MAX });
  const items = [...first.items];
  const pages = Math.ceil(first.total / REPO_PAGE_SIZE_MAX);
  for (let next = 2; next <= pages; next += 1) {
    const page = await listRepositoriesSorted({
      ...params,
      page: next,
      page_size: REPO_PAGE_SIZE_MAX,
    });
    items.push(...page.items);
  }
  return { items, total: first.total };
}

// ---- 维护与清理 ----
// 这四组端点都是**非契约端点**（登记在 docs/API.md），不进 api/openapi.yaml：
// 它们服务运维手动排障与清理，形状随清理能力演进。

/** 清理类别：与后端 domain.CleanupKind* 一一对应。 */
export type CleanupKind = "empty_dirs" | "stale_uploads" | "pre_restore" | "quarantine";

export interface StorageHealth {
  orphanBlobs: number;
  orphanBlobBytes: number;
  quarantinedBlobs: number;
  quarantinedBytes: number;
  emptyDirs: number;
  preRestoreDirs: number;
  preRestoreBytes: number;
  backupUploadsBytes: number;
  backupImportsBytes: number;
  backupDrillBytes: number;
  totalBytes: number;
  partialError?: string;
}

export interface CleanupCandidate {
  /** 相对数据目录的路径；对外只暴露相对路径，前端不得拼绝对路径。 */
  path: string;
  isDir: boolean;
  bytes: number;
}

export interface CleanupPreview {
  kind: CleanupKind;
  items: CleanupCandidate[];
  itemCount: number;
  freedBytes: number;
  skipped?: string[];
}

export interface CleanTask {
  ID: number;
  Kind: string;
  Status: string;
  TotalItems: number;
  ProcessedItems: number;
  FreedBytes: number;
  ErrorMessage: string | null;
  CreatedAt: string;
  StartedAt: string | null;
  FinishedAt: string | null;
}

export interface TrashEntry {
  ID: number;
  TaskID: number;
  Kind: string;
  OriginalPath: string;
  TrashPath: string;
  IsDir: boolean;
  Bytes: number;
  Status: string;
  CreatedAt: string;
  ResolvedAt: string | null;
}

export interface TrashConflict {
  id: number;
  originalPath: string;
  reason: string;
}

export interface MaintenanceJob {
  name: string;
  intervalSeconds: number;
  running: boolean;
  runs?: number;
  failures?: number;
  lastStartedAt?: string | null;
  lastFinishedAt?: string | null;
  lastError?: string | null;
}

/** 存储体检快照（只读）。 */
export function getStorageHealth(): Promise<StorageHealth> {
  return request<StorageHealth>("/maintenance/storage-health");
}

/** 清理预览（**只读**，绝不删东西）。 */
export function previewCleanup(kind: CleanupKind, keepLast?: number): Promise<CleanupPreview> {
  return request<CleanupPreview>("/maintenance/cleanup/preview", {
    method: "POST",
    body: { kind, ...(keepLast === undefined ? {} : { keepLast }) },
  });
}

/** 执行清理：只把目标**移入回收站**，不物理删除。 */
export function executeCleanup(input: {
  kind: CleanupKind;
  items?: string[];
  keepLast?: number;
}): Promise<{ taskId: number; status: string }> {
  return request("/maintenance/cleanup", { method: "POST", body: input });
}

/** 清理任务历史与进度。 */
export function listCleanTasks(
  page = 1,
  pageSize = 20,
): Promise<{
  items: CleanTask[];
  total: number;
  page: number;
  pageSize: number;
}> {
  return request("/maintenance/clean-tasks", { query: { page, page_size: pageSize } });
}

/** 回收站列表（含占用统计）。
 *
 * `stats` 用 small-camel 字段（后端已加 json tag 统一）。但页面侧仍必须做兜底：
 * 一旦字段名再漂移，读 undefined 会显示成「项 / NAN KB」而不是崩掉——
 * 这类"半坏"状态比白屏更难发现，所以宁可显式 ?? 默认值。 */
export function listTrash(
  page = 1,
  pageSize = 20,
): Promise<{
  items: TrashEntry[];
  total: number;
  stats?: { entries?: number; bytes?: number };
}> {
  return request("/maintenance/trash", { query: { page, page_size: pageSize } });
}

/** 恢复回收站条目（反向操作；目标已存在时跳过并回 conflicts）。 */
export function restoreTrash(ids: number[]): Promise<{
  restored: number;
  conflicts?: TrashConflict[];
}> {
  return request("/maintenance/trash/restore", { method: "POST", body: { ids } });
}

/** 彻底删除回收站条目（**不可逆**）。 */
export function purgeTrash(ids: number[]): Promise<{ purged: number; freedBytes: number }> {
  return request("/maintenance/trash/purge", { method: "POST", body: { ids } });
}

/** 清空回收站（**不可逆**）。 */
export function purgeAllTrash(): Promise<{ purged: number; freedBytes: number }> {
  return request("/maintenance/trash/purge-all", { method: "POST" });
}

/** 维护作业清单。
 *
 * 注意字段名是 `jobs` 而不是 `items`：本端点沿用后端既有形状（`MaintenanceJobList`），
 * 与其它列表接口的分页形状不同。写成 `items` 会让 `res.items` 为 undefined，
 * 页面读 `.length` 直接白屏——这正是上线时踩到的坑。 */
export function listMaintenanceJobs(): Promise<{ jobs: MaintenanceJob[] }> {
  return request("/maintenance/jobs");
}

/** 手动触发维护作业（**物理删除**，不进回收站）。 */
export function runMaintenanceJob(name: string): Promise<{ name: string; started: boolean }> {
  return request(`/maintenance/jobs/${encodeURIComponent(name)}/run`, { method: "POST" });
}
