// 类型化的管理 API 端点封装：每个函数对应 docs/API.md / src/api 的一个端点。
// 路径与字段严格对齐后端真实契约。

import { ApiError, getToken, request } from './client';
import type {
  ProtectionConfig,
  AclDto,
  ArtifactDetailDto,
  ArtifactDto,
  AuditEntryDto,
  AuditListParams,
  CreateRepositoryRequest,
  CreateTokenResponse,
  CreateUserRequest,
  GroupAclView,
  GroupMemberView,
  GroupView,
  LoginResponse,
  MigrationJobCreated,
  MigrationJobSummary,
  MigrationReport,
  NexusMigrateRequest,
  NexusOfflinePreviewRequest,
  NexusPreviewRequest,
  NexusRepoSummary,
  OfflineRepoSummary,
  OnlineMigrateRequest,
  OnlinePullJob,
  Paginated,
  Permission,
  ProtectionAlertDto,
  ProtectionStatusDto,
  RepoFormat,
  RepositoryDto,
  SearchHit,
  TokenView,
  UpdateRepositoryRequest,
  UpdateUserRequest,
  UsageAnalyticsDto,
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

// —— 用户组管理（仅管理员，FR-49） ——

/** 列出全部用户组。 */
export function listGroups(): Promise<GroupView[]> {
  return request<GroupView[]>('/groups');
}

/** 创建用户组（组名重复 409）。 */
export function createGroup(name: string): Promise<GroupView> {
  return request<GroupView>('/groups', { method: 'POST', body: { name } });
}

/** 删除用户组（级联清成员与组 ACL）。 */
export function deleteGroup(id: string): Promise<void> {
  return request<void>(`/groups/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

/** 列出某组成员。 */
export function listGroupMembers(groupId: string): Promise<GroupMemberView[]> {
  return request<GroupMemberView[]>(`/groups/${encodeURIComponent(groupId)}/members`);
}

/** 把用户加入组（重复加入 409）。 */
export function addGroupMember(groupId: string, userId: string): Promise<void> {
  return request<void>(`/groups/${encodeURIComponent(groupId)}/members`, {
    method: 'POST',
    body: { user_id: userId },
  });
}

/** 把用户移出组。 */
export function removeGroupMember(groupId: string, userId: string): Promise<void> {
  return request<void>(
    `/groups/${encodeURIComponent(groupId)}/members/${encodeURIComponent(userId)}`,
    { method: 'DELETE' },
  );
}

// —— 仓库组 ACL（仅管理员，FR-49） ——

/** 列出某仓库的组 ACL。 */
export function listGroupAcl(repoId: string): Promise<GroupAclView[]> {
  return request<GroupAclView[]>(`/repositories/${encodeURIComponent(repoId)}/group-acl`);
}

/** 对组授予一条仓库 ACL。 */
export function createGroupAcl(
  repoId: string,
  groupId: string,
  permission: Permission,
): Promise<GroupAclView> {
  return request<GroupAclView>(`/repositories/${encodeURIComponent(repoId)}/group-acl`, {
    method: 'POST',
    body: { group_id: groupId, permission },
  });
}

/** 撤销一条组 ACL。 */
export function deleteGroupAcl(repoId: string, aclId: string): Promise<void> {
  return request<void>(
    `/repositories/${encodeURIComponent(repoId)}/group-acl/${encodeURIComponent(aclId)}`,
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

// —— 使用分析数据面板（仅管理员） ——

/** 查询使用分析聚合（访问 / 下载总量、热门制品、仓库用量）。 */
export function usageAnalytics(top?: number): Promise<UsageAnalyticsDto> {
  return request<UsageAnalyticsDto>('/analytics/usage', { query: { top } });
}

// —— 防护配置（FR-79，仅管理员） ——

/** 读取当前生效的防护配置（各防护维度阈值 / 开关 / 难度 / 名单 / WAF 规则）。 */
export function getProtectionConfig(): Promise<ProtectionConfig> {
  return request<ProtectionConfig>('/protection/config');
}

/** 整体替换防护配置，校验通过即时生效、无须重启；返回替换后的配置。 */
export function updateProtectionConfig(config: ProtectionConfig): Promise<ProtectionConfig> {
  return request<ProtectionConfig>('/protection/config', { method: 'PATCH', body: config });
}

// —— 通用制品上传（FR-73） ——

/** 通用上传管理 API 前缀（与 client.ts 的 API_BASE 一致）。 */
const UPLOAD_API_BASE = '/api/v1';

/**
 * 经统一上传端点上传制品（multipart/form-data）。
 *
 * 用 XMLHttpRequest 以支持上传进度回调（fetch 不原生支持上传进度）。
 * `formData` 须含 `file` 文件字段，以及按格式区分的坐标字段
 * （Maven: group_id/artifact_id/version；npm: name/version；Raw: path）。
 * `onProgress` 在每次进度事件回调 0~100 的百分比（不可计算时不回调）。
 */
export function uploadArtifact(
  repoId: string,
  formData: FormData,
  onProgress?: (percent: number) => void,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', `${UPLOAD_API_BASE}/repositories/${encodeURIComponent(repoId)}/upload`);
    const token = getToken();
    if (token) {
      xhr.setRequestHeader('Authorization', `Bearer ${token}`);
    }

    xhr.upload.onprogress = (event) => {
      if (onProgress && event.lengthComputable) {
        onProgress(Math.round((event.loaded / event.total) * 100));
      }
    };

    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve();
        return;
      }
      reject(parseUploadError(xhr));
    };
    xhr.onerror = () => reject(new ApiError(0, 'error', '网络错误，上传失败'));
    xhr.onabort = () => reject(new ApiError(0, 'aborted', '上传已取消'));

    xhr.send(formData);
  });
}

/** 从上传 XHR 的错误响应解析后端统一错误结构，回退到通用文案。 */
function parseUploadError(xhr: XMLHttpRequest): ApiError {
  let code = 'error';
  let message = `上传失败（HTTP ${xhr.status}）`;
  try {
    const data = JSON.parse(xhr.responseText) as {
      error?: { code?: string; message?: string };
    };
    if (data.error) {
      code = data.error.code ?? code;
      message = data.error.message ?? message;
    }
  } catch {
    // 响应体非 JSON：保留默认文案
  }
  return new ApiError(xhr.status, code, message);
}

// —— 审计日志（仅管理员，FR-77） ——

/** 分页查询审计日志（按时间倒序，支持动作 / 仓库 / 主体过滤）。 */
export function listAudit(params: AuditListParams = {}): Promise<Paginated<AuditEntryDto>> {
  return request<Paginated<AuditEntryDto>>('/audit', {
    query: {
      action: params.action,
      target_repo: params.target_repo,
      actor: params.actor,
      offset: params.offset,
      limit: params.limit,
    },
  });
}
// —— 防护状态监控（仅管理员，FR-78） ——

/** 查询防护状态快照（各维度窗内计数、当前封禁 IP 数、最近告警）。 */
export function protectionStatus(): Promise<ProtectionStatusDto> {
  return request<ProtectionStatusDto>('/protection/status');
}

/** 分页查询告警历史（可按维度过滤，按时间倒序）。 */
export function listProtectionAlerts(
  options: { dimension?: string; offset?: number; limit?: number } = {},
): Promise<Paginated<ProtectionAlertDto>> {
  return request<Paginated<ProtectionAlertDto>>('/protection/alerts', {
    query: {
      dimension: options.dimension,
      offset: options.offset,
      limit: options.limit,
    },
  });
}
// —— Nexus 迁移（仅管理员，FR-81；对接 ADR-0006 已有端点） ——

/** 在线预览：枚举源 Nexus 可迁移仓库列表（不搬运制品）。 */
export function previewNexusRepositories(req: NexusPreviewRequest): Promise<NexusRepoSummary[]> {
  return request<NexusRepoSummary[]>('/migrate/nexus/preview', {
    method: 'POST',
    body: req,
  });
}

/** 离线预览：枚举本地 blob store 可迁移内容（按 repo 分组，不搬运 blob 本体）。 */
export function previewNexusOffline(
  req: NexusOfflinePreviewRequest,
): Promise<OfflineRepoSummary[]> {
  return request<OfflineRepoSummary[]>('/migrate/nexus/offline/preview', {
    method: 'POST',
    body: req,
  });
}

/** 执行 proxy 仓库配置创建 + 缓存制品搬运，返回迁移报告。 */
export function migrateNexusProxy(req: NexusMigrateRequest): Promise<MigrationReport> {
  return request<MigrationReport>('/migrate/nexus/proxy/migrate', {
    method: 'POST',
    body: req,
  });
}

/** 执行 hosted 仓库配置创建 + 完整制品搬运，返回迁移报告。 */
export function migrateNexusHosted(req: NexusMigrateRequest): Promise<MigrationReport> {
  return request<MigrationReport>('/migrate/nexus/hosted/migrate', {
    method: 'POST',
    body: req,
  });
}
/**
 * 在线拉取迁移（FR-82 / FR-83）：发起异步任务，立即返回任务句柄（job_id）；
 * 实际搬运在后台运行，进度经 getMigrationJob 轮询。同步阶段失败（未选仓库 / 源不存在 /
 * 凭据未配置 / 源不可达）仍同步返回错误。仅 maven2 hosted 仓库会被拉取。
 */
export function migrateNexusOnline(req: OnlineMigrateRequest): Promise<MigrationJobCreated> {
  return request<MigrationJobCreated>('/migrate/nexus/online/migrate', {
    method: 'POST',
    body: req,
  });
}

/** 查询某在线拉取任务的进度快照（未知 id 返回 404）。 */
export function getMigrationJob(id: string): Promise<OnlinePullJob> {
  return request<OnlinePullJob>(`/migrate/jobs/${encodeURIComponent(id)}`);
}

/** 列出活动 / 近期在线拉取任务（供客户端重连点选续看）。 */
export function listMigrationJobs(): Promise<MigrationJobSummary[]> {
  return request<MigrationJobSummary[]>('/migrate/jobs');
}

/** 对制品路径逐段编码（保留 `/` 分隔，避免破坏 catch-all 路径语义）。 */
function encodePath(path: string): string {
  return path
    .split('/')
    .map((seg) => encodeURIComponent(seg))
    .join('/');
}
