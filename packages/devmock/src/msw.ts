// MSW 请求处理器：把 api/openapi.yaml 定义的 0.2.0 管理面端点映射到内存态 store。
// 浏览器（src/browser.ts）与 Node（src/node.ts）双端复用同一组 handlers，
// 保证 web 开发态脱离后端可跑、vitest 集成测试与真实 fetch 链路同源。
import { http, HttpResponse } from "msw";

import { MOCK_TOKEN, store } from "./store";
import type { AclEntry, MigrationPlan, MigrationTask, Repository, User } from "./store";

function err(code: string, message: string, status: number) {
  return HttpResponse.json({ error: { code, message } }, { status });
}

/** 受保护端点统一鉴权：无 `Authorization: Bearer` 即 401。 */
function unauthorized(request: Request) {
  const header = request.headers.get("Authorization") ?? "";
  return header.startsWith("Bearer ") ? null : err("unauthorized", "未认证", 401);
}

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
      if (!store.anonymousAccess()) {
        return err("unauthorized", "匿名访问已关闭", 401);
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
