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
  BackupLink,
  BackupPackage,
  BackupPackageList,
  BackupPackageMode,
  BackupVerification,
  BackupImport,
  BackupImportList,
  BackupUploadSession,
  WriteFreezeState,
  ConnectionStatus,
  EnabledFormats,
  LoginResponse,
  MigrationConflictPolicy,
  OperationsDashboard,
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

/** 读取当前实例所在主机的持久化分钟样本。 */
export function getHostMonitoring(
  query: OperationsObservabilityQuery = {},
): Promise<HostMonitoring> {
  return request<HostMonitoring>(operationsObservabilityPath("/observability/host", query));
}

export interface Pagination {
  page?: number;
  page_size?: number;
}

/** 实例状态：版本、就绪、是否已初始化、用户数与自举许可。 */
export function getStatus(): Promise<StatusInfo> {
  return request<StatusInfo>("/status");
}

/** FR-32：读取本进程启动时启用的格式（仅管理员）。 */
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

/** FR-109：管理员读取指定用户在 hosted 仓库的发布策略。 */
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

/** FR-109：读取发布账号策略与 hosted Release 不可变开关。 */
export function getPublishPolicy(userId: number, repository: string): Promise<PublishPolicy> {
  return request<PublishPolicy>(
    `/users/${userId}/publish-policies/${encodeURIComponent(repository)}`,
  );
}

/** FR-109：全量保存发布账号策略；空前缀数组表示允许全部路径。 */
export function updatePublishPolicy(
  userId: number,
  repository: string,
  policy: Omit<PublishPolicy, "userId" | "username" | "repository">,
): Promise<PublishPolicy> {
  return request<PublishPolicy>(
    `/users/${userId}/publish-policies/${encodeURIComponent(repository)}`,
    { method: "PUT", body: policy },
  );
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
  },
): Promise<Repository> {
  return request<Repository>(`/repositories/${name}`, { method: "PATCH", body: patch });
}

export function deleteRepository(name: string): Promise<void> {
  return request<void>(`/repositories/${name}`, { method: "DELETE" });
}

/** FR-113：设置仓库 online/offline 状态（仅管理员；本地运维状态，不参与复制）。 */
export function setRepositoryOnline(name: string, online: boolean): Promise<Repository> {
  return request<Repository>(`/repositories/${name}/online`, { method: "PUT", body: { online } });
}

/** FR-114：手动重测仓库上游连接（仅管理员；仅 online proxy 可重测），返回最新状态。 */
export function recheckConnection(name: string): Promise<ConnectionStatus> {
  return request<ConnectionStatus>(`/repositories/${name}/recheck-connection`, {
    method: "POST",
  });
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
  let total = 0;
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

/** FR-102: 协议层删除制品（DELETE /repository/{name}/{path}；元数据删除，blob 内容保留）。 */
export function deleteAsset(repo: string, path: string): Promise<void> {
  const enc = path
    .split("/")
    .filter(Boolean)
    .map((s) => encodeURIComponent(s))
    .join("/");
  const url = `/repository/${encodeURIComponent(repo)}/${enc}`;
  return deleteProtocolAsset(url);
}

/** FR-103: 管理端批量删除制品（仅管理员；逐条尽力，返回成功数与失败明细）。 */
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

/** FR-105：调用统一资产操作事务（删除、移动、重命名）。 */
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

/** FR-73: Maven 网页上传表单字段。 */
export interface MavenUploadForm {
  groupId: string;
  artifactId: string;
  version: string;
  packaging: string;
  file: File;
}

/** FR-73: Maven 网页上传（POST multipart，服务端生成 pom.xml/.md5/.sha1/maven-metadata.xml）。 */
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

// —— 节点备份与搬迁（FR-132）——
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

// —— 写入冻结窗口（FR-134）——
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

// —— 从 URL 导入备份包（FR-137）——
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

// —— 分片上传备份包（FR-137 第三通道）——
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

/** 拼装并触发本地导入；返回受理记录（202），导入进度随后由导入记录列表轮询可见，需重启服务生效。 */
export function completeBackupUpload(
  uploadId: string,
  input?: { sha256?: string; overwrite?: boolean; deep?: boolean },
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

// —— 运维端点（非契约）——

/** 清理 Maven 仓库中无 jar 的 GAV 目录（仅 maven hosted）。 */
export function cleanupEmptyArtifacts(repoName: string): Promise<{ deleted: number }> {
  return request<{ deleted: number }>(`/repositories/${repoName}/cleanup`, { method: "POST" });
}

/** 公开仓库列表（无需认证，仅返回 visibility=public 的仓库）。 */
export function listPublicRepositories(): Promise<RepositoryList> {
  return request<RepositoryList>("/public/repositories");
}

// —— FR-66: 匿名访问全局开关（admin）——

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

// —— FR-89: 基础设置（admin，非契约）——

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
  /** 回源 Token 校验开关（FR-130，节点本地）。 */
  originTokenEnabled: boolean;
  /** 回源 Token 请求头名（CDN 回源时注入）。 */
  originTokenHeader: string;
  /** 回源 Token 值（粘贴到 CDN 回源请求头规则）。 */
  originTokenValue: string;
}

/**
 * 归一化设置响应：旧后端 / 旧 devmock 场景可能缺少
 * allowedHosts 与 originToken* 字段（FR-129/130 后新增），补齐默认值，
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

/** 审计日志查询参数（FR-38，全可选）。 */
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

/** 审计日志（FR-38，分页 + 筛选，仅管理员）。 */
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

/** FR-118：统一审计观测筛选。category/result 可多选；时间为空时由服务端固定为最近 24 小时。 */
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
  query: AuditObservabilityQuery & { snapshot?: string; cursor?: string; limit?: number } = {},
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

/** FR-117：通知查询参数。缺省为页眉口径（最近 24 小时未确认风险预览）。 */
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

// ---- FR-54: Tree API ----

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
    /** FR-142：累计完整下载次数（原始口径，公开） */
    downloadCount?: number;
  }[];
}

/** 目录懒加载：获取仓库指定前缀下的目录和文件。 */
export function getRepositoryTree(name: string, prefix?: string): Promise<TreeEntry> {
  return request<TreeEntry>(`/repositories/${encodeURIComponent(name)}/tree`, {
    query: { prefix },
  });
}

// ---- FR-30: Search API ----

export interface SearchResult {
  items: { repository: string; path: string; size: number; hash: string; updatedAt: string }[];
  total: number;
  /** 按仓库聚合的命中数（钻取导航用，按命中数降序）。 */
  facets?: { repository: string; count: number }[];
}

/** 全局跨仓库制品搜索。 */
export function searchAssets(params: {
  q: string;
  repository?: string;
  sort?: string;
  order?: "asc" | "desc";
  page?: number;
  page_size?: number;
}): Promise<SearchResult> {
  return request<SearchResult>("/search", {
    query: {
      q: params.q,
      repository: params.repository,
      sort: params.sort,
      order: params.order,
      page: params.page,
      page_size: params.page_size,
    },
  });
}

// ---- FR-56: Sorted repo list ----

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
 * 为什么需要它：置顶是本地偏好、名称筛选是客户端条件，两者都要求前端持有全量集合；
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
