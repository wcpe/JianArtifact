// MSW 请求处理器：把 api/openapi.yaml 定义的 0.2.0 管理面端点映射到内存态 store。
// 浏览器（src/browser.ts）与 Node（src/node.ts）双端复用同一组 handlers，
// 保证 web 开发态脱离后端可跑、vitest 集成测试与真实 fetch 链路同源。
import { http, HttpResponse } from "msw";

import { mockAuditLogList, mockMetrics } from "./handlers";
import {
  observabilityStore,
  type AuditCategory,
  type AuditQuery,
  type AuditResult,
  type NotificationQuery,
} from "./observability";
import { MOCK_LICENSES } from "./licenses.gen";
import { mockNetworkDelayMs, mockVolumeFactor } from "./console";
import {
  DEV_MOCK_ROUTE_HEADER,
  currentDevMockScenario,
  interceptDevMockScenario,
} from "./scenario";
import {
  MOCK_SECOND_ADMIN_TOKEN,
  MOCK_TOKEN,
  nextSnapshotTick,
  snapshotJitter,
  store,
} from "./store";
import { MOCK_APP_VERSION, MOCK_SCHEMA_VERSION } from "./version";
import type { components } from "./schema.gen";
import type {
  AclEntry,
  BackupPackage,
  BackupImport,
  WriteFreezeState,
  MigrationPlan,
  MigrationReport,
  MigrationSourceAuth,
  MigrationSourceConfig,
  MigrationTask,
  PublishPolicy,
  RemoteNexusSourceConfig,
  Repository,
  User,
} from "./store";

function err(code: string, message: string, status: number) {
  return HttpResponse.json({ error: { code, message } }, { status });
}

const MOCK_USER_TOKEN = "mock.jwt.token:user";
const MOCK_ANONYMOUS_ID = 0;
const MOCK_ADMIN_ID = 1;
const MOCK_USER_ID = 2;
const MOCK_SECOND_ADMIN_ID = 3;

// 主机监控的模拟网卡：两个非回环网卡，与后端 host_network_interface_minute 口径一致
// （总量 = 自网卡启动以来的累计字节数，随采样单调不减）。
const MOCK_HOST_INTERFACES = [
  {
    name: "eth0",
    receiveBase: 4_200_000_000,
    transmitBase: 1_600_000_000,
    receiveRate: 8192,
    transmitRate: 4096,
  },
  {
    name: "wlan0",
    receiveBase: 900_000_000,
    transmitBase: 320_000_000,
    receiveRate: 2048,
    transmitRate: 1024,
  },
] as const;

const auditCategories = new Set<AuditCategory>([
  "management_change",
  "asset_change",
  "security_event",
  "replication",
]);
const auditResults = new Set<AuditResult>(["success", "failure", "pending", "unknown"]);

function bearerToken(request: Request) {
  const header = request.headers.get("Authorization") ?? "";
  return header.startsWith("Bearer ") ? header.slice("Bearer ".length) : "";
}

function mockUserId(request: Request): number | undefined {
  switch (bearerToken(request)) {
    case MOCK_TOKEN:
      return MOCK_ADMIN_ID;
    case MOCK_USER_TOKEN:
      return MOCK_USER_ID;
    case MOCK_SECOND_ADMIN_TOKEN:
      return MOCK_SECOND_ADMIN_ID;
    default:
      return undefined;
  }
}

/** 受保护端点统一鉴权：仅接受 mock 管理员或普通用户令牌。 */
function unauthorized(request: Request) {
  return mockUserId(request) === undefined ? err("unauthorized", "未认证", 401) : null;
}

/** 管理员端点鉴权：未知令牌未认证，普通用户令牌无权限。 */
function adminUnauthorized(request: Request) {
  const userId = mockUserId(request);
  if (userId === undefined) {
    return err("unauthorized", "未认证", 401);
  }
  return userId !== undefined && store.findUser(userId)?.role === "admin"
    ? null
    : err("forbidden", "需要管理员权限", 403);
}

function isGlobalAdmin(userId: number): boolean {
  return store.findUser(userId)?.role === "admin";
}

function auditQuery(url: URL): AuditQuery | null {
  const from = url.searchParams.get("from") ?? undefined;
  const to = url.searchParams.get("to") ?? undefined;
  if (Boolean(from) !== Boolean(to)) return null;
  if (from && to) {
    const fromMs = Date.parse(from);
    const toMs = Date.parse(to);
    if (
      !Number.isFinite(fromMs) ||
      !Number.isFinite(toMs) ||
      fromMs >= toMs ||
      toMs - fromMs > 30 * 86_400_000
    ) {
      return null;
    }
  }
  const categories = url.searchParams.getAll("category");
  const results = url.searchParams.getAll("result");
  if (
    !categories.every((value): value is AuditCategory =>
      auditCategories.has(value as AuditCategory),
    )
  ) {
    return null;
  }
  if (!results.every((value): value is AuditResult => auditResults.has(value as AuditResult))) {
    return null;
  }
  // 关键字搜索（q）与风险状态（attention）在服务端过滤，保证全量分页下的语义一致。
  const q = url.searchParams.get("q") ?? undefined;
  const attentionParam = url.searchParams.get("attention") ?? undefined;
  if (attentionParam && attentionParam !== "pending" && attentionParam !== "acknowledged") {
    return null;
  }
  return {
    from,
    to,
    categories: categories as AuditCategory[],
    results: results as AuditResult[],
    actor: url.searchParams.get("actor") ?? undefined,
    repository: url.searchParams.get("repository") ?? undefined,
    q,
    attention: attentionParam as "pending" | "acknowledged" | undefined,
    method: url.searchParams.get("method") ?? undefined,
    action: url.searchParams.get("action") ?? undefined,
    clientIp: url.searchParams.get("clientIp") ?? undefined,
    actorEmail: url.searchParams.get("actorEmail") ?? undefined,
    authSource: url.searchParams.get("authSource") ?? undefined,
  };
}

function auditPage(url: URL, prefix: string): { offset: number; limit: number } | null {
  const limit = integerQuery(url, "limit");
  if (limit.invalid || (limit.value !== undefined && (limit.value < 1 || limit.value > 100))) {
    return null;
  }
  // 显式 offset 优先于游标：分页器可直接跳到任意页，无需顺序回放游标。
  const explicitOffset = integerQuery(url, "offset");
  if (explicitOffset.invalid || (explicitOffset.value !== undefined && explicitOffset.value < 0)) {
    return null;
  }
  if (explicitOffset.value !== undefined) {
    return { offset: explicitOffset.value, limit: limit.value ?? 50 };
  }
  const cursor = url.searchParams.get("cursor");
  if (!cursor) return { offset: 0, limit: limit.value ?? 50 };
  const match = new RegExp(`^${prefix}(\\d+)$`).exec(cursor);
  if (!match) return null;
  return { offset: Number(match[1]), limit: limit.value ?? 50 };
}

function acknowledgementActor(
  request: Request,
): { displayName: string; subjectType: "user"; userId: number; authSource: string } | null {
  const userId = mockUserId(request);
  const user = userId === undefined ? undefined : store.findUser(userId);
  return user?.role === "admin"
    ? { displayName: user.username, subjectType: "user", userId: user.id, authSource: "web" }
    : null;
}

function repositoryActionUnauthorized(
  request: Request,
  repository: string,
  action: "read" | "write" | "admin",
) {
  const userId = mockUserId(request);
  if (userId === undefined) {
    return err("unauthorized", "未认证", 401);
  }
  if (isGlobalAdmin(userId)) {
    return null;
  }
  const allowed = store.canAccess(repository, userId, action);
  if (allowed === null) {
    return err("not_found", "仓库不存在", 404);
  }
  return allowed ? null : err("forbidden", "无权访问该仓库", 403);
}

function repositoryReadUnauthorized(request: Request, repository: string) {
  const userId = mockUserId(request) ?? MOCK_ANONYMOUS_ID;
  if (isGlobalAdmin(userId)) {
    return null;
  }
  const allowed = store.canAccess(repository, userId, "read");
  if (allowed === null) {
    return err("not_found", "仓库不存在", 404);
  }
  if (allowed) {
    return null;
  }
  return userId === MOCK_ANONYMOUS_ID
    ? err("unauthorized", "未认证", 401)
    : err("forbidden", "无权访问该仓库", 403);
}

function integerQuery(url: URL, key: string): { value?: number; invalid: boolean } {
  const raw = url.searchParams.get(key);
  if (raw === null) {
    return { invalid: false };
  }
  if (!/^[+-]?\d+$/.test(raw)) {
    return { invalid: true };
  }
  const value = Number(raw);
  return { value, invalid: !Number.isInteger(value) };
}

function intParam(url: URL, key: string, fallback: number): number {
  const raw = url.searchParams.get(key);
  const n = raw === null ? NaN : Number.parseInt(raw, 10);
  return Number.isFinite(n) && n > 0 ? n : fallback;
}

type DownloadGroupedTrendResponse = components["schemas"]["DownloadGroupedTrendResponse"];
type RepositoryDownloadTrendResponse = components["schemas"]["RepositoryDownloadTrendResponse"];
type DownloadTrendPoint = components["schemas"]["DownloadTrendPoint"];

interface DownloadTrendWindow {
  from: string;
  to: string;
  fromMs: number;
  toMs: number;
}

/** 解析下载趋势窗口：from/to 成对出现，缺省取最近 24 小时，跨度不超过 30 天。 */
function downloadTrendWindow(url: URL, now = Date.now()): DownloadTrendWindow | null {
  const from = url.searchParams.get("from");
  const to = url.searchParams.get("to");
  if ((from === null) !== (to === null)) return null;
  if (from === null || to === null) {
    const toMs = now;
    const fromMs = toMs - 24 * 60 * 60_000;
    return { from: new Date(fromMs).toISOString(), to: new Date(toMs).toISOString(), fromMs, toMs };
  }
  const isoDateTime = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;
  const fromMs = Date.parse(from);
  const toMs = Date.parse(to);
  if (
    !isoDateTime.test(from) ||
    !isoDateTime.test(to) ||
    !Number.isFinite(fromMs) ||
    !Number.isFinite(toMs) ||
    fromMs >= toMs ||
    toMs - fromMs > 30 * 24 * 60 * 60_000
  ) {
    return null;
  }
  return { from, to, fromMs, toMs };
}

/** 下载趋势桶粒度与 operationsBucket 保持一致。 */
function downloadTrendBucket(window: DownloadTrendWindow): "minute" | "hour" | "day" {
  const span = window.toMs - window.fromMs;
  if (span <= 24 * 60 * 60_000) return "minute";
  if (span <= 7 * 24 * 60 * 60_000) return "hour";
  return "day";
}

/** 按分钟源桶与 [from,to) 规则生成服务端同起点的连续聚合桶。 */
function downloadTrendBuckets(window: DownloadTrendWindow, bucket: "minute" | "hour" | "day") {
  const minuteMs = 60_000;
  const stepMs =
    bucket === "minute" ? minuteMs : bucket === "hour" ? 60 * minuteMs : 24 * 60 * minuteMs;
  const firstSourceMinute = Math.ceil(window.fromMs / minuteMs) * minuteMs;
  const endSourceMinute = Math.ceil(window.toMs / minuteMs) * minuteMs;
  if (firstSourceMinute >= endSourceMinute) return [];
  const firstBucket = Math.floor(firstSourceMinute / stepMs) * stepMs;
  const lastBucket = Math.floor((endSourceMinute - minuteMs) / stepMs) * stepMs;
  const buckets: Array<{ from: string; to: string }> = [];
  for (let cursor = firstBucket; cursor <= lastBucket; cursor += stepMs) {
    buckets.push({
      from: new Date(cursor).toISOString(),
      to: new Date(cursor + stepMs).toISOString(),
    });
  }
  return buckets;
}

/** 字符串转稳定种子，让不同仓库得到可复现但有区别的演示数据。 */
function downloadTrendHash(value: string): number {
  let hash = 2_166_136_261;
  for (let index = 0; index < value.length; index += 1) {
    hash = Math.imul(hash ^ value.charCodeAt(index), 16_777_619);
  }
  return hash >>> 0;
}

/** 仓库列表排序的最小结构（id/audit 等其余字段不参与排序）。 */
type RepoLike = Pick<Repository, "name" | "type" | "createdAt"> & {
  artifactCount?: number;
  totalSize?: number;
};

/** 按 sort 参数排序仓库（名称/类型/制品数/总大小/创建时间），未识别的排序键保持原序。 */
function sortRepoItems(items: RepoLike[], sort: string | null, desc: boolean): RepoLike[] {
  const field =
    sort === "artifact_count"
      ? "artifactCount"
      : sort === "total_size"
        ? "totalSize"
        : sort === "created_at"
          ? "createdAt"
          : sort === "type"
            ? "type"
            : sort === "name"
              ? "name"
              : null;
  if (!field) return items;
  const dir = desc ? -1 : 1;
  const numeric = field === "artifactCount" || field === "totalSize";
  return [...items].sort((a, b) => {
    const av = numeric ? (a[field] ?? 0) : (a[field] ?? "");
    const bv = numeric ? (b[field] ?? 0) : (b[field] ?? "");
    if (av === bv) return 0;
    return (av > bv ? 1 : -1) * dir;
  });
}

function isEmptyScenario(request: Request): boolean {
  return request.headers.get("X-Jian-DevMock-Scenario") === "empty";
}

const notificationStatuses = ["unacknowledged", "acknowledged", "all"] as const;

// —— FR-132 备份包内存态（仅 devmock）——
// 生成过程用定时器推进状态，便于在浏览器里看到 queued → snapshotting → packing → done。
function seedBackups(): BackupPackage[] {
  return [
    {
      packageId: "bk-20260910-090000-a1b2c3",
      mode: "hot",
      status: "done",
      label: "例行备份",
      sizeBytes: 73400320,
      counts: {
        users: 5,
        tokens: 8,
        repositories: 20,
        acls: 12,
        assets: 74481,
        formatMetadata: 74,
      },
      nodeId: "np-demo",
      appVersion: MOCK_APP_VERSION,
      dbSchemaVersion: MOCK_SCHEMA_VERSION,
      createdAt: new Date(Date.now() - 6 * 3600 * 1000).toISOString(),
      finishedAt: new Date(Date.now() - 6 * 3600 * 1000 + 42000).toISOString(),
    },
  ];
}

const mockBackups: BackupPackage[] = seedBackups();

let mockBackupSeq = 1;

// —— FR-134：写入冻结窗口内存态（仅 devmock）——
// 进程内一个状态对象；未冻结时仅含 frozen:false，避免输出 until/frozenAt。
let mockFreeze: WriteFreezeState = { frozen: false };

// —— FR-137：从 URL 导入备份包内存态（仅 devmock）——
// 预置 2~3 条真实分布：一条待重启、一条成功、一条失败（带 errorCode + error）。
function seedImports(): BackupImport[] {
  const now = Date.now();
  const iso = (msAgo: number) => new Date(now - msAgo).toISOString();
  return [
    {
      importId: "imp-20260908-140000-a1b2c3",
      origin: "url",
      status: "pending_restart",
      sourceUrl: "https://repo.example.com/backups/bk-20260908.tar.gz",
      operator: "admin",
      overwrite: true,
      deep: false,
      totalBytes: 73400320,
      fetchedBytes: 73400320,
      blobCount: 74481,
      packageId: "bk-20260908-140000-a1b2c3",
      createdAt: iso(2 * 86_400_000),
      updatedAt: iso(2 * 86_400_000),
      finishedAt: iso(2 * 86_400_000),
      restorePendingAt: iso(2 * 86_400_000),
    },
    {
      importId: "imp-20260909-100000-d4e5f6",
      origin: "cli",
      status: "done",
      operator: "admin",
      overwrite: false,
      deep: true,
      totalBytes: 52428800,
      fetchedBytes: 52428800,
      blobCount: 51024,
      packageId: "bk-20260909-100000-d4e5f6",
      createdAt: iso(86_400_000),
      updatedAt: iso(86_400_000),
      finishedAt: iso(86_400_000),
    },
    {
      importId: "imp-20260909-160000-g7h8i9",
      origin: "upload",
      status: "failed",
      operator: "admin",
      overwrite: false,
      deep: false,
      totalBytes: 10485760,
      fetchedBytes: 3145728,
      blobCount: 0,
      errorCode: "manifest_schema_version_unsupported",
      error: "包 manifest 的 schemaVersion 不被当前版本支持",
      createdAt: iso(20 * 3_600_000),
      updatedAt: iso(20 * 3_600_000),
    },
  ];
}

let mockImports: BackupImport[] = seedImports();
let mockImportSeq = 1;

// —— FR-137：分片上传备份包内存态（仅 devmock）——
// 服务端决定的分片大小固定 8 MiB；uploadedChunks 以实际收到的分片为准（可续传）。
const UPLOAD_CHUNK_SIZE = 8 * 1024 * 1024;
type UploadStatus = "initialized" | "receiving" | "completed" | "aborted";
interface MockUploadSession {
  uploadId: string;
  fileName: string;
  totalBytes: number;
  chunkSize: number;
  uploadedChunks: number[];
  status: UploadStatus;
  expiresAt: string;
}
let mockUploads: Record<string, MockUploadSession> = {};
// 进程内单调递增、不复位：uploadId 只到秒级时间戳，若序号同时复位，同秒内的不同会话
// （尤其并行用例）会撞出相同 id；真实后端 id 必然唯一，这里对齐该语义。
let mockUploadIdCounter = 0;

/** 定时推进导入状态：queued → fetching（fetchedBytes 渐进逼近 totalBytes）→ staging → pending_restart。
 * 终态后写 restore.pending 标记，等待重启替换；便于前端进度条与轮询可见。 */
function advanceMockImport(importId: string): void {
  const find = () => mockImports.find((item) => item.importId === importId);
  setTimeout(() => {
    const rec = find();
    if (rec && rec.status === "queued") {
      rec.status = "fetching";
    }
  }, 300);
  const timer = setInterval(() => {
    const rec = find();
    if (!rec || rec.status !== "fetching") {
      clearInterval(timer);
      return;
    }
    // totalBytes/fetchedBytes 在契约里是可选的，这里取本地非空值参与运算。
    const total = rec.totalBytes ?? 0;
    const fetched = rec.fetchedBytes ?? 0;
    rec.fetchedBytes = Math.min(total, fetched + Math.ceil(total * 0.25));
    rec.updatedAt = new Date().toISOString();
    if ((rec.fetchedBytes ?? 0) >= total) {
      clearInterval(timer);
      rec.status = "staging";
      rec.blobCount = 74481;
      setTimeout(() => {
        const r = find();
        if (r) {
          r.status = "pending_restart";
          r.packageId = `bk-${new Date()
            .toISOString()
            .replace(/[-:T.]/g, "")
            .slice(0, 14)}-imported`;
          r.finishedAt = new Date().toISOString();
          r.restorePendingAt = new Date().toISOString();
          r.updatedAt = new Date().toISOString();
        }
      }, 500);
    }
  }, 400);
}

/** 复位备份包内存态（测试用例间隔离用；devmock 自身长驻时保持状态）。 */
export function resetMockBackups(): void {
  mockBackups.splice(0, mockBackups.length, ...seedBackups());
  mockBackupSeq = 1;
  // 冻结窗口与导入记录同属备份域，一并复位，避免用例间状态泄漏。
  mockFreeze = { frozen: false };
  mockImports = seedImports();
  mockImportSeq = 1;
  // 分片上传会话同属备份域，一并复位，避免用例间状态泄漏。
  // 注意 mockUploadIdCounter 不复位：id 需跨会话保持唯一。
  mockUploads = {};
}

function findMockBackup(packageId: string): BackupPackage | undefined {
  return mockBackups.find((item) => item.packageId === packageId);
}

function createMockBackup(mode: BackupPackage["mode"], label: string): BackupPackage {
  mockBackupSeq += 1;
  const stamp = new Date()
    .toISOString()
    .replace(/[-:T.]/g, "")
    .slice(0, 14);
  return {
    packageId: `bk-${stamp}-dev${String(mockBackupSeq).padStart(3, "0")}`,
    mode,
    status: "queued",
    label,
    sizeBytes: 0,
    nodeId: "np-demo",
    appVersion: MOCK_APP_VERSION,
    dbSchemaVersion: MOCK_SCHEMA_VERSION,
    createdAt: new Date().toISOString(),
  };
}

/** 定时推进生成状态；终态补上包体大小与计数。 */
function advanceMockBackup(packageId: string): void {
  const stages: Array<{ status: BackupPackage["status"]; delay: number }> = [
    { status: "snapshotting", delay: 500 },
    { status: "packing", delay: 1500 },
  ];
  for (const stage of stages) {
    setTimeout(() => {
      const found = findMockBackup(packageId);
      if (found && found.status !== "done" && found.status !== "failed") {
        found.status = stage.status;
      }
    }, stage.delay);
  }
  setTimeout(() => {
    const found = findMockBackup(packageId);
    if (found && found.status !== "failed") {
      found.status = "done";
      found.sizeBytes = 73400320;
      found.counts = {
        users: 5,
        tokens: 8,
        repositories: 20,
        acls: 12,
        assets: 74481,
        formatMetadata: 74,
      };
      found.finishedAt = new Date().toISOString();
    }
  }, 2600);
}
/** FR-117：解析通知中心查询参数；非法组合返回 null（映射 400）。 */
function notificationQuery(url: URL): NotificationQuery | null {
  const params = url.searchParams;
  const from = params.get("from") ?? undefined;
  const to = params.get("to") ?? undefined;
  if ((from === undefined) !== (to === undefined)) return null;
  const rawStatus = params.get("status") ?? undefined;
  if (rawStatus !== undefined && !notificationStatuses.includes(rawStatus as never)) return null;
  const rawLimit = params.get("limit");
  let limit: number | undefined;
  if (rawLimit !== null) {
    limit = Number.parseInt(rawLimit, 10);
    if (!Number.isFinite(limit) || limit < 1 || limit > 100) return null;
  }
  const rawCursor = params.get("cursor");
  let cursor: number | undefined;
  if (rawCursor !== null) {
    cursor = Number.parseInt(rawCursor, 10);
    if (!Number.isFinite(cursor) || cursor < 0) return null;
  }
  return {
    from,
    to,
    status: rawStatus as NotificationQuery["status"],
    limit,
    cursor,
  };
}

function isSetupEmptyScenario(request: Request): boolean {
  return isEmptyScenario(request) && request.headers.get(DEV_MOCK_ROUTE_HEADER) === "/setup";
}

const EMPTY_MIGRATION_TASK_ID = 1;
const EMPTY_MIGRATION_TIME = "2026-08-26T00:00:00Z";

function emptyMigrationTask(id: number): MigrationTask | null {
  if (id !== EMPTY_MIGRATION_TASK_ID) {
    return null;
  }
  return {
    id,
    status: "planned",
    sourceType: "online_rest",
    conflictPolicy: "skip",
    createdAt: EMPTY_MIGRATION_TIME,
    updatedAt: EMPTY_MIGRATION_TIME,
    plan: {
      repositories: [],
      warnings: [],
      stats: { repositoryCount: 0, estimatedAssets: 0 },
      estimated: true,
    },
  };
}

function emptyMigrationReport(id: number): MigrationReport | null {
  const task = emptyMigrationTask(id);
  if (!task) {
    return null;
  }
  return {
    taskId: task.id,
    status: task.status,
    sourceType: task.sourceType,
    conflictPolicy: task.conflictPolicy,
    totals: { copied: 0, skipped: 0, failed: 0 },
    raw: {},
  };
}

function prepareMigrationFixtureScenario(request: Request): void {
  // 复用统一场景解析：浏览器正常访问不带场景头，此时就是 normal，必须注入夹具，
  // 否则"迁移与搬迁"页在开发态永远停在「暂无数据」。
  const scenario = currentDevMockScenario(request);
  if (scenario === "normal" || scenario === "loading") {
    store.ensureMigrationLifecycleFixtures();
  }
}

function rawProtocolTarget(request: Request): { repository: string; path: string } | null {
  const parts = new URL(request.url).pathname.split("/").filter(Boolean);
  if (parts[0] !== "repository" || parts.length < 3) {
    return null;
  }
  return {
    repository: decodeURIComponent(parts[1]!),
    path: parts
      .slice(2)
      .map((part) => decodeURIComponent(part))
      .join("/"),
  };
}

interface BootstrapBody {
  username?: string;
  password?: string;
}
interface CreateUserBody {
  username?: string;
  password?: string;
  role?: User["role"];
}
interface UpdateUserBody {
  role?: User["role"];
  status?: User["status"];
  webLoginDisabled?: boolean;
}
interface CreateRepoBody {
  name?: string;
  format?: Repository["format"];
  type?: Repository["type"];
  visibility?: Repository["visibility"];
  description?: string;
  remoteUrl?: string;
  members?: string[];
  aliases?: string[];
}

interface AssetOperationBody {
  action?: "delete" | "move" | "rename";
  targets?: {
    type?:
      | "raw_path"
      | "maven_version"
      | "maven_artifact"
      | "npm_package"
      | "npm_version"
      | "asset_path";
    path?: string;
  }[];
  destinationPath?: string;
  newPath?: string;
  overrideReason?: string;
}

type ValidAssetOperationBody = AssetOperationBody & {
  action: NonNullable<AssetOperationBody["action"]>;
  targets: NonNullable<AssetOperationBody["targets"]>;
  overrideReason: string;
};

function isValidAssetOperationBody(body: AssetOperationBody): body is ValidAssetOperationBody {
  return (
    (body.action === "delete" || body.action === "move" || body.action === "rename") &&
    Array.isArray(body.targets) &&
    body.targets.every(
      (target) =>
        (target.type === "raw_path" ||
          target.type === "maven_version" ||
          target.type === "maven_artifact" ||
          target.type === "npm_package" ||
          target.type === "npm_version" ||
          target.type === "asset_path") &&
        typeof target.path === "string" &&
        target.path.trim().length > 0,
    ) &&
    typeof body.overrideReason === "string" &&
    body.overrideReason.trim().length > 0 &&
    body.overrideReason.length <= 512
  );
}

export const handlers = [
  // 开发态通用场景必须先于业务 handler 处理；undefined 时继续走既有路由。
  // 同一个前置守卫顺带承担"网速"模拟：按控制台档位在放行前等待一段延迟，
  // 这样任何端点都受网速档位影响，无需逐 handler 改造。
  http.all("*", async ({ request }) => {
    const scenarioResponse = await interceptDevMockScenario(request);
    if (scenarioResponse) return scenarioResponse;
    const delay = mockNetworkDelayMs();
    if (delay > 0) {
      await new Promise((resolve) => setTimeout(resolve, delay));
    }
    return undefined;
  }),

  // —— 健康 / 状态（公开）——
  http.get("*/healthz", () => HttpResponse.json({ status: "ok", version: store.status().version })),
  http.get("*/readyz", () => HttpResponse.json({ status: "ok", version: store.status().version })),
  // FR-34：OIDC 登录（开发环境不接真实 IdP：起跳指向示例地址，回调直接回带 mock 会话令牌）。
  http.get("*/api/v1/auth/oidc/start", () =>
    HttpResponse.redirect("https://idp.example.com/authorize?client_id=devmock&state=devmock"),
  ),
  http.get("*/api/v1/auth/oidc/callback", () =>
    HttpResponse.redirect(`/login#token=${encodeURIComponent(MOCK_TOKEN)}`),
  ),
  // FR-39：运维指标端点（文本暴露格式，与后端 /metrics 同口径）。
  http.get(
    "*/metrics",
    () =>
      new HttpResponse(mockMetrics(), {
        headers: { "Content-Type": "text/plain; version=0.0.4; charset=utf-8" },
      }),
  ),
  http.get("*/api/v1/status", ({ request }) =>
    HttpResponse.json(
      isSetupEmptyScenario(request)
        ? { ...store.status(), initialized: false, userCount: 0, bootstrapAllowed: true }
        : store.status(),
    ),
  ),

  // —— 开源协议清单（admin 专属，非契约）——
  http.get(
    "*/api/v1/licenses",
    ({ request }) =>
      adminUnauthorized(request) ??
      HttpResponse.json(
        isEmptyScenario(request) ? { ...MOCK_LICENSES, go: [], npm: [] } : MOCK_LICENSES,
      ),
  ),

  // —— 全局制品搜索（开发态仓库树夹具）——
  http.get("*/api/v1/search", ({ request }) => {
    if (isEmptyScenario(request)) {
      return HttpResponse.json({ items: [], total: 0, facets: [] });
    }
    const url = new URL(request.url);
    const userId = mockUserId(request) ?? MOCK_ANONYMOUS_ID;
    if (userId === MOCK_ANONYMOUS_ID && !store.anonymousAccess()) {
      return err("unauthorized", "未认证", 401);
    }
    const repository = url.searchParams.get("repository") ?? "";
    const readableRepositories = isGlobalAdmin(userId)
      ? undefined
      : store.readableRepositoryNames(userId);
    if (repository) {
      const denied = repositoryReadUnauthorized(request, repository);
      if (denied) {
        return denied;
      }
    }
    const order = url.searchParams.get("order") === "desc" ? "desc" : "asc";
    return HttpResponse.json(
      store.searchAssets(
        url.searchParams.get("q") ?? "",
        url.searchParams.get("sort") ?? "path",
        order,
        intParam(url, "page", 1),
        intParam(url, "page_size", 50),
        readableRepositories,
      ),
    );
  }),

  // —— 认证（公开）——
  http.post("*/api/v1/auth/bootstrap", async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as BootstrapBody;
    if (!body.username || !body.password) {
      return err("bad_request", "用户名与口令必填", 400);
    }
    const user = store.bootstrap(body.username);
    if (!user) {
      return err("already_initialized", "实例已初始化，自举关闭", 409);
    }
    return HttpResponse.json({ token: MOCK_TOKEN, user }, { status: 201 });
  }),

  http.post("*/api/v1/auth/login", async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as BootstrapBody;
    if (!body.username || !body.password) {
      return err("bad_request", "用户名与口令必填", 400);
    }
    const user = store.login(body.username);
    if (!user) {
      return err("unauthorized", "用户名或口令错误", 401);
    }
    return HttpResponse.json(
      {
        token:
          user.id === MOCK_SECOND_ADMIN_ID
            ? MOCK_SECOND_ADMIN_TOKEN
            : user.role === "admin"
              ? MOCK_TOKEN
              : MOCK_USER_TOKEN,
        user,
      },
      { status: 200 },
    );
  }),

  http.post(
    "*/api/v1/auth/logout",
    ({ request }) => unauthorized(request) ?? new HttpResponse(null, { status: 204 }),
  ),

  // —— 用户 ——
  http.get("*/api/v1/users", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    if (isEmptyScenario(request)) {
      return HttpResponse.json({ items: [], total: 0 });
    }
    const url = new URL(request.url);
    return HttpResponse.json(
      store.listUsers(intParam(url, "page", 1), intParam(url, "page_size", 20)),
    );
  }),

  http.post("*/api/v1/users", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as CreateUserBody;
    if (!body.username || !body.password) {
      return err("bad_request", "用户名与口令必填", 400);
    }
    const user = store.createUser(body.username, body.role ?? "user");
    if (!user) {
      return err("conflict", "用户名已存在", 409);
    }
    return HttpResponse.json(user, { status: 201 });
  }),

  http.patch("*/api/v1/users/:id", async ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as UpdateUserBody;
    const user = store.updateUser(Number(params.id), body);
    return user ? HttpResponse.json(user) : err("not_found", "用户不存在", 404);
  }),

  http.delete("*/api/v1/users/:id", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    return store.deleteUser(Number(params.id))
      ? new HttpResponse(null, { status: 204 })
      : err("not_found", "用户不存在", 404);
  }),

  http.post("*/api/v1/users/:id/password", async ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const userId = mockUserId(request)!;
    if (userId !== MOCK_ADMIN_ID && userId !== Number(params.id)) {
      return err("forbidden", "只能修改自己的口令", 403);
    }
    const body = (await request.json().catch(() => ({}))) as { password?: string };
    if (!body.password || body.password.length < 8) {
      return err("bad_request", "口令至少 8 位", 400);
    }
    return store.findUser(Number(params.id))
      ? new HttpResponse(null, { status: 204 })
      : err("not_found", "用户不存在", 404);
  }),

  // FR-109：管理员发布账号策略，mock 与真实管理接口保持同一路径与字段。
  http.get("*/api/v1/users/:id/publish-policies/:repo", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const policy = store.getPublishPolicy(Number(params.id), String(params.repo));
    return policy ? HttpResponse.json(policy) : err("not_found", "用户或 Hosted 仓库不存在", 404);
  }),

  http.put("*/api/v1/users/:id/publish-policies/:repo", async ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const body = (await request.json().catch(() => ({}))) as Partial<PublishPolicy>;
    // immutableRelease 已废弃：契约与真实后端都按「只读兼容字段」处理——不要求出现，
    // 出现即拒绝（与真实后端 PUT 端点口径一致），否则前端按契约发来的请求会被误判为缺字段。
    if (body.immutableRelease !== undefined) {
      return err("immutable_release_moved", "不可变 Release 请在仓库配置中更新", 400);
    }
    if (
      typeof body.webLoginDisabled !== "boolean" ||
      !Array.isArray(body.allowedPrefixes) ||
      typeof body.maxAssetsHour !== "number" ||
      typeof body.maxBytesDay !== "number" ||
      typeof body.maxFileBytes !== "number"
    ) {
      return err("bad_request", "发布策略字段不完整", 400);
    }
    const policy = store.setPublishPolicy(Number(params.id), String(params.repo), {
      webLoginDisabled: body.webLoginDisabled,
      allowedPrefixes: body.allowedPrefixes.filter(
        (value): value is string => typeof value === "string",
      ),
      maxAssetsHour: body.maxAssetsHour,
      maxBytesDay: body.maxBytesDay,
      maxFileBytes: body.maxFileBytes,
    });
    return policy ? HttpResponse.json(policy) : err("not_found", "用户或 Hosted 仓库不存在", 404);
  }),

  // FR-109：批量应用发布策略。与真实后端同源：先对全部仓库做统一预校验，任一仓库不存在或
  // 不是 hosted 即整体拒绝（4xx，消息点名仓库）；否则逐仓库落库并返回逐仓库结果。
  http.put("*/api/v1/users/:id/publish-policies", async ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const body = (await request.json().catch(() => ({}))) as Partial<PublishPolicy> & {
      repositories?: unknown;
    };
    if (!Array.isArray(body.repositories) || body.repositories.length === 0) {
      return err("bad_request", "至少选择一个仓库", 400);
    }
    // immutableRelease 已废弃：契约与真实后端都按「只读兼容字段」处理——不要求出现，
    // 出现即拒绝（与真实后端 PUT 端点口径一致），否则前端按契约发来的请求会被误判为缺字段。
    if (body.immutableRelease !== undefined) {
      return err("immutable_release_moved", "不可变 Release 请在仓库配置中更新", 400);
    }
    if (
      typeof body.webLoginDisabled !== "boolean" ||
      !Array.isArray(body.allowedPrefixes) ||
      typeof body.maxAssetsHour !== "number" ||
      typeof body.maxBytesDay !== "number" ||
      typeof body.maxFileBytes !== "number"
    ) {
      return err("bad_request", "发布策略字段不完整", 400);
    }
    const repositories = body.repositories.filter(
      (value): value is string => typeof value === "string",
    );
    if (repositories.length === 0) {
      return err("bad_request", "至少选择一个仓库", 400);
    }
    const results = store.setPublishPolicies(Number(params.id), repositories, {
      webLoginDisabled: body.webLoginDisabled,
      allowedPrefixes: body.allowedPrefixes.filter(
        (value): value is string => typeof value === "string",
      ),
      maxAssetsHour: body.maxAssetsHour,
      maxBytesDay: body.maxBytesDay,
      maxFileBytes: body.maxFileBytes,
    });
    if (!results) {
      return err("not_found", "用户不存在", 404);
    }
    if (results.missing) {
      return err("not_found", `仓库 ${results.missing}：仓库不存在`, 404);
    }
    if (results.notHosted) {
      return err(
        "validation_error",
        `仓库 ${results.notHosted}：不是 Hosted 仓库，无法配置发布策略`,
        400,
      );
    }
    return HttpResponse.json({ results: results.results });
  }),

  // —— API Token ——
  http.get("*/api/v1/tokens", ({ request }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    return HttpResponse.json(
      isEmptyScenario(request) ? { items: [] } : store.listTokens(mockUserId(request)!),
    );
  }),

  http.post("*/api/v1/tokens", async ({ request }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as { name?: string };
    if (!body.name) {
      return err("bad_request", "令牌名称必填", 400);
    }
    return HttpResponse.json(store.createToken(mockUserId(request)!, body.name), { status: 201 });
  }),

  http.delete("*/api/v1/tokens/:id", ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    return store.deleteToken(Number(params.id), mockUserId(request)!)
      ? new HttpResponse(null, { status: 204 })
      : err("not_found", "令牌不存在", 404);
  }),

  // —— 仓库 ——
  // FR-66：匿名（无 Bearer）也可列仓库——开关开时返回匿名可读集合，关时 401。
  http.get("*/api/v1/repositories", ({ request }) => {
    const url = new URL(request.url);
    const page = intParam(url, "page", 1);
    const pageSize = intParam(url, "page_size", 20);
    // 数据量档位由 store 自己物化（`store.reconcileVolumeRepositories`：模块加载 + 档位变更时
    // 各对账一次），这里不再需要放大列表——克隆仓库是 store 里真实存在的条目，列表与按名查询
    // 看到的是同一份数据。
    const userId = mockUserId(request);
    if (userId === undefined) {
      if (request.headers.has("Authorization") || !store.anonymousAccess()) {
        return err("unauthorized", "未认证", 401);
      }
      if (isEmptyScenario(request)) {
        return HttpResponse.json({ items: [], total: 0 });
      }
      return HttpResponse.json(store.listAnonymousRepositories(page, pageSize));
    }
    if (isEmptyScenario(request)) {
      return HttpResponse.json({ items: [], total: 0 });
    }
    // 先取全量再排序分页（sort/order 参数生效，表头排序与排序下拉共用）。
    const source =
      userId === MOCK_ADMIN_ID
        ? store.listRepositories(1, 100_000).items
        : store.listAccessibleRepositories(userId, 1, 100_000).items;
    const sorted = sortRepoItems(
      source,
      url.searchParams.get("sort"),
      url.searchParams.get("order") === "desc",
    );
    const start = (page - 1) * pageSize;
    return HttpResponse.json({
      items: sorted.slice(start, start + pageSize),
      total: sorted.length,
    });
  }),

  // 公开仓库列表（侧边栏公开导航；开关关时与真实端点一致返回 401）。
  // 响应携带 pinnedNames（全局置顶且匿名可读的仓库名），使公开页也能展示置顶。
  http.get("*/api/v1/public/repositories", ({ request }) => {
    if (!store.anonymousAccess()) {
      return err("unauthorized", "匿名访问已关闭", 401);
    }
    if (isEmptyScenario(request)) {
      return HttpResponse.json({ items: [], total: 0, pinnedNames: [] });
    }
    // 档位克隆同样是真实仓库，公开导航与登录态列表口径一致。
    return HttpResponse.json({
      ...store.listAnonymousRepositories(1, 100),
      pinnedNames: store.globalPinnedNames(),
    });
  }),

  // —— 置顶仓库（用户级 + 全局兜底，迁移 0043）——

  // 当前用户的置顶：登录用户读自己的；匿名回退全局置顶（与后端一致）。
  http.get("*/api/v1/me/pinned-repositories", ({ request }) => {
    const userId = mockUserId(request);
    return HttpResponse.json(store.pinnedResponse(userId ?? null));
  }),

  // 覆盖式写入当前用户的置顶，仅登录用户（匿名 401——匿名没有归属主体）。
  http.put("*/api/v1/me/pinned-repositories", async ({ request }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const userId = mockUserId(request);
    if (userId === undefined) {
      return err("unauthorized", "未认证", 401);
    }
    const body = (await request.json().catch(() => ({}))) as { repositoryIds?: number[] };
    if (!Array.isArray(body.repositoryIds)) {
      return err("bad_request", "repositoryIds 必填", 400);
    }
    const saved = store.setPinned(userId, body.repositoryIds);
    if (saved === null) {
      return err("not_found", "资源不存在", 404);
    }
    return HttpResponse.json(store.pinnedResponse(userId));
  }),

  // 全局置顶：读（admin）、覆盖写（admin）。
  http.get(
    "*/api/v1/settings/pinned-repositories",
    ({ request }) => adminUnauthorized(request) ?? HttpResponse.json(store.pinnedResponse(null)),
  ),

  http.put("*/api/v1/settings/pinned-repositories", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as { repositoryIds?: number[] };
    if (!Array.isArray(body.repositoryIds)) {
      return err("bad_request", "repositoryIds 必填", 400);
    }
    const saved = store.setPinned(null, body.repositoryIds);
    if (saved === null) {
      return err("not_found", "资源不存在", 404);
    }
    return HttpResponse.json(store.pinnedResponse(null));
  }),

  // —— 匿名访问全局开关（FR-66，admin）——
  http.get(
    "*/api/v1/settings/anonymous-access",
    ({ request }) =>
      adminUnauthorized(request) ?? HttpResponse.json({ enabled: store.anonymousAccess() }),
  ),

  http.put("*/api/v1/settings/anonymous-access", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as { enabled?: boolean };
    if (typeof body.enabled !== "boolean") {
      return err("bad_request", "enabled 必填", 400);
    }
    return HttpResponse.json({ enabled: store.setAnonymousAccess(body.enabled) });
  }),

  // 实例基础设置：供开发态设置页完整预览，敏感部署配置不在此返回。
  http.get("*/api/v1/settings", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    return HttpResponse.json(
      isEmptyScenario(request)
        ? { ...store.settings(), anonymousAccess: false, publicUrl: "" }
        : store.settings(),
    );
  }),

  http.put("*/api/v1/settings", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      anonymousAccess?: boolean;
      publicUrl?: string;
      upstreamTimeout?: number;
      allowedHosts?: string[];
      originTokenEnabled?: boolean;
      originTokenHeader?: string;
      originTokenValue?: string;
    };
    const current = store.settings();
    return HttpResponse.json(
      store.updateSettings({
        anonymousAccess:
          typeof body.anonymousAccess === "boolean"
            ? body.anonymousAccess
            : current.anonymousAccess,
        publicUrl: typeof body.publicUrl === "string" ? body.publicUrl : current.publicUrl,
        upstreamTimeout:
          typeof body.upstreamTimeout === "number" ? body.upstreamTimeout : current.upstreamTimeout,
        allowedHosts: Array.isArray(body.allowedHosts)
          ? body.allowedHosts.map((item) => String(item).trim()).filter(Boolean)
          : current.allowedHosts,
        originTokenEnabled:
          typeof body.originTokenEnabled === "boolean"
            ? body.originTokenEnabled
            : current.originTokenEnabled,
        originTokenHeader:
          typeof body.originTokenHeader === "string"
            ? body.originTokenHeader
            : current.originTokenHeader,
        originTokenValue:
          typeof body.originTokenValue === "string"
            ? body.originTokenValue
            : current.originTokenValue,
      }),
    );
  }),

  // FR-118：当前节点统一审计观测。旧 /audit-logs 仍保留在下方兼容既有页面。
  http.get("*/api/v1/observability/audit/summary", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const query = auditQuery(new URL(request.url));
    return query
      ? HttpResponse.json(
          isEmptyScenario(request)
            ? observabilityStore.emptySummary(query)
            : observabilityStore.summary(query),
        )
      : err("bad_request", "审计筛选条件或时间范围无效", 400);
  }),

  http.get("*/api/v1/observability/audit/events", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const url = new URL(request.url);
    const query = auditQuery(url);
    const page = auditPage(url, "audit-cursor-");
    if (!query || !page) return err("bad_request", "审计筛选条件或分页参数无效", 400);
    const snapshot = url.searchParams.get("snapshot") ?? undefined;
    const result = isEmptyScenario(request)
      ? observabilityStore.emptyEvents(query, snapshot)
      : observabilityStore.events(query, snapshot, page.offset, page.limit);
    return result === "stale"
      ? err("attention_stale", "审计快照与当前筛选条件不匹配", 409)
      : HttpResponse.json(result);
  }),

  http.get("*/api/v1/observability/audit/attentions", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const url = new URL(request.url);
    const query = auditQuery(url);
    const page = auditPage(url, "audit-attention-cursor-");
    if (!query || !page) return err("bad_request", "审计筛选条件或分页参数无效", 400);
    const snapshot = url.searchParams.get("snapshot") ?? undefined;
    const result = isEmptyScenario(request)
      ? observabilityStore.attentions(query, snapshot, 0, page.limit)
      : observabilityStore.attentions(query, snapshot, page.offset, page.limit);
    return result === "stale"
      ? err("attention_stale", "审计快照与当前筛选条件不匹配", 409)
      : HttpResponse.json(result);
  }),

  http.get("*/api/v1/observability/audit/events/:eventId", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const eventId = String(params.eventId ?? "");
    const result = observabilityStore.eventDetail(eventId);
    return result ? HttpResponse.json(result) : err("not_found", "审计事件不存在", 404);
  }),

  http.get("*/api/v1/observability/audit/attention/:attentionId", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const page = auditPage(new URL(request.url), "attention-cursor-");
    if (!page) return err("bad_request", "审计分页参数无效", 400);
    if (isEmptyScenario(request)) return err("not_found", "风险关注批次不存在", 404);
    const result = observabilityStore.attention(
      String(params.attentionId ?? ""),
      page.offset,
      page.limit,
    );
    if (result === "stale") return err("attention_stale", "风险关注批次快照不可重建", 409);
    return result ? HttpResponse.json(result) : err("not_found", "风险关注批次不存在", 404);
  }),

  http.put("*/api/v1/observability/audit/attention-acknowledgements", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const body = (await request.json().catch(() => ({}))) as { attentionId?: unknown };
    if (typeof body.attentionId !== "string" || body.attentionId.trim() === "") {
      return err("bad_request", "attentionId 必填", 400);
    }
    const actor = acknowledgementActor(request);
    if (!actor) return err("forbidden", "需要管理员权限", 403);
    const result = observabilityStore.acknowledge(body.attentionId, actor);
    return result === "stale"
      ? err("attention_stale", "风险关注批次快照不可重建", 409)
      : HttpResponse.json(result);
  }),

  http.get("*/api/v1/observability/audit/notifications", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const query = notificationQuery(new URL(request.url));
    if (!query) return err("bad_request", "通知筛选条件或分页参数无效", 400);
    return HttpResponse.json(
      isEmptyScenario(request)
        ? observabilityStore.emptyNotifications(query)
        : observabilityStore.notifications(query),
    );
  }),

  http.get("*/api/v1/observability/dashboard", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const now = new Date();
    const minute = new Date(now.getTime() - 60_000).toISOString();
    // v0.8.0：上游自动阻止告警样例（与 t1 测试站一致），展示去重面板与阻止窗口。
    const blockedAt = (minutesAgo: number) =>
      new Date(now.getTime() - minutesAgo * 60_000).toISOString();
    const blockedUntil = new Date(now.getTime() + 40 * 60_000).toISOString();
    // 每次请求前移快照序号：让页眉刷新后 KPI 与趋势都有可见变化（此前数值恒定，
    // 刷新完全看不出区别）。序号 0 为确定性基线——测试断言与 resetStore 都依赖它。
    const tick = nextSnapshotTick("dashboard");
    const jitter = (salt: number, amplitude: number) => snapshotJitter(tick, salt, amplitude);
    // 趋势样例：12 个桶（每桶 5 分钟），让趋势图呈真实曲线而非单点。
    // 数据量档位会放大桶数（大档 192 桶），用于验证前端渲染护栏。
    const BUCKETS = 12 * mockVolumeFactor();
    const bucketRange = (i: number) => {
      const end = new Date(now.getTime() - (BUCKETS - 1 - i) * 5 * 60_000);
      const start = new Date(end.getTime() - 5 * 60_000);
      return { from: start.toISOString(), to: end.toISOString() };
    };
    const trendBuckets = Array.from({ length: BUCKETS }, (_, i) => {
      const wave = Math.sin(i / 1.8);
      // 每个桶用独立 salt 抖动，整条曲线形状每次刷新都会变。
      const requestCount = Math.max(6, Math.round(96 + wave * 26 + i * 2) + jitter(100 + i, 7));
      const downloadCount = Math.max(4, Math.round(requestCount * 0.72) + jitter(200 + i, 5));
      const cacheHitCount = Math.round(downloadCount * 0.85);
      return {
        ...bucketRange(i),
        requestCount,
        downloadCount,
        failureCount: Math.max(0, (i % 5 === 3 ? 2 : i % 3 === 0 ? 1 : 0) + jitter(300 + i, 1)),
        cacheHitCount,
        cacheMissCount: Math.max(0, downloadCount - cacheHitCount),
      };
    });
    // 仓库数必须与列表同源：档位放大会把种子物化成更多真实仓库，
    // 写死 9 会让「仪表盘说 9、仓库列表说 144」当场对不上。
    const repositoryTotal = store.listRepositories(1, 1).total;
    const capacityTrendBuckets = Array.from({ length: BUCKETS }, (_, i) => ({
      ...bucketRange(i),
      repositoryCount: repositoryTotal,
      assetCount: 28416 + jitter(400 + i, 260),
      logicalBytes: 86_000_000_000 + i * 620_000_000 + jitter(500 + i, 3_000_000_000),
    }));
    return HttpResponse.json({
      from: new Date(now.getTime() - 86_400_000).toISOString(),
      to: now.toISOString(),
      effectiveBucket: "minute",
      current: {
        from: minute,
        to: now.toISOString(),
        repositoryCount: repositoryTotal,
        assetCount: 28416 + jitter(1, 240),
        logicalBytes: 92771293542 + jitter(2, 6_000_000_000),
      },
      kpi: {
        repositoryCount: repositoryTotal,
        assetCount: 28416 + jitter(1, 240),
        logicalBytes: 92771293542 + jitter(2, 6_000_000_000),
        requestCount: 128 + jitter(3, 22),
        downloadCount: 92 + jitter(4, 16),
        failureCount: Math.max(0, 1 + jitter(5, 2)),
        // 沿用 78/84 的分母口径，只抖分子（幅度远小于 84，不会越界成 >100%）。
        cacheHitRate: (78 + jitter(6, 5)) / 84,
      },
      requestTrend: trendBuckets,
      // FR-143：下载累计趋势（与 requestTrend 同桶口径；预览数据取请求趋势的下载分量做基线，
      // 保证「下载 ≤ 请求」的直觉关系，且刷新时随快照序号抖动）。
      downloadTrend: trendBuckets.map((bucket, index) => ({
        from: bucket.from,
        to: bucket.to,
        downloadCount: Math.max(
          0,
          Math.round(bucket.downloadCount * (0.6 + jitter(index + 40, 3) / 20)),
        ),
      })),
      capacityTrend: capacityTrendBuckets,
      alerts: [
        {
          code: "upstream_auto_blocked",
          severity: "warning",
          source: "repository:maven-papermc",
          observedAt: now.toISOString(),
          firstObservedAt: blockedAt(26),
          blockedUntil,
        },
        {
          code: "upstream_auto_blocked",
          severity: "warning",
          source: "repository:maven-central",
          observedAt: now.toISOString(),
          firstObservedAt: blockedAt(51),
          blockedUntil,
        },
        {
          code: "upstream_auto_blocked",
          severity: "warning",
          source: "repository:maven-airgame",
          observedAt: now.toISOString(),
          firstObservedAt: blockedAt(77),
          blockedUntil,
        },
        {
          code: "upstream_auto_blocked",
          severity: "critical",
          source: "repository:docker-hub",
          observedAt: now.toISOString(),
          firstObservedAt: blockedAt(103),
          blockedUntil,
        },
        {
          code: "upstream_auto_blocked",
          severity: "warning",
          source: "repository:pypi-mirror",
          observedAt: now.toISOString(),
          firstObservedAt: blockedAt(132),
          blockedUntil,
        },
      ],
    });
  }),

  http.get("*/api/v1/observability/downloads/by-client", () => {
    // FR-144：来源聚合（独立来源口径）。预览数据给固定来源集 + 快照抖动，
    // 便于验证 Top 排名与族分布渲染；真实口径以服务端为准。
    const tick = nextSnapshotTick("download-clients");
    const jitter = (salt: number, amplitude: number) => snapshotJitter(tick, salt, amplitude);
    return HttpResponse.json({
      topIps: [
        { ip: "10.0.0.12", count: 46 + jitter(70, 8) },
        { ip: "10.0.0.31", count: 28 + jitter(71, 6) },
        { ip: "10.0.0.7", count: 19 + jitter(72, 5) },
        { ip: "10.0.1.5", count: 9 + jitter(73, 3) },
      ],
      families: [
        { family: "maven", count: 62 + jitter(74, 9) },
        { family: "gradle", count: 24 + jitter(75, 5) },
        { family: "curl", count: 11 + jitter(76, 3) },
        { family: "npm", count: 6 + jitter(77, 2) },
        { family: "browser", count: 3 + jitter(78, 1) },
      ],
    });
  }),

  http.get("*/api/v1/observability/downloads/trend", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const url = new URL(request.url);
    const window = downloadTrendWindow(url);
    if (!window) return err("bad_request", "下载趋势时间范围无效", 400);
    const rawGroupBy = url.searchParams.get("groupBy") ?? "family";
    if (rawGroupBy !== "family" && rawGroupBy !== "ip") {
      return err("bad_request", "groupBy 仅支持 family 或 ip", 400);
    }
    const groupBy = rawGroupBy;
    const repo = url.searchParams.get("repo");
    const groups =
      groupBy === "family"
        ? ["maven", "gradle", "docker", "npm", "pip"]
        : ["10.0.2.15", "10.0.2.28", "10.0.3.7", "10.0.4.19", "10.0.5.3"];
    const effectiveBucket = downloadTrendBucket(window);
    const buckets = downloadTrendBuckets(window, effectiveBucket);
    const points: DownloadGroupedTrendResponse["points"] = [];
    if (!isEmptyScenario(request) && (repo === null || store.findRepository(repo))) {
      const repoSalt = repo === null ? 0 : downloadTrendHash(repo) % 7;
      const bases = groupBy === "family" ? [4, 3, 3, 2, 1] : [3, 3, 2, 2, 1];
      for (let bucketIndex = 0; bucketIndex < buckets.length; bucketIndex += 1) {
        const bucket = buckets[bucketIndex]!;
        for (let groupIndex = 0; groupIndex < groups.length; groupIndex += 1) {
          const wave = Math.sin(bucketIndex / 2.4 + groupIndex * 0.85);
          const count = Math.max(
            0,
            Math.round(
              bases[groupIndex]! +
                wave * 1.6 +
                ((bucketIndex * 7 + groupIndex * 3 + repoSalt) % 3) -
                1,
            ),
          );
          if (count > 0) points.push({ ...bucket, group: groups[groupIndex]!, count });
        }
      }
    }
    // 饼图 totals 必须逐组等于 points 的全窗口求和，并按降序返回，确保饼图与时序总量一致。
    const totals = [
      ...points.reduce((counts, point) => {
        counts.set(point.group, (counts.get(point.group) ?? 0) + point.count);
        return counts;
      }, new Map<string, number>()),
    ]
      .map(([group, count]) => ({ group, count }))
      .sort((left, right) => right.count - left.count || left.group.localeCompare(right.group));
    const response: DownloadGroupedTrendResponse = {
      from: window.from,
      to: window.to,
      effectiveBucket,
      groupBy,
      points,
      totals,
    };
    return HttpResponse.json(response);
  }),

  http.get("*/api/v1/observability/host", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) return denied;
    const now = new Date();
    const minute = new Date(now.getTime() - 60_000).toISOString();
    // 与 dashboard 同源：每次请求前移快照序号，让刷新后 CPU/内存/网络曲线都有可见变化。
    const tick = nextSnapshotTick("host");
    const jitter = (salt: number, amplitude: number) => snapshotJitter(tick, salt, amplitude);
    // 按请求窗口自适应生成 48 个采样桶（桶宽 = 窗口/48），任何档位都有足够样本支撑拖选聚焦。
    const url = new URL(request.url);
    const parsedFrom = Date.parse(url.searchParams.get("from") ?? "");
    const parsedTo = Date.parse(url.searchParams.get("to") ?? "");
    const windowTo = Number.isFinite(parsedTo) ? parsedTo : now.getTime();
    const windowFrom = Number.isFinite(parsedFrom)
      ? Math.min(parsedFrom, windowTo - 3_600_000)
      : windowTo - 86_400_000;
    const span = Math.max(windowTo - windowFrom, 3_600_000);
    // 选定网卡（interface 参数）：省略时速率与总量按全部非回环网卡聚合；指定时只按该网卡。
    const requestedInterface = url.searchParams.get("interface");
    const SAMPLES = 48 * mockVolumeFactor();
    const step = span / SAMPLES;
    const samples: components["schemas"]["HostMetricPoint"][] = Array.from(
      { length: SAMPLES },
      (_, i) => {
        const end = new Date(windowFrom + step * (i + 1));
        const start = new Date(end.getTime() - step);
        const wave = Math.sin(i / 3.2);
        const drift = Math.sin(i / 11);
        // 采样序号取模 48：数据量档位会放大桶数（大档 768），若不取模，
        // 单调递减的内存 / 磁盘序列会穿过 0 变成负数，曲线形状也不对。
        const slot = i % 48;
        // 容量字段与后端同口径：已用 = 同一采样点 total − available（前端只展示不复算）。
        // 桶序取模 48 使 available 呈锯齿，已用随 slot 单调上升后再回落，与既有曲线一致。
        const memoryTotal = 17179869184;
        const memoryAvailable =
          6871947673 - slot * 1024 * 1024 * 37 + jitter(100 + slot, 120 * 1024 * 1024);
        const diskTotal = 1073741824000; // 数据目录所在卷固定 1 TiB
        const diskAvailable =
          536870912000 - slot * 1024 * 1024 * 173 + jitter(200 + slot, 400 * 1024 * 1024);
        // 逐网卡累计总量 = 基数 + 常量速率 × 累计时长，不加抖动：保证总量随采样单调不减，
        // 与后端「自网卡启动以来的累计计数」一致。速率仍带抖动，让曲线有可见变化。
        const elapsedSeconds = (step * (i + 1)) / 1000;
        const interfaces = MOCK_HOST_INTERFACES.map((iface, interfaceIndex) => {
          const shape = 1 + 0.4 * Math.sin(i / 3.2 + interfaceIndex);
          return {
            name: iface.name,
            receiveBytesTotal: iface.receiveBase + Math.round(iface.receiveRate * elapsedSeconds),
            transmitBytesTotal:
              iface.transmitBase + Math.round(iface.transmitRate * elapsedSeconds),
            receiveBytesPerSecond: Math.max(
              0,
              Math.round(iface.receiveRate * shape) + jitter(300 + interfaceIndex * 20 + slot, 900),
            ),
            transmitBytesPerSecond: Math.max(
              0,
              Math.round(iface.transmitRate * shape) +
                jitter(400 + interfaceIndex * 20 + slot, 500),
            ),
          };
        });
        // 全部网卡聚合 = 各网卡求和（与后端 host_metric_minute 的聚合口径一致）。
        const aggregate = interfaces.reduce(
          (total, item) => ({
            receiveBytesPerSecond: total.receiveBytesPerSecond + item.receiveBytesPerSecond,
            transmitBytesPerSecond: total.transmitBytesPerSecond + item.transmitBytesPerSecond,
            receiveBytesTotal: total.receiveBytesTotal + item.receiveBytesTotal,
            transmitBytesTotal: total.transmitBytesTotal + item.transmitBytesTotal,
          }),
          {
            receiveBytesPerSecond: 0,
            transmitBytesPerSecond: 0,
            receiveBytesTotal: 0,
            transmitBytesTotal: 0,
          },
        );
        const selected = requestedInterface
          ? interfaces.find((item) => item.name === requestedInterface)
          : undefined;
        // 指定网卡时速率与总量都按该网卡；未知网卡不留假 0，整组置 null。
        const network = requestedInterface
          ? {
              receiveBytesPerSecond: selected?.receiveBytesPerSecond ?? null,
              transmitBytesPerSecond: selected?.transmitBytesPerSecond ?? null,
              receiveBytesTotal: selected?.receiveBytesTotal ?? null,
              transmitBytesTotal: selected?.transmitBytesTotal ?? null,
            }
          : aggregate;
        return {
          from: start.toISOString(),
          to: end.toISOString(),
          hostState: { state: "ok" },
          networkState: { state: "ok" },
          processState: { state: "ok" },
          readinessState: { state: "ok" },
          cpuPercent:
            Math.round(
              Math.max(5, Math.min(95, 42 + wave * 14 + drift * 10 + jitter(10 + slot, 6))) * 10,
            ) / 10,
          memoryTotalBytes: memoryTotal,
          memoryAvailableBytes: memoryAvailable,
          memoryUsedBytes: memoryTotal - memoryAvailable,
          diskTotalBytes: diskTotal,
          diskAvailableBytes: diskAvailable,
          diskUsedBytes: diskTotal - diskAvailable,
          networkReceiveBytesPerSecond: network.receiveBytesPerSecond,
          networkTransmitBytesPerSecond: network.transmitBytesPerSecond,
          networkReceiveBytesTotal: network.receiveBytesTotal,
          networkTransmitBytesTotal: network.transmitBytesTotal,
          processRssBytes: 67108864 + slot * 1024 * 512 + jitter(500 + slot, 3 * 1024 * 1024),
          processCpuPercent:
            Math.round(
              Math.max(0, 1.2 + wave * 0.5 + drift * 0.8 + jitter(600 + slot, 0.4)) * 100,
            ) / 100,
          // 进程运行时长（挂钟口径，仅本进程）：随桶时间单调增长，序列起点为 1 小时。
          processUptimeSeconds: Math.max(60, Math.round(((i + 1) * step) / 1000) + 3600),
          goroutineCount: Math.max(1, 37 + (slot % 4) + jitter(700 + slot, 3)),
        };
      },
    );
    const latest = samples[SAMPLES - 1];
    // 可用网卡列表与各网卡累计总量取窗口末端：选择器需要**全部**网卡，故不受 interface 过滤影响。
    const lastElapsedSeconds = (step * SAMPLES) / 1000;
    const response: components["schemas"]["HostMonitoring"] = {
      hostState: "healthy",
      latestSampleAt: minute,
      effectiveBucket: "minute",
      latest,
      samples,
      networkInterfaces: MOCK_HOST_INTERFACES.map((iface) => ({
        name: iface.name,
        receiveBytesTotal: iface.receiveBase + Math.round(iface.receiveRate * lastElapsedSeconds),
        transmitBytesTotal:
          iface.transmitBase + Math.round(iface.transmitRate * lastElapsedSeconds),
      })),
    };
    return HttpResponse.json(response);
  }),

  // FR-38：管理审计日志（非 OpenAPI 端点，仅管理员可读）。
  http.get("*/api/v1/audit-logs", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    if (isEmptyScenario(request)) {
      return HttpResponse.json({ items: [], total: 0 });
    }
    const url = new URL(request.url);
    const limitParam = integerQuery(url, "limit");
    const offsetParam = integerQuery(url, "offset");
    if (limitParam.invalid || offsetParam.invalid) {
      return err("bad_request", "limit、offset 须为整数", 400);
    }
    const { items } = mockAuditLogList();
    const from = url.searchParams.get("from");
    const to = url.searchParams.get("to");
    const filtered = items.filter(
      (item) =>
        (!url.searchParams.get("actor") || item.actor === url.searchParams.get("actor")) &&
        (!url.searchParams.get("userId") ||
          String(item.userId ?? "") === url.searchParams.get("userId")) &&
        (!url.searchParams.get("authSource") ||
          item.authSource === url.searchParams.get("authSource")) &&
        (!url.searchParams.get("tokenId") ||
          String(item.tokenId ?? "") === url.searchParams.get("tokenId")) &&
        (!url.searchParams.get("action") || item.action === url.searchParams.get("action")) &&
        (!url.searchParams.get("repo") || item.repo === url.searchParams.get("repo")) &&
        (!url.searchParams.get("result") || item.result === url.searchParams.get("result")) &&
        (!url.searchParams.get("ip") || item.ip === url.searchParams.get("ip")) &&
        (!from || item.ts >= from) &&
        (!to || item.ts <= to),
    );
    const requestedLimit = limitParam.value ?? 50;
    const limit = requestedLimit > 0 && requestedLimit <= 200 ? requestedLimit : 50;
    const requestedOffset = offsetParam.value ?? 0;
    const offset = requestedOffset >= 0 ? requestedOffset : 0;
    // 不做档位放大：该端点当前**无前端消费者**（审计中心走 observability 端点），
    // 而"只在响应里放大"会产出与种子共享 eventId 的克隆（`scaleList` 只偏移数值 id），
    // 一旦被接线就是「按 id 钻取命中另一条事件」的陷阱。域放大统一走 store 物化。
    return HttpResponse.json({
      items: filtered.slice(offset, offset + limit),
      total: filtered.length,
    });
  }),

  http.post("*/api/v1/repositories", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as CreateRepoBody;
    if (!body.name || !body.format || !body.type) {
      return err("bad_request", "名称 / 格式 / 类型必填", 400);
    }
    const repo = store.createRepository({
      name: body.name,
      format: body.format,
      type: body.type,
      visibility: body.visibility,
      description: body.description,
      remoteUrl: body.remoteUrl,
      members: body.members,
      aliases: body.aliases,
    });
    if (!repo) {
      return err("conflict", "仓库名已存在", 409);
    }
    return HttpResponse.json(repo, { status: 201 });
  }),

  // FR-32：开发态返回当前实例启动时启用的格式。
  http.get("*/api/v1/formats/enabled", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    return HttpResponse.json({ formats: ["raw", "maven", "npm"] });
  }),

  http.patch("*/api/v1/repositories/:name", async ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      visibility?: Repository["visibility"];
      description?: string;
      remoteUrl?: string;
      members?: string[];
      aliases?: string[];
    };
    const repo = store.updateRepository(String(params.name), body);
    return repo ? HttpResponse.json(repo) : err("not_found", "仓库不存在", 404);
  }),

  // 仓库重命名（仅管理员）：旧名自动转为别名，仍可通过旧名解析到该仓库。
  http.post("*/api/v1/repositories/:name/rename", async ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as { newName?: string };
    if (!body.newName || !body.newName.trim()) {
      return err("bad_request", "新名称不能为空", 400);
    }
    const result = store.renameRepository(String(params.name), body.newName.trim());
    if (result === null) {
      return err("not_found", "仓库不存在", 404);
    }
    if (result === "conflict") {
      return err("conflict", "名称已被占用", 409);
    }
    return HttpResponse.json(result);
  }),

  http.delete("*/api/v1/repositories/:name", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    return store.deleteRepository(String(params.name))
      ? new HttpResponse(null, { status: 204 })
      : err("not_found", "仓库不存在", 404);
  }),

  http.post("*/api/v1/repositories/:name/cleanup", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const deleted = store.cleanupEmptyMavenArtifacts(String(params.name));
    if (deleted === null) {
      return err("not_found", "仓库不存在", 404);
    }
    if (deleted === "conflict") {
      return err("bad_request", "仅 Maven hosted 仓库可清理", 400);
    }
    return HttpResponse.json({ deleted });
  }),

  // FR-113：仓库 online/offline 开关（仅管理员；本地运维状态，不参与复制）。
  http.put("*/api/v1/repositories/:name/online", async ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as { online?: boolean };
    if (typeof body.online !== "boolean") {
      return err("bad_request", "online 必填", 400);
    }
    const repo = store.setOnline(String(params.name), body.online);
    return repo ? HttpResponse.json(repo) : err("not_found", "仓库不存在", 404);
  }),

  // FR-114：手动重测仓库上游连接（仅管理员；仅 online 的 proxy 可重测）。
  http.post("*/api/v1/repositories/:name/recheck-connection", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const repo = store.findRepository(String(params.name));
    if (!repo) {
      return err("not_found", "仓库不存在", 404);
    }
    if (repo.type !== "proxy") {
      return err("bad_request", "仅 proxy 仓库可重测连接", 400);
    }
    if (repo.online === false) {
      return err("bad_request", "仓库已离线，请先上线后再重测", 400);
    }
    const status = store.recheckConnection(String(params.name));
    return status ? HttpResponse.json(status) : err("bad_request", "无法重测连接", 400);
  }),

  http.get("*/api/v1/repositories/:name/acl", ({ request, params }) => {
    const denied = repositoryActionUnauthorized(request, String(params.name), "admin");
    if (denied) {
      return denied;
    }
    if (isEmptyScenario(request)) {
      return store.findRepository(String(params.name))
        ? HttpResponse.json({ items: [] })
        : err("not_found", "仓库不存在", 404);
    }
    const items = store.getAcl(String(params.name));
    return items ? HttpResponse.json({ items }) : err("not_found", "仓库不存在", 404);
  }),

  http.put("*/api/v1/repositories/:name/acl", async ({ request, params }) => {
    const denied = repositoryActionUnauthorized(request, String(params.name), "admin");
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as { items?: AclEntry[] };
    if (!Array.isArray(body.items)) {
      return err("bad_request", "items 必填", 400);
    }
    const items = store.setAcl(String(params.name), body.items);
    return items ? HttpResponse.json({ items }) : err("not_found", "仓库不存在", 404);
  }),

  http.get("*/api/v1/repositories/:name/assets", ({ request, params }) => {
    const denied = repositoryReadUnauthorized(request, String(params.name));
    if (denied) {
      return denied;
    }
    if (isEmptyScenario(request)) {
      return store.findRepository(String(params.name))
        ? HttpResponse.json({ items: [], total: 0 })
        : err("not_found", "仓库不存在", 404);
    }
    const url = new URL(request.url);
    const result = store.listAssets(
      String(params.name),
      url.searchParams.get("prefix") ?? "",
      intParam(url, "page", 1),
      intParam(url, "page_size", 20),
    );
    return result ? HttpResponse.json(result) : err("not_found", "仓库不存在", 404);
  }),

  http.post("*/api/v1/repositories/:name/assets/operations", async ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as AssetOperationBody;
    if (!isValidAssetOperationBody(body)) {
      return err("bad_request", "制品操作参数不合法", 400);
    }
    const result = store.applyAssetOperation(String(params.name), {
      action: body.action,
      targets: body.targets.map((target) => ({ type: target.type!, path: target.path! })),
      destinationPath: body.destinationPath,
      newPath: body.newPath,
    });
    if (result === null) {
      return err("not_found", "仓库不存在", 404);
    }
    if (result === "invalid") {
      return err("bad_request", "制品操作参数不合法", 400);
    }
    if (result === "conflict") {
      return err("conflict", "制品操作与当前仓库状态冲突", 409);
    }
    return HttpResponse.json(result);
  }),

  // FR-103：已弃用的批量删除兼容入口，委托统一原子操作。
  http.post("*/api/v1/repositories/:name/assets/batch-delete", async ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      paths?: string[];
      overrideReason?: string;
    };
    if (!Array.isArray(body.paths) || body.paths.length === 0) {
      return err("bad_request", "paths 不能为空", 400);
    }
    if (body.paths.length > 500) {
      return err("bad_request", "paths 单次最多 500 条", 400);
    }
    if (body.paths.some((p) => typeof p !== "string" || p === "")) {
      return err("bad_request", "paths 中存在空路径", 400);
    }
    if (!body.overrideReason?.trim() || body.overrideReason.length > 512) {
      return err("bad_request", "overrideReason 必填且长度不得超过 512", 400);
    }
    const result = store.applyAssetOperation(String(params.name), {
      action: "delete",
      targets: body.paths.map((path) => ({ type: "raw_path", path })),
    });
    if (result === null) {
      return err("not_found", "仓库不存在", 404);
    }
    if (result === "invalid") {
      return err("bad_request", "制品操作参数不合法", 400);
    }
    if (result === "conflict") {
      return err("conflict", "制品操作与当前仓库状态冲突", 409);
    }
    return HttpResponse.json({ deleted: result.affected, failed: [] });
  }),

  // 仓库下载趋势沿用仓库树的读取权限，返回全时段累计与所选小时窗口趋势。
  http.get("*/api/v1/repositories/:name/download-trend", ({ request, params }) => {
    const name = String(params.name);
    const denied = repositoryReadUnauthorized(request, name);
    if (denied) return denied;
    const window = downloadTrendWindow(new URL(request.url));
    if (!window) return err("bad_request", "下载趋势时间范围无效", 400);
    const effectiveBucket = downloadTrendBucket(window);
    const buckets = downloadTrendBuckets(window, effectiveBucket);
    const hash = downloadTrendHash(name);
    // 累计值是全时段口径；仓库名哈希只用于稳定区分预览数据，不与所选 24 小时趋势相加。
    const totalDownloadCount = isEmptyScenario(request) ? 0 : 120_000 + (hash % 40_000);
    const trend: DownloadTrendPoint[] = buckets.map((bucket, index) => {
      const isQuietBucket = (index + (hash % 11)) % 17 === 0;
      const wave = Math.sin(index / 2.3 + (hash % 9));
      return {
        ...bucket,
        downloadCount: isEmptyScenario(request)
          ? 0
          : isQuietBucket
            ? 0
            : Math.max(0, Math.round(34 + (hash % 13) + wave * 18 + ((index * 5) % 9) - 4)),
      };
    });
    const response: RepositoryDownloadTrendResponse = {
      from: window.from,
      to: window.to,
      effectiveBucket,
      totalDownloadCount,
      trend,
    };
    return HttpResponse.json(response);
  }),

  // FR-54：目录懒加载。匿名仅在全局开关开启且仓库 public 时可读。
  http.get("*/api/v1/repositories/:name/tree", ({ request, params }) => {
    const name = String(params.name);
    const denied = repositoryReadUnauthorized(request, name);
    if (denied) {
      return denied;
    }
    if (isEmptyScenario(request)) {
      return store.findRepository(name)
        ? HttpResponse.json({ directories: [], files: [] })
        : err("not_found", "仓库不存在", 404);
    }
    const url = new URL(request.url);
    const entry = store.listDirectory(name, url.searchParams.get("prefix") ?? "");
    return entry ? HttpResponse.json(entry) : err("not_found", "仓库不存在", 404);
  }),

  http.get("*/api/v1/repositories/:name/usage", ({ request, params }) => {
    const denied = repositoryReadUnauthorized(request, String(params.name));
    if (denied) {
      return denied;
    }
    const url = new URL(request.url);
    const result = store.usage(String(params.name), `${url.protocol}//${url.host}`);
    return result ? HttpResponse.json(result) : err("not_found", "仓库不存在", 404);
  }),

  // FR-73：Maven 网页上传（服务端生成 pom/校验和/metadata；mock 仅登记 asset 摘要）。
  http.post("*/api/v1/repositories/:name/maven-upload", async ({ request, params }) => {
    const denied = repositoryActionUnauthorized(request, String(params.name), "write");
    if (denied) {
      return denied;
    }
    const form = await request.formData();
    const groupId = String(form.get("groupId") ?? "").trim();
    const artifactId = String(form.get("artifactId") ?? "").trim();
    const version = String(form.get("version") ?? "").trim();
    const packaging = String(form.get("packaging") ?? "").trim() || "jar";
    const file = form.get("file");
    if (!groupId || !artifactId || !version || !(file instanceof File)) {
      return err("invalid_gav", "groupId/artifactId/version 与 file 必填", 400);
    }
    if (version.toUpperCase().includes("-SNAPSHOT")) {
      return err("snapshot_not_supported", "网页上传仅限 release 版本", 400);
    }
    const result = store.uploadMavenArtifact(
      String(params.name),
      groupId,
      artifactId,
      version,
      packaging,
      file.size,
    );
    if (result === null) {
      return err("not_found", "仓库不存在", 404);
    }
    if (result === "conflict") {
      return err("not_maven_hosted", "仅 Maven hosted 仓库支持网页上传", 409);
    }
    return HttpResponse.json(result, { status: 201 });
  }),

  // —— Raw 协议写入（开发态只维护浏览树摘要）——
  http.put("*/repository/*", async ({ request }) => {
    const target = rawProtocolTarget(request);
    if (!target) {
      return err("not_found", "制品路径不存在", 404);
    }
    const denied = repositoryActionUnauthorized(request, target.repository, "write");
    if (denied) {
      return denied;
    }
    const content = await request.arrayBuffer();
    const asset = store.putRawAsset(
      target.repository,
      target.path,
      content.byteLength,
      request.headers.get("content-type") ?? "application/octet-stream",
    );
    if (asset === null) {
      return err("not_found", "仓库不存在", 404);
    }
    if (asset === "conflict") {
      return err("conflict", "仅 Raw hosted 仓库支持此协议写入", 409);
    }
    return HttpResponse.json({
      repository: target.repository,
      path: asset.path,
      hash: asset.hash,
      size: asset.size,
      contentType: asset.contentType,
    });
  }),

  http.delete("*/repository/*", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const target = rawProtocolTarget(request);
    if (!target) {
      return err("not_found", "制品路径不存在", 404);
    }
    const deleted = store.deleteRawAsset(target.repository, target.path);
    if (deleted === null) {
      return err("not_found", "仓库不存在", 404);
    }
    if (deleted === "conflict") {
      return err("conflict", "仅 Raw hosted 仓库支持此协议删除", 409);
    }
    return deleted ? new HttpResponse(null, { status: 204 }) : err("not_found", "制品不存在", 404);
  }),

  // —— 迁移任务（0.4.0 foundation）——
  http.get("*/api/v1/migrations", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    if (isEmptyScenario(request)) {
      return HttpResponse.json({ items: [], total: 0 });
    }
    prepareMigrationFixtureScenario(request);
    const url = new URL(request.url);
    return HttpResponse.json(
      store.listMigrations(intParam(url, "page", 1), intParam(url, "page_size", 20)),
    );
  }),

  http.post("*/api/v1/migrations", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      sourceType?: MigrationTask["sourceType"];
      sourceConfig?: MigrationSourceConfig;
      sourceAuth?: MigrationSourceAuth;
      credentialRef?: string;
      conflictPolicy?: MigrationTask["conflictPolicy"];
      plan?: MigrationPlan;
    };
    if (!body.sourceType) {
      return err("bad_request", "sourceType 必填", 400);
    }
    const task = store.createMigration({
      sourceType: body.sourceType,
      sourceConfig: body.sourceConfig,
      sourceAuth: body.sourceAuth,
      credentialRef: body.credentialRef,
      conflictPolicy: body.conflictPolicy,
      plan: body.plan,
    });
    return HttpResponse.json(task, { status: 201 });
  }),

  http.post("*/api/v1/migrations/discover", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      sourceType?: MigrationTask["sourceType"];
      sourceConfig?: MigrationSourceConfig;
      sourceAuth?: MigrationSourceAuth;
      credentialRef?: string;
      conflictPolicy?: MigrationTask["conflictPolicy"];
    };
    if (!body.sourceType) {
      return err("bad_request", "sourceType 必填", 400);
    }
    return HttpResponse.json(
      store.discoverMigration({
        sourceType: body.sourceType,
        sourceConfig: body.sourceConfig,
        sourceAuth: body.sourceAuth,
        credentialRef: body.credentialRef,
        conflictPolicy: body.conflictPolicy,
      }),
    );
  }),

  http.post("*/api/v1/migrations/remote-repositories", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      sourceConfig?: RemoteNexusSourceConfig;
      sourceAuth?: MigrationSourceAuth;
      credentialRef?: string;
    };
    if (!body.sourceConfig) {
      return err("bad_request", "sourceConfig 必填", 400);
    }
    return HttpResponse.json({
      items: [
        { name: "maven-releases", format: "maven", type: "hosted" },
        { name: "npm-hosted", format: "npm", type: "hosted" },
      ],
      total: 2,
    });
  }),

  http.get("*/api/v1/migrations/:id", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const id = Number(params.id);
    if (isEmptyScenario(request)) {
      const task = emptyMigrationTask(id);
      return task ? HttpResponse.json(task) : err("not_found", "任务不存在", 404);
    }
    prepareMigrationFixtureScenario(request);
    const task = store.findMigration(id);
    return task ? HttpResponse.json(task) : err("not_found", "任务不存在", 404);
  }),

  http.post("*/api/v1/migrations/:id/start", async ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    prepareMigrationFixtureScenario(request);
    const body = (await request.json().catch(() => ({}))) as {
      includeRepositories?: string[];
    };
    const result = store.startMigration(Number(params.id), body.includeRepositories);
    if (result === "not_found") {
      return err("not_found", "任务不存在", 404);
    }
    if (result === "conflict") {
      return err("conflict", "仅 planned 可 start", 409);
    }
    return HttpResponse.json(result);
  }),

  http.post("*/api/v1/migrations/:id/resume", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    prepareMigrationFixtureScenario(request);
    const result = store.resumeMigration(Number(params.id));
    if (result === "not_found") {
      return err("not_found", "任务不存在", 404);
    }
    if (result === "conflict") {
      return err("conflict", "仅 failed/cancelled 可 resume", 409);
    }
    return HttpResponse.json(result);
  }),

  http.post("*/api/v1/migrations/:id/cancel", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    prepareMigrationFixtureScenario(request);
    const result = store.cancelMigration(Number(params.id));
    if (result === "not_found") {
      return err("not_found", "任务不存在", 404);
    }
    if (result === "conflict") {
      return err("conflict", "当前状态不可 cancel", 409);
    }
    return HttpResponse.json(result);
  }),

  http.get("*/api/v1/migrations/:id/report", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const id = Number(params.id);
    if (isEmptyScenario(request)) {
      const report = emptyMigrationReport(id);
      return report ? HttpResponse.json(report) : err("not_found", "任务不存在", 404);
    }
    prepareMigrationFixtureScenario(request);
    const report = store.migrationReport(id);
    return report ? HttpResponse.json(report) : err("not_found", "任务不存在", 404);
  }),

  http.post("*/api/v1/migrations/:id/finalize", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    prepareMigrationFixtureScenario(request);
    const result = store.finalizeMigration(Number(params.id));
    if (result === "not_found") {
      return err("not_found", "任务不存在", 404);
    }
    if (result === "conflict") {
      return err("conflict", "仅 completed 可 finalize", 409);
    }
    return HttpResponse.json(result);
  }),

  // —— 节点备份与搬迁（FR-132）——
  // —— 写入冻结窗口（FR-134，admin）——
  // 冻结期间节点拒绝本地业务与管理写入，但放行读 / 登录 / 备份导入路径。
  http.get(
    "*/api/v1/maintenance/freeze",
    ({ request }) => adminUnauthorized(request) ?? HttpResponse.json(mockFreeze),
  ),

  http.post("*/api/v1/maintenance/freeze", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      until?: string;
      ttlSeconds?: number;
      reason?: string;
    };
    const now = new Date();
    // 窗口必须有界：until 优先；否则 ttlSeconds（缺省 7200，上限 24h）；契约层也不允许无限期。
    let until: string;
    if (typeof body.until === "string" && body.until.length > 0) {
      until = body.until;
    } else {
      const ttl = Math.min(Math.max(body.ttlSeconds ?? 7200, 60), 86400);
      until = new Date(now.getTime() + ttl * 1000).toISOString();
    }
    mockFreeze = {
      frozen: true,
      frozenAt: now.toISOString(),
      until,
      ...(body.reason ? { reason: body.reason } : {}),
    };
    return HttpResponse.json(mockFreeze);
  }),

  http.delete("*/api/v1/maintenance/freeze", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    // 幂等：未冻结时同样返回 200 与当前（未冻结）状态。
    mockFreeze = { frozen: false };
    return HttpResponse.json(mockFreeze);
  }),

  // —— 从 URL 导入备份包（FR-137，admin）——
  // 注意路由注册顺序：必须早于 `*/api/v1/backups/:id`，否则 /backups/imports
  // 会被 :id 通配吞掉（与 gin 上实测一致的静默吞路由问题）。
  http.post("*/api/v1/backups/import", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      sourceUrl?: string;
      overwrite?: boolean;
      deep?: boolean;
      expectedSha256?: string;
    };
    if (!body.sourceUrl || body.sourceUrl.trim().length === 0) {
      return err("bad_request", "备份包地址必填", 400);
    }
    const operator = store.findUser(mockUserId(request) ?? 0)?.username ?? "admin";
    const importId = `imp-${new Date()
      .toISOString()
      .replace(/[-:T.]/g, "")
      .slice(0, 14)}-${String(++mockImportSeq).padStart(3, "0")}`;
    const record: BackupImport = {
      importId,
      origin: "url",
      status: "queued",
      sourceUrl: body.sourceUrl,
      operator,
      overwrite: body.overwrite ?? false,
      deep: body.deep ?? false,
      totalBytes: 73_400_320,
      fetchedBytes: 0,
      blobCount: 0,
      packageId: "",
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    };
    mockImports.unshift(record);
    advanceMockImport(importId);
    // 受理即返回 202；拉取进度由定时器推进，前端轮询可见。
    return HttpResponse.json(record, { status: 202 });
  }),

  http.get("*/api/v1/backups/imports", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const url = new URL(request.url);
    const page = intParam(url, "page", 1);
    const pageSize = intParam(url, "page_size", 20);
    // 最近优先：按创建时间倒序。
    const sorted = [...mockImports].sort(
      (a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt),
    );
    const start = (page - 1) * pageSize;
    return HttpResponse.json({
      items: sorted.slice(start, start + pageSize),
      total: sorted.length,
    });
  }),

  http.get("*/api/v1/backups/imports/:id", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const found = mockImports.find((item) => item.importId === String(params.id));
    return found ? HttpResponse.json(found) : err("not_found", "导入记录不存在", 404);
  }),

  // —— 分片上传备份包（FR-137 第三通道，admin）——
  // 路由注册顺序：必须早于 `*/api/v1/backups/:id`，否则 `POST /api/v1/backups/uploads`
  // 会被 :id 通配吞掉（与 gin 上实测一致的静默吞路由问题，同 backups/imports 的处理）。
  http.post("*/api/v1/backups/uploads", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      fileName?: string;
      totalBytes?: number;
      sha256?: string;
    };
    if (!body.fileName || (body.totalBytes ?? 0) <= 0) {
      return err("bad_request", "fileName 与 totalBytes 必填", 400);
    }
    const uploadId = `up-${new Date()
      .toISOString()
      .replace(/[-:T.]/g, "")
      .slice(0, 14)}-${String(++mockUploadIdCounter).padStart(4, "0")}`;
    const session: MockUploadSession = {
      uploadId,
      fileName: body.fileName,
      totalBytes: body.totalBytes ?? 0,
      // 服务端决定的分片大小：前端必须用它切分，不得硬编码。
      chunkSize: UPLOAD_CHUNK_SIZE,
      uploadedChunks: [],
      status: "initialized",
      expiresAt: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
    };
    mockUploads[uploadId] = session;
    return HttpResponse.json(session, { status: 201 });
  }),

  http.put("*/api/v1/backups/uploads/:id/chunks/:index", async ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const session = mockUploads[String(params.id)];
    if (!session) {
      return err("not_found", "上传会话不存在", 404);
    }
    if (session.status === "completed" || session.status === "aborted") {
      return err("conflict", "会话已终态，无法继续上传", 409);
    }
    const totalChunks = Math.ceil(session.totalBytes / session.chunkSize);
    const index = Number(params.index);
    if (!Number.isInteger(index) || index < 0 || index >= totalChunks) {
      return err("chunk_index_out_of_range", `片号越界：应为 0..${totalChunks - 1}`, 400);
    }
    const buf = await request.arrayBuffer().catch(() => null);
    const size = buf?.byteLength ?? 0;
    if (size === 0) {
      return err("empty_chunk", "分片体为空", 400);
    }
    if (size > session.chunkSize) {
      return err("chunk_oversize", "单片体积超出 chunkSize 上限", 400);
    }
    session.status = "receiving";
    // uploadedChunks 以实际已收分片为准，按升序去重。
    if (!session.uploadedChunks.includes(index)) {
      session.uploadedChunks.push(index);
    }
    session.uploadedChunks.sort((a, b) => a - b);
    return HttpResponse.json(session, { status: 200 });
  }),

  http.get("*/api/v1/backups/uploads/:id", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const session = mockUploads[String(params.id)];
    // abort 后记录保留供审计：仍返回 200 + status=aborted，而非 404。
    return session ? HttpResponse.json(session) : err("not_found", "上传会话不存在", 404);
  }),

  http.post("*/api/v1/backups/uploads/:id/complete", async ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const session = mockUploads[String(params.id)];
    if (!session) {
      return err("not_found", "上传会话不存在", 404);
    }
    if (session.status === "aborted") {
      return err("conflict", "会话已取消，无法完成", 409);
    }
    const totalChunks = Math.ceil(session.totalBytes / session.chunkSize);
    const missing: number[] = [];
    for (let i = 0; i < totalChunks; i += 1) {
      if (!session.uploadedChunks.includes(i)) {
        missing.push(i);
      }
    }
    if (missing.length > 0) {
      return err("chunks_missing", `缺少分片：${missing.join(",")}`, 400);
    }
    const body = (await request.json().catch(() => ({}))) as {
      sha256?: string;
      overwrite?: boolean;
      deep?: boolean;
    };
    session.status = "completed";
    const operator = store.findUser(mockUserId(request) ?? 0)?.username ?? "admin";
    const importId = `imp-${new Date()
      .toISOString()
      .replace(/[-:T.]/g, "")
      .slice(0, 14)}-${String(++mockImportSeq).padStart(3, "0")}`;
    const record: BackupImport = {
      importId,
      origin: "upload",
      status: "queued",
      operator,
      overwrite: body.overwrite ?? false,
      deep: body.deep ?? false,
      totalBytes: session.totalBytes,
      fetchedBytes: session.totalBytes,
      blobCount: 0,
      packageId: "",
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    };
    mockImports.unshift(record);
    // 复用从 URL 导入的推进状态机：queued → fetching → staging → pending_restart。
    advanceMockImport(importId);
    return HttpResponse.json(record, { status: 202 });
  }),

  http.post("*/api/v1/backups/uploads/:id/abort", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const session = mockUploads[String(params.id)];
    if (!session) {
      return err("not_found", "上传会话不存在", 404);
    }
    // 置 aborted 并清分片；记录保留供审计（GET 仍返回 200 + status=aborted）。
    session.status = "aborted";
    session.uploadedChunks = [];
    return new HttpResponse(null, { status: 204 });
  }),

  http.get("*/api/v1/backups", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    if (isEmptyScenario(request)) {
      return HttpResponse.json({ items: [], total: 0 });
    }
    const url = new URL(request.url);
    const page = intParam(url, "page", 1);
    const pageSize = intParam(url, "page_size", 20);
    const start = (page - 1) * pageSize;
    return HttpResponse.json({
      items: mockBackups.slice(start, start + pageSize),
      total: mockBackups.length,
    });
  }),

  http.post("*/api/v1/backups", async ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      mode?: BackupPackage["mode"];
      label?: string;
    };
    if (body.mode !== "hot" && body.mode !== "frozen") {
      return err("validation_error", "未知备份模式", 400);
    }
    const created = createMockBackup(body.mode, body.label ?? "");
    mockBackups.unshift(created);
    advanceMockBackup(created.packageId);
    return HttpResponse.json(created, { status: 201 });
  }),

  http.get("*/api/v1/backups/:id", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const found = findMockBackup(String(params.id));
    return found ? HttpResponse.json(found) : err("not_found", "备份包不存在", 404);
  }),

  http.delete("*/api/v1/backups/:id", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const index = mockBackups.findIndex((item) => item.packageId === String(params.id));
    if (index < 0) {
      return err("not_found", "备份包不存在", 404);
    }
    mockBackups.splice(index, 1);
    return new HttpResponse(null, { status: 204 });
  }),

  http.post("*/api/v1/backups/:id/link", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const found = findMockBackup(String(params.id));
    if (!found) {
      return err("not_found", "备份包不存在", 404);
    }
    return HttpResponse.json({
      url: `/api/v1/backups/${found.packageId}/download?exp=${
        Math.floor(Date.now() / 1000) + 1800
      }&token=mock-signature`,
      expiresAt: new Date(Date.now() + 30 * 60 * 1000).toISOString(),
    });
  }),

  http.post("*/api/v1/backups/:id/verify", ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const found = findMockBackup(String(params.id));
    if (!found) {
      return err("not_found", "备份包不存在", 404);
    }
    const assets = found.counts?.assets ?? 0;
    return HttpResponse.json({
      ok: true,
      packageId: found.packageId,
      deep: new URL(request.url).searchParams.get("deep") === "true",
      blobs: assets,
      assets,
    });
  }),
];
