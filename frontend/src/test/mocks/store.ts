// 有状态内存 mock 后端的数据 store（FR-116 / FR-119，ADR-0035）。
//
// 纯内存数据结构，不依赖浏览器 / Node：测试（msw/node）与运行时 Mock 模式（msw/browser）
// 共用同一份 store + handlers。提供 reset()（清空回初始）与 seed()（预置示例数据，便于一开即用）。
// 字段严格对齐 src/api/types.ts 与后端契约。

import type {
  AclDto,
  ArtifactDetailDto,
  ArtifactDto,
  AuditEntryDto,
  DashboardSummary,
  DynamicConfig,
  GroupAclView,
  GroupView,
  HostMetrics,
  MetricPoint,
  OnlinePullJob,
  ProtectionConfig,
  ProtectionStatusDto,
  RepositoryDto,
  SettingsView,
  SystemLogEntryDto,
  TokenView,
  UsageAnalyticsDto,
  UserInfo,
  UserView,
} from '../../api/types';

/** 内存用户记录（含口令明文，仅用于 mock 登录校验，绝不进生产）。 */
export interface MockUser extends UserView {
  password: string;
}

/** 内存令牌记录（含明文，仅签发时回显一次）。 */
export interface MockToken extends TokenView {
  /** 仅签发时一次性回显的明文；列表视图不含此字段。 */
  plaintext: string;
}

/** 内存制品记录（浏览索引 + 详情共用底层数据）。 */
export interface MockArtifact {
  repoId: string;
  path: string;
  size: number;
  sha256: string;
  sha1: string;
  md5: string;
  sha512: string;
  contentType: string | null;
  cached: boolean;
  createdAt: string;
}

/** 内存用户组成员关系（组 id → 用户 id 列表）。 */
export interface MockGroupMembers {
  [groupId: string]: string[];
}

/** mock 后端的全部可变状态。 */
export interface MockState {
  users: MockUser[];
  repositories: RepositoryDto[];
  artifacts: MockArtifact[];
  tokens: MockToken[];
  acls: Map<string, AclDto[]>;
  audit: AuditEntryDto[];
  settings: SettingsView;
  /** 用户组集合（FR-49）。 */
  groups: GroupView[];
  /** 组成员关系（组 id → 用户 id 列表）。 */
  groupMembers: Map<string, string[]>;
  /** 仓库组 ACL（仓库 id → 组 ACL 列表）。 */
  groupAcls: Map<string, GroupAclView[]>;
  /** 防护配置（FR-79，整体读写）。 */
  protection: ProtectionConfig;
  /** 动态配置（FR-106，整体读写）。 */
  dynamicConfig: DynamicConfig;
  /** 在线拉取迁移任务（FR-83）。 */
  migrationJobs: OnlinePullJob[];
  /** 系统运行日志（FR-107，tail 最新在前）。 */
  systemLogs: SystemLogEntryDto[];
  /** 当前会话 token → 用户 id 的映射（登录后写入，模拟服务端会话）。 */
  sessions: Map<string, string>;
}

/** 自增 id 计数器（每次 reset 重置，保证用例间确定）。 */
let idCounter = 0;
/** 生成稳定的递增 id（前缀区分实体）。 */
export function nextId(prefix: string): string {
  idCounter += 1;
  return `${prefix}-${idCounter}`;
}

/** 构造一份空白初始状态（仅含必备的内置管理员，无业务数据）。 */
function emptyState(): MockState {
  const admin: MockUser = {
    id: 'u-admin',
    username: 'admin',
    role: 'admin',
    disabled: false,
    created_at: '2026-01-01T00:00:00Z',
    password: 'admin123',
  };
  return {
    users: [admin],
    repositories: [],
    artifacts: [],
    tokens: [],
    acls: new Map(),
    audit: [],
    settings: defaultSettings(),
    groups: [],
    groupMembers: new Map(),
    groupAcls: new Map(),
    protection: defaultProtection(),
    dynamicConfig: defaultDynamicConfig(),
    migrationJobs: [],
    systemLogs: [],
    sessions: new Map(),
  };
}

/** 默认设置视图（脱敏，无凭据回显）。 */
function defaultSettings(): SettingsView {
  return {
    current_version: '0.5.0-mock',
    network_proxy: {
      http: { url: null, username: null, has_password: false },
      https: { url: null, username: null, has_password: false },
      all: { url: null, username: null, has_password: false },
      no_proxy: null,
    },
    update: {
      enabled: false,
      repo: 'example/jianartifact',
      api_base_url: 'https://api.github.com',
      restart_mode: 'self',
      channel: 'stable',
      has_token: false,
      rollback_available: false,
    },
  };
}

/** 默认防护配置（七维度，给关闭 / 合理阈值的默认值，FR-79）。 */
function defaultProtection(): ProtectionConfig {
  return {
    rate_limit: {
      enabled: false,
      window_secs: 60,
      ip_max_requests: 600,
      identity_max_requests: 1200,
      repo_max_requests: 1200,
      ip_max_concurrent: 64,
      user_max_concurrent: 128,
      repo_max_concurrent: 128,
    },
    ip_list: { allow: [], deny: [] },
    ban: { enabled: false, window_secs: 300, threshold: 100, duration_secs: 900 },
    slowloris: {
      enabled: false,
      body_read_timeout_secs: 30,
      header_timeout_secs: 10,
      max_body_bytes: 1024 * 1024 * 1024,
    },
    cc_challenge: { enabled: false, difficulty: 16, ttl_secs: 300, exempt_authenticated: true },
    waf: { enabled: false, rules: [] },
    alerts: {
      enabled: false,
      window_secs: 60,
      rate_limit_warn_threshold: 100,
      ban_warn_threshold: 10,
      cc_challenge_fail_warn_threshold: 50,
      waf_block_warn_threshold: 50,
      slowloris_warn_threshold: 20,
      max_rows: 1000,
    },
  };
}

/** 默认动态配置（各非密钥节给合理默认值，FR-106）。 */
function defaultDynamicConfig(): DynamicConfig {
  return {
    limits: { max_artifact_size: null },
    audit: { retention_days: 90, max_rows: 100_000 },
    usage: { detail_enabled: true, max_detail_rows: 100_000 },
    metrics: { enabled: false, allow_anonymous: false },
    metrics_timeseries: {
      enabled: true,
      sample_interval_secs: 60,
      retention_days: 7,
      max_rows: 100_000,
    },
    vuln: {
      enabled: false,
      source_base_url: 'https://osv-vulnerabilities.storage.googleapis.com',
      ecosystems: ['Maven', 'npm'],
      refresh_interval_secs: 86_400,
      download_timeout_secs: 300,
    },
    auth: { session_ttl_secs: 3600, login_max_failures: 5, login_lockout_secs: 900 },
  };
}

/** 全局单例 state。 */
export const state: MockState = emptyState();

/** 把单例 state 重置回初始空白态（测试 beforeEach 调用，保证用例隔离）。 */
export function reset(): void {
  idCounter = 0;
  const fresh = emptyState();
  state.users = fresh.users;
  state.repositories = fresh.repositories;
  state.artifacts = fresh.artifacts;
  state.tokens = fresh.tokens;
  state.acls = fresh.acls;
  state.audit = fresh.audit;
  state.settings = fresh.settings;
  state.groups = fresh.groups;
  state.groupMembers = fresh.groupMembers;
  state.groupAcls = fresh.groupAcls;
  state.protection = fresh.protection;
  state.dynamicConfig = fresh.dynamicConfig;
  state.migrationJobs = fresh.migrationJobs;
  state.systemLogs = fresh.systemLogs;
  state.sessions = fresh.sessions;
}

/**
 * 预置示例数据（运行时 Mock 模式一开即用 / 测试按需调用）。
 * 先 reset 再填，幂等。
 */
export function seed(): void {
  reset();
  const developer: MockUser = {
    id: nextId('u'),
    username: 'developer',
    role: 'user',
    disabled: false,
    created_at: '2026-02-01T08:00:00Z',
    password: 'dev123',
  };
  state.users.push(developer);

  const mavenHosted: RepositoryDto = {
    id: nextId('r'),
    name: 'maven-releases',
    format: 'maven',
    type: 'hosted',
    visibility: 'public',
    upstream_url: null,
    created_at: '2026-02-02T10:00:00Z',
  };
  const npmProxy: RepositoryDto = {
    id: nextId('r'),
    name: 'npm-proxy',
    format: 'npm',
    type: 'proxy',
    visibility: 'private',
    upstream_url: 'https://registry.npmjs.org',
    created_at: '2026-02-03T10:00:00Z',
  };
  const dockerHosted: RepositoryDto = {
    id: nextId('r'),
    name: 'docker-internal',
    format: 'docker',
    type: 'hosted',
    visibility: 'private',
    upstream_url: null,
    created_at: '2026-02-04T10:00:00Z',
  };
  state.repositories.push(mavenHosted, npmProxy, dockerHosted);

  state.artifacts.push(
    artifact(mavenHosted.id, 'com/example/app/1.0.0/app-1.0.0.jar'),
    artifact(mavenHosted.id, 'com/example/lib/2.1.0/lib-2.1.0.jar'),
    artifact(npmProxy.id, 'left-pad/-/left-pad-1.3.0.tgz', true),
  );

  state.tokens.push({
    id: nextId('t'),
    name: 'ci-pipeline',
    created_at: '2026-02-05T12:00:00Z',
    last_used_at: '2026-02-10T09:00:00Z',
    revoked: false,
    plaintext: 'jart_seed_ci_xxx',
  });

  state.audit.push({
    id: 1,
    ts: '2026-02-05T12:00:00Z',
    actor: 'admin',
    actor_kind: 'user',
    request_id: 'req-seed-1',
    source_ip: '127.0.0.1',
    action: 'repo.create',
    target_repo: 'maven-releases',
    target: null,
    result: 'ok',
    detail: null,
  });

  // 用户组（FR-49）：预置 2 个组，developers 含 developer 成员。
  const developers: GroupView = {
    id: nextId('g'),
    name: 'developers',
    created_at: '2026-02-07T09:00:00Z',
  };
  const releaseManagers: GroupView = {
    id: nextId('g'),
    name: 'release-managers',
    created_at: '2026-02-07T09:30:00Z',
  };
  state.groups.push(developers, releaseManagers);
  state.groupMembers.set(developers.id, [developer.id]);
  state.groupMembers.set(releaseManagers.id, []);
  // 组 ACL：developers 对 maven-releases 有写权限。
  state.groupAcls.set(mavenHosted.id, [
    { id: nextId('gacl'), group_id: developers.id, permission: 'write' },
  ]);

  // 系统运行日志（FR-107，tail 最新在前）：预置几条示例行。
  state.systemLogs.push(
    {
      timestamp: '2026-02-10T09:05:00Z',
      level: 'INFO',
      message: 'jianartifact::server 服务已启动，监听 0.0.0.0:8080',
    },
    {
      timestamp: '2026-02-10T09:06:12Z',
      level: 'INFO',
      message: 'jianartifact::repo 仓库 maven-releases 收到上传请求',
    },
    {
      timestamp: '2026-02-10T09:07:30Z',
      level: 'WARN',
      message: 'jianartifact::proxy 上游 registry.npmjs.org 响应较慢（1200ms）',
    },
  );
}

/** 构造一条内存制品（四校验和用占位但稳定的值）。 */
function artifact(repoId: string, path: string, cached = false): MockArtifact {
  const tag = path.replace(/[^a-z0-9]/gi, '').slice(0, 8) || 'blob';
  return {
    repoId,
    path,
    size: 1024 + path.length,
    sha256: `sha256-${tag}`.padEnd(20, '0'),
    sha1: `sha1-${tag}`.padEnd(20, '0'),
    md5: `md5-${tag}`.padEnd(20, '0'),
    sha512: `sha512-${tag}`.padEnd(20, '0'),
    contentType: 'application/octet-stream',
    cached,
    createdAt: '2026-02-06T10:00:00Z',
  };
}

// —— 视图投影（store 记录 → API DTO，剥离 mock 专用字段） ——

/** 用户记录 → UserView（去口令）。 */
export function toUserView(u: MockUser): UserView {
  return {
    id: u.id,
    username: u.username,
    role: u.role,
    disabled: u.disabled,
    created_at: u.created_at,
  };
}

/** 当前用户 → UserInfo。 */
export function toUserInfo(u: MockUser): UserInfo {
  return { id: u.id, username: u.username, role: u.role };
}

/** 令牌记录 → TokenView（去明文）。 */
export function toTokenView(t: MockToken): TokenView {
  return {
    id: t.id,
    name: t.name,
    created_at: t.created_at,
    last_used_at: t.last_used_at,
    revoked: t.revoked,
  };
}

/** 制品记录 → 浏览索引项 ArtifactDto。 */
export function toArtifactDto(a: MockArtifact): ArtifactDto {
  return {
    path: a.path,
    size: a.size,
    sha256: a.sha256,
    content_type: a.contentType,
    cached: a.cached,
    created_at: a.createdAt,
  };
}

/** 制品记录 + 仓库 → 详情 DTO。 */
export function toArtifactDetail(a: MockArtifact, repo: RepositoryDto): ArtifactDetailDto {
  return {
    repo_id: repo.id,
    repo_name: repo.name,
    format: repo.format,
    path: a.path,
    size: a.size,
    content_type: a.contentType,
    cached: a.cached,
    created_at: a.createdAt,
    checksums: { sha256: a.sha256, sha1: a.sha1, md5: a.md5, sha512: a.sha512 },
    usage: [
      {
        title: '下载',
        language: 'bash',
        content: `curl -O http://localhost/${repo.name}/${a.path}`,
      },
    ],
  };
}

/** 仪表盘 KPI（按当前 store 实时聚合）。 */
export function dashboardSummary(): DashboardSummary {
  const uniqueShas = new Set(state.artifacts.map((a) => a.sha256));
  const totalBytes = [...uniqueShas].reduce((sum, sha) => {
    const a = state.artifacts.find((x) => x.sha256 === sha);
    return sum + (a ? a.size : 0);
  }, 0);
  return {
    repo_count: state.repositories.length,
    artifact_count: state.artifacts.length,
    total_bytes: totalBytes,
    user_count: state.users.length,
  };
}

/** 主机指标快照（固定示例值，运行时面板可见）。 */
export function hostMetrics(): HostMetrics {
  return {
    cpu: { usage_percent: 12.5, logical_cores: 8 },
    memory: {
      total_bytes: 16 * 1024 ** 3,
      used_bytes: 6 * 1024 ** 3,
      swap_total_bytes: 4 * 1024 ** 3,
      swap_used_bytes: 0,
    },
    disk: {
      total_bytes: 512 * 1024 ** 3,
      available_bytes: 256 * 1024 ** 3,
      disks: [
        {
          mount_point: '/',
          total_bytes: 512 * 1024 ** 3,
          available_bytes: 256 * 1024 ** 3,
        },
      ],
    },
    uptime_secs: 86_400,
  };
}

/** 使用分析聚合（按当前 store 制品给示例聚合，FR-99）。 */
export function usageAnalytics(top = 5): UsageAnalyticsDto {
  const topDownloads = state.artifacts.slice(0, top).map((a, i) => {
    const repo = state.repositories.find((r) => r.id === a.repoId);
    return {
      repo_name: repo?.name ?? a.repoId,
      repo_path: a.path,
      count: (top - i) * 10,
      last_at: a.createdAt,
    };
  });
  const repoUsage = state.repositories.map((r) => ({
    repo_name: r.name,
    count: state.artifacts.filter((a) => a.repoId === r.id).length * 25,
  }));
  return {
    total_access: 1280,
    total_download: 640,
    top_downloads: topDownloads,
    repo_usage: repoUsage,
  };
}

/** 防护状态快照（据当前防护配置给「无异常」的合理默认，FR-78）。 */
export function protectionStatus(): ProtectionStatusDto {
  return {
    alerts_enabled: state.protection.alerts.enabled,
    window_secs: state.protection.alerts.window_secs,
    window_counts: [
      { dimension: 'rate_limit', count: 0 },
      { dimension: 'ban', count: 0 },
      { dimension: 'cc_challenge', count: 0 },
      { dimension: 'waf', count: 0 },
      { dimension: 'slowloris', count: 0 },
    ],
    active_banned_ips: 0,
    dropped_alerts: 0,
    recent_alerts: [],
  };
}

/**
 * 某指标键的时序点（FR-105）：在 [from, to] 区间按 step 生成少量稳定示例点。
 * 取值由指标键派生（确定、非随机），便于一开即用地看到折线。
 */
export function metricPoints(
  metric: string,
  opts: { from?: number; to?: number; step?: number } = {},
): MetricPoint[] {
  const now = Date.now();
  const to = opts.to ?? now;
  const from = opts.from ?? to - 3600_000;
  const span = Math.max(to - from, 1);
  const step = opts.step && opts.step > 0 ? opts.step : Math.max(Math.floor(span / 6), 1);
  // 指标键 → 基线值（确定性，避免随机抖动让快照不稳定）。
  const base = metric.length * 3;
  const points: MetricPoint[] = [];
  for (let ts = from, i = 0; ts <= to; ts += step, i += 1) {
    points.push({ ts, value: base + (i % 5) * 2 });
  }
  return points;
}

/** 投影：组 id → 成员视图列表（按当前用户集解析用户名）。 */
export function groupMemberViews(groupId: string) {
  const ids = state.groupMembers.get(groupId) ?? [];
  return ids
    .map((uid) => state.users.find((u) => u.id === uid))
    .filter((u): u is MockUser => u !== undefined)
    .map((u) => ({ user_id: u.id, username: u.username }));
}
