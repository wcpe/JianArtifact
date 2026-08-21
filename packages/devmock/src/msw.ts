// MSW 请求处理器：把 api/openapi.yaml 定义的 0.2.0 管理面端点映射到内存态 store。
// 浏览器（src/browser.ts）与 Node（src/node.ts）双端复用同一组 handlers，
// 保证 web 开发态脱离后端可跑、vitest 集成测试与真实 fetch 链路同源。
import { http, HttpResponse } from "msw";

import { mockAuditLogList, mockReplicationApplyLogList } from "./handlers";
import { MOCK_TOKEN, store } from "./store";
import type { AclEntry, MigrationPlan, MigrationTask, Repository, User } from "./store";

function err(code: string, message: string, status: number) {
  return HttpResponse.json({ error: { code, message } }, { status });
}

const MOCK_USER_TOKEN = "mock.jwt.token:user";

function bearerToken(request: Request) {
  const header = request.headers.get("Authorization") ?? "";
  return header.startsWith("Bearer ") ? header.slice("Bearer ".length) : "";
}

/** 受保护端点统一鉴权：仅接受 mock 管理员或普通用户令牌。 */
function unauthorized(request: Request) {
  const token = bearerToken(request);
  return token === MOCK_TOKEN || token === MOCK_USER_TOKEN
    ? null
    : err("unauthorized", "未认证", 401);
}

/** 管理员端点鉴权：未知令牌未认证，普通用户令牌无权限。 */
function adminUnauthorized(request: Request) {
  const token = bearerToken(request);
  if (token !== MOCK_TOKEN && token !== MOCK_USER_TOKEN) {
    return err("unauthorized", "未认证", 401);
  }
  return token === MOCK_TOKEN ? null : err("forbidden", "需要管理员权限", 403);
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

/** 集群状态 mock（FR-86，admin 端点）。 */
function mockClusterStatus() {
  return {
    nodeId: "mock-node",
    peerUrl: store.peerURLState(),
    tokenSet: store.peerTokenSet(),
    enabled: store.replicationEnabledState(),
    watermark: 12,
    hasWatermark: true,
    lastSyncAt: "2026-08-13T00:00:00Z",
  };
}

/** 同步历史 mock（FR-88，admin 端点）：含成功 / 进行中 / 失败样本与实体构成。 */
const mockSyncLogs = [
  {
    id: 3,
    peerUrl: "https://repo1.wcpe.top",
    startedAt: new Date(Date.now() - 30_000).toISOString(),
    finishedAt: new Date(Date.now() - 29_000).toISOString(),
    success: true,
    fromSeq: 10,
    toSeq: 15,
    changes: 5,
    applied: 5,
    failed: 0,
    blobs: 2,
    entityCounts: JSON.stringify({ repository: 2, user: 1, asset: 2 }),
    errorText: "",
  },
  {
    id: 2,
    peerUrl: "https://repo1.wcpe.top",
    startedAt: new Date(Date.now() - 5_000).toISOString(),
    finishedAt: null,
    success: null, // 进行中
    fromSeq: 15,
    toSeq: 15,
    changes: 0,
    applied: 0,
    failed: 0,
    blobs: 0,
    entityCounts: "{}",
    errorText: "",
  },
  {
    id: 1,
    peerUrl: "https://repo1.wcpe.top",
    startedAt: new Date(Date.now() - 120_000).toISOString(),
    finishedAt: new Date(Date.now() - 119_000).toISOString(),
    success: false,
    fromSeq: 5,
    toSeq: 5,
    changes: 0,
    applied: 0,
    failed: 0,
    blobs: 0,
    entityCounts: "{}",
    errorText: "拉取复制变更失败：HTTP 401",
  },
];

/** 开源协议清单 mock（admin 端点；真实数据由后端内嵌 JSON 返回）。 */
const MOCK_LICENSES = {
  generatedAt: "2026-01-01T00:00:00Z",
  go: [
    { name: "github.com/gin-gonic/gin", version: "v1.10.0", license: "MIT", author: "gin-gonic" },
    { name: "modernc.org/sqlite", version: "v1.34.0", license: "BSD-3-Clause", author: "modernc" },
  ],
  npm: [
    { name: "@mantine/core", version: "7.15.0", license: "MIT", author: "Mantine" },
    { name: "react", version: "18.3.1", license: "MIT", author: "Meta" },
  ],
};

function intParam(url: URL, key: string, fallback: number): number {
  const raw = url.searchParams.get(key);
  const n = raw === null ? NaN : Number.parseInt(raw, 10);
  return Number.isFinite(n) && n > 0 ? n : fallback;
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
}
interface CreateRepoBody {
  name?: string;
  format?: Repository["format"];
  type?: Repository["type"];
  visibility?: Repository["visibility"];
  description?: string;
  remoteUrl?: string;
  members?: string[];
}

export const handlers = [
  // —— 健康 / 状态（公开）——
  http.get("*/healthz", () => HttpResponse.json({ status: "ok", version: store.status().version })),
  http.get("*/readyz", () => HttpResponse.json({ status: "ok", version: store.status().version })),
  http.get("*/api/v1/status", () => HttpResponse.json(store.status())),

  // —— 开源协议清单（admin 专属，非契约）——
  http.get(
    "*/api/v1/licenses",
    ({ request }) => unauthorized(request) ?? HttpResponse.json(MOCK_LICENSES),
  ),

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
    return HttpResponse.json({ token: MOCK_TOKEN, user }, { status: 200 });
  }),

  http.post(
    "*/api/v1/auth/logout",
    ({ request }) => unauthorized(request) ?? new HttpResponse(null, { status: 204 }),
  ),

  // —— 用户 ——
  http.get("*/api/v1/users", ({ request }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const url = new URL(request.url);
    return HttpResponse.json(
      store.listUsers(intParam(url, "page", 1), intParam(url, "page_size", 20)),
    );
  }),

  http.post("*/api/v1/users", async ({ request }) => {
    const denied = unauthorized(request);
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
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as UpdateUserBody;
    const user = store.updateUser(Number(params.id), body);
    return user ? HttpResponse.json(user) : err("not_found", "用户不存在", 404);
  }),

  http.delete("*/api/v1/users/:id", ({ request, params }) => {
    const denied = unauthorized(request);
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
    const body = (await request.json().catch(() => ({}))) as { password?: string };
    if (!body.password || body.password.length < 8) {
      return err("bad_request", "口令至少 8 位", 400);
    }
    return store.findUser(Number(params.id))
      ? new HttpResponse(null, { status: 204 })
      : err("not_found", "用户不存在", 404);
  }),

  // —— API Token ——
  http.get(
    "*/api/v1/tokens",
    ({ request }) => unauthorized(request) ?? HttpResponse.json(store.listTokens()),
  ),

  http.post("*/api/v1/tokens", async ({ request }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as { name?: string };
    if (!body.name) {
      return err("bad_request", "令牌名称必填", 400);
    }
    return HttpResponse.json(store.createToken(body.name), { status: 201 });
  }),

  http.delete("*/api/v1/tokens/:id", ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    return store.deleteToken(Number(params.id))
      ? new HttpResponse(null, { status: 204 })
      : err("not_found", "令牌不存在", 404);
  }),

  // —— 仓库 ——
  // FR-66：匿名（无 Bearer）也可列仓库——开关开时返回匿名可读集合，关时 401。
  http.get("*/api/v1/repositories", ({ request }) => {
    const url = new URL(request.url);
    const page = intParam(url, "page", 1);
    const pageSize = intParam(url, "page_size", 20);
    if (unauthorized(request)) {
      if (request.headers.has("Authorization") || !store.anonymousAccess()) {
        return err("unauthorized", "未认证", 401);
      }
      return HttpResponse.json(store.listAnonymousRepositories(page, pageSize));
    }
    return HttpResponse.json(store.listRepositories(page, pageSize));
  }),

  // 公开仓库列表（侧边栏公开导航；开关关时与真实端点一致返回 401）。
  http.get("*/api/v1/public/repositories", () => {
    if (!store.anonymousAccess()) {
      return err("unauthorized", "匿名访问已关闭", 401);
    }
    return HttpResponse.json(store.listAnonymousRepositories(1, 100));
  }),

  // —— 匿名访问全局开关（FR-66，admin）——
  http.get(
    "*/api/v1/settings/anonymous-access",
    ({ request }) =>
      unauthorized(request) ?? HttpResponse.json({ enabled: store.anonymousAccess() }),
  ),

  http.put("*/api/v1/settings/anonymous-access", async ({ request }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as { enabled?: boolean };
    if (typeof body.enabled !== "boolean") {
      return err("bad_request", "enabled 必填", 400);
    }
    return HttpResponse.json({ enabled: store.setAnonymousAccess(body.enabled) });
  }),

  // —— 集群状态（FR-86/88，admin）——
  http.get(
    "*/api/v1/cluster",
    ({ request }) => unauthorized(request) ?? HttpResponse.json(mockClusterStatus()),
  ),

  http.put("*/api/v1/cluster", async ({ request }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      peerUrl?: string;
      peerToken?: string;
      enabled?: boolean;
    };
    if (body.peerUrl !== undefined || body.peerToken !== undefined) {
      store.setPeerConfig(body.peerUrl ?? store.peerURLState(), body.peerToken ?? "");
    }
    if (body.enabled !== undefined) {
      store.setReplicationEnabled(body.enabled);
    }
    return HttpResponse.json(mockClusterStatus());
  }),

  // FR-88：立即同步（手动触发一次）。
  http.post("*/api/v1/cluster/sync-now", async ({ request }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    return HttpResponse.json(mockClusterStatus());
  }),

  // FR-88：同步历史记录（分页，admin）。
  http.get("*/api/v1/cluster/sync-logs", ({ request }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const url = new URL(request.url);
    const limit = Number(url.searchParams.get("limit") ?? 50);
    const offset = Number(url.searchParams.get("offset") ?? 0);
    const items = mockSyncLogs.slice(offset, offset + limit);
    return HttpResponse.json({ items, total: mockSyncLogs.length });
  }),

  // 复制接收审计记录（管理员端点，支持筛选与分页）。
  http.get("*/api/v1/replication-apply-logs", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const url = new URL(request.url);
    const limitParam = integerQuery(url, "limit");
    const offsetParam = integerQuery(url, "offset");
    const sourceSeqParam = integerQuery(url, "sourceSeq");
    if (limitParam.invalid || offsetParam.invalid || sourceSeqParam.invalid) {
      return err("bad_request", "limit、offset、sourceSeq 须为整数", 400);
    }
    const sourceSeq = sourceSeqParam.value;
    if (sourceSeq !== undefined && sourceSeq < 0) {
      return err("bad_request", "sourceSeq 须为非负整数", 400);
    }
    const { items } = mockReplicationApplyLogList();
    const sourceNode = url.searchParams.get("sourceNode");
    const peerURL = url.searchParams.get("peerURL");
    const entityType = url.searchParams.get("entityType");
    const entityKey = url.searchParams.get("entityKey");
    const op = url.searchParams.get("op");
    const result = url.searchParams.get("result");
    const filtered = items.filter(
      (item) =>
        (!sourceNode || item.sourceNode === sourceNode) &&
        (sourceSeq === undefined || item.sourceSeq === sourceSeq) &&
        (!peerURL || item.peerUrl === peerURL) &&
        (!entityType || item.entityType === entityType) &&
        (!entityKey || item.entityKey === entityKey) &&
        (!op || item.op === op) &&
        (!result || item.result === result),
    );
    const requestedLimit = limitParam.value ?? 50;
    const limit = requestedLimit > 0 && requestedLimit <= 200 ? requestedLimit : 50;
    const requestedOffset = offsetParam.value ?? 0;
    const offset = requestedOffset >= 0 ? requestedOffset : 0;
    return HttpResponse.json({
      items: filtered.slice(offset, offset + limit),
      total: filtered.length,
    });
  }),

  // FR-38：管理审计日志（非 OpenAPI 端点，仅管理员可读）。
  http.get("*/api/v1/audit-logs", ({ request }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
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
        (!url.searchParams.get("action") || item.action === url.searchParams.get("action")) &&
        (!url.searchParams.get("repo") || item.repo === url.searchParams.get("repo")) &&
        (!from || item.ts >= from) &&
        (!to || item.ts <= to),
    );
    const requestedLimit = limitParam.value ?? 50;
    const limit = requestedLimit > 0 && requestedLimit <= 200 ? requestedLimit : 50;
    const requestedOffset = offsetParam.value ?? 0;
    const offset = requestedOffset >= 0 ? requestedOffset : 0;
    return HttpResponse.json({
      items: filtered.slice(offset, offset + limit),
      total: filtered.length,
    });
  }),

  http.post("*/api/v1/repositories", async ({ request }) => {
    const denied = unauthorized(request);
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
    });
    if (!repo) {
      return err("conflict", "仓库名已存在", 409);
    }
    return HttpResponse.json(repo, { status: 201 });
  }),

  http.patch("*/api/v1/repositories/:name", async ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      visibility?: Repository["visibility"];
      description?: string;
      remoteUrl?: string;
      members?: string[];
    };
    const repo = store.updateRepository(String(params.name), body);
    return repo ? HttpResponse.json(repo) : err("not_found", "仓库不存在", 404);
  }),

  http.delete("*/api/v1/repositories/:name", ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    return store.deleteRepository(String(params.name))
      ? new HttpResponse(null, { status: 204 })
      : err("not_found", "仓库不存在", 404);
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
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const items = store.getAcl(String(params.name));
    return items ? HttpResponse.json({ items }) : err("not_found", "仓库不存在", 404);
  }),

  http.put("*/api/v1/repositories/:name/acl", async ({ request, params }) => {
    const denied = unauthorized(request);
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
    const denied = unauthorized(request);
    if (denied) {
      return denied;
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

  // FR-103：批量删除（仅管理员；逐条尽力，部分失败以 failed 明细返回）。
  http.post("*/api/v1/repositories/:name/assets/batch-delete", async ({ request, params }) => {
    const denied = adminUnauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as { paths?: string[] };
    if (!Array.isArray(body.paths) || body.paths.length === 0) {
      return err("bad_request", "paths 不能为空", 400);
    }
    if (body.paths.length > 500) {
      return err("bad_request", "paths 单次最多 500 条", 400);
    }
    if (body.paths.some((p) => typeof p !== "string" || p === "")) {
      return err("bad_request", "paths 中存在空路径", 400);
    }
    const result = store.batchDeleteAssets(String(params.name), body.paths);
    return result ? HttpResponse.json(result) : err("not_found", "仓库不存在", 404);
  }),

  // FR-54：目录懒加载。匿名仅在全局开关开启且仓库 public 时可读。
  http.get("*/api/v1/repositories/:name/tree", ({ request, params }) => {
    const name = String(params.name);
    if (unauthorized(request)) {
      const repo = store.findRepository(name);
      if (!store.anonymousAccess() || !repo || repo.visibility !== "public") {
        return err("unauthorized", "未认证", 401);
      }
    }
    const url = new URL(request.url);
    const entry = store.listDirectory(name, url.searchParams.get("prefix") ?? "");
    return entry ? HttpResponse.json(entry) : err("not_found", "仓库不存在", 404);
  }),

  http.get("*/api/v1/repositories/:name/usage", ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const url = new URL(request.url);
    const result = store.usage(String(params.name), `${url.protocol}//${url.host}`);
    return result ? HttpResponse.json(result) : err("not_found", "仓库不存在", 404);
  }),

  // FR-73：Maven 网页上传（服务端生成 pom/校验和/metadata；mock 仅登记 asset 摘要）。
  http.post("*/api/v1/repositories/:name/maven-upload", async ({ request, params }) => {
    const denied = unauthorized(request);
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

  // —— 迁移任务（0.4.0 foundation）——
  http.get("*/api/v1/migrations", ({ request }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const url = new URL(request.url);
    return HttpResponse.json(
      store.listMigrations(intParam(url, "page", 1), intParam(url, "page_size", 20)),
    );
  }),

  http.post("*/api/v1/migrations", async ({ request }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      sourceType?: MigrationTask["sourceType"];
      sourceConfig?: Record<string, unknown>;
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
      credentialRef: body.credentialRef,
      conflictPolicy: body.conflictPolicy,
      plan: body.plan,
    });
    return HttpResponse.json(task, { status: 201 });
  }),

  http.post("*/api/v1/migrations/discover", async ({ request }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const body = (await request.json().catch(() => ({}))) as {
      sourceType?: MigrationTask["sourceType"];
      sourceConfig?: Record<string, unknown>;
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
        credentialRef: body.credentialRef,
        conflictPolicy: body.conflictPolicy,
      }),
    );
  }),

  http.get("*/api/v1/migrations/:id", ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const task = store.findMigration(Number(params.id));
    return task ? HttpResponse.json(task) : err("not_found", "任务不存在", 404);
  }),

  http.post("*/api/v1/migrations/:id/start", async ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
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
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
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
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
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
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const report = store.migrationReport(Number(params.id));
    return report ? HttpResponse.json(report) : err("not_found", "任务不存在", 404);
  }),

  http.post("*/api/v1/migrations/:id/finalize", ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) {
      return denied;
    }
    const result = store.finalizeMigration(Number(params.id));
    if (result === "not_found") {
      return err("not_found", "任务不存在", 404);
    }
    if (result === "conflict") {
      return err("conflict", "仅 completed 可 finalize", 409);
    }
    return HttpResponse.json(result);
  }),
];
