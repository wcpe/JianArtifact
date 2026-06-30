// MSW handlers：对 src/api/endpoints.ts 各端点实现有状态 CRUD（FR-116 / FR-119，ADR-0035）。
//
// 读写内存 store，鉴权与错误码贴合后端契约：
// - 未带 / 无效 Bearer → 401 unauthorized；非管理员访问仅管理员端点 → 403 forbidden；
// - 资源不存在 → 404 not_found；唯一性冲突（重名仓库 / 重复用户名）→ 409 conflict。
// 响应结构对齐 types.ts：列表端点返回裸数组、/search 返回分页结构、错误体 {error:{code,message}}。

import { http, HttpResponse } from 'msw';
import type {
  AclDto,
  CreateRepositoryRequest,
  CreateUserRequest,
  DynamicConfig,
  GroupAclView,
  GroupView,
  OnlinePullJob,
  Paginated,
  Permission,
  ProtectionConfig,
  SearchHit,
  UpdateRepositoryRequest,
  UpdateUserRequest,
} from '../../api/types';
import {
  dashboardSummary,
  groupMemberViews,
  hostMetrics,
  metricPoints,
  nextId,
  protectionStatus,
  state,
  toArtifactDetail,
  toArtifactDto,
  toTokenView,
  toUserInfo,
  toUserView,
  usageAnalytics,
  type MockUser,
} from './store';

/** 管理 API 前缀（与 client.ts API_BASE 一致）。 */
const API = '/api/v1';

/** 统一错误响应（对齐后端 {error:{code,message}} 结构）。 */
function error(status: number, code: string, message: string): Response {
  return HttpResponse.json({ error: { code, message } }, { status });
}

/** 从请求 Authorization 头解析当前用户；无 / 无效令牌返回 null。 */
function currentUser(request: Request): MockUser | null {
  const auth = request.headers.get('Authorization');
  if (!auth || !auth.startsWith('Bearer ')) {
    return null;
  }
  const token = auth.slice('Bearer '.length);
  const userId = state.sessions.get(token);
  if (!userId) {
    return null;
  }
  return state.users.find((u) => u.id === userId) ?? null;
}

/** 鉴权守卫：要求登录（可选要求管理员）。返回用户或错误响应。 */
function requireUser(request: Request, opts: { admin?: boolean } = {}): MockUser | Response {
  const user = currentUser(request);
  if (!user) {
    return error(401, 'unauthorized', '未认证');
  }
  if (opts.admin && user.role !== 'admin') {
    return error(403, 'forbidden', '需要管理员权限');
  }
  return user;
}

/** 类型守卫：判断守卫返回的是否为错误响应。 */
function isResponse(v: MockUser | Response): v is Response {
  return v instanceof Response;
}

export const handlers = [
  // —— 认证 ——
  http.post(`${API}/auth/login`, async ({ request }) => {
    const { username, password } = (await request.json()) as {
      username: string;
      password: string;
    };
    const user = state.users.find((u) => u.username === username && !u.disabled);
    if (!user || user.password !== password) {
      return error(401, 'unauthorized', '用户名或口令错误');
    }
    const token = nextId('session');
    state.sessions.set(token, user.id);
    return HttpResponse.json({
      access_token: token,
      token_type: 'Bearer',
      expires_in: 3600,
      user: toUserInfo(user),
    });
  }),

  http.post(`${API}/auth/logout`, ({ request }) => {
    const auth = request.headers.get('Authorization');
    if (auth?.startsWith('Bearer ')) {
      state.sessions.delete(auth.slice('Bearer '.length));
    }
    return new HttpResponse(null, { status: 204 });
  }),

  http.post(`${API}/auth/refresh`, ({ request }) => {
    const user = requireUser(request);
    if (isResponse(user)) return user;
    const token = nextId('session');
    state.sessions.set(token, user.id);
    return HttpResponse.json({
      access_token: token,
      token_type: 'Bearer',
      expires_in: 3600,
      user: toUserInfo(user),
    });
  }),

  http.get(`${API}/me`, ({ request }) => {
    const user = requireUser(request);
    if (isResponse(user)) return user;
    return HttpResponse.json(toUserInfo(user));
  }),

  // —— 用户管理（仅管理员） ——
  http.get(`${API}/users`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json(state.users.map(toUserView));
  }),

  http.post(`${API}/users`, async ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const body = (await request.json()) as CreateUserRequest;
    if (state.users.some((u) => u.username === body.username)) {
      return error(409, 'conflict', '用户名已存在');
    }
    const user: MockUser = {
      id: nextId('u'),
      username: body.username,
      role: body.role,
      disabled: false,
      created_at: new Date().toISOString(),
      password: body.password,
    };
    state.users.push(user);
    return HttpResponse.json(toUserView(user));
  }),

  http.patch(`${API}/users/:id`, async ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const user = state.users.find((u) => u.id === params.id);
    if (!user) return error(404, 'not_found', '用户不存在');
    const body = (await request.json()) as UpdateUserRequest;
    if (body.role !== undefined) user.role = body.role;
    if (body.disabled !== undefined) user.disabled = body.disabled;
    return HttpResponse.json(toUserView(user));
  }),

  http.delete(`${API}/users/:id`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const idx = state.users.findIndex((u) => u.id === params.id);
    if (idx === -1) return error(404, 'not_found', '用户不存在');
    state.users.splice(idx, 1);
    return new HttpResponse(null, { status: 204 });
  }),

  // —— 仓库管理 ——
  http.get(`${API}/repositories`, ({ request }) => {
    const user = currentUser(request);
    // 匿名仅见 public；登录用户见全部（mock 简化，不展开每仓 ACL）。
    const visible = state.repositories.filter((r) => r.visibility === 'public' || user !== null);
    return HttpResponse.json(visible);
  }),

  http.get(`${API}/repositories/:id`, ({ request, params }) => {
    const repo = state.repositories.find((r) => r.id === params.id);
    if (!repo) return error(404, 'not_found', '仓库不存在');
    if (repo.visibility === 'private' && currentUser(request) === null) {
      return error(404, 'not_found', '仓库不存在');
    }
    return HttpResponse.json(repo);
  }),

  http.post(`${API}/repositories`, async ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const body = (await request.json()) as CreateRepositoryRequest;
    if (state.repositories.some((r) => r.name === body.name)) {
      return error(409, 'conflict', '仓库名已存在');
    }
    const repo = {
      id: nextId('r'),
      name: body.name,
      format: body.format,
      type: body.type,
      visibility: body.visibility,
      upstream_url: body.upstream_url ?? null,
      created_at: new Date().toISOString(),
    };
    state.repositories.push(repo);
    return HttpResponse.json(repo);
  }),

  http.patch(`${API}/repositories/:id`, async ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const repo = state.repositories.find((r) => r.id === params.id);
    if (!repo) return error(404, 'not_found', '仓库不存在');
    const body = (await request.json()) as UpdateRepositoryRequest;
    if (body.visibility !== undefined) repo.visibility = body.visibility;
    if (body.upstream_url !== undefined) repo.upstream_url = body.upstream_url;
    return HttpResponse.json(repo);
  }),

  http.delete(`${API}/repositories/:id`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const idx = state.repositories.findIndex((r) => r.id === params.id);
    if (idx === -1) return error(404, 'not_found', '仓库不存在');
    const [removed] = state.repositories.splice(idx, 1);
    state.artifacts = state.artifacts.filter((a) => a.repoId !== removed.id);
    return new HttpResponse(null, { status: 204 });
  }),

  // —— 制品浏览 / 详情 / 删除 ——
  http.get(`${API}/repositories/:id/artifacts`, ({ request, params }) => {
    const repo = state.repositories.find((r) => r.id === params.id);
    if (!repo) return error(404, 'not_found', '仓库不存在');
    if (repo.visibility === 'private' && currentUser(request) === null) {
      return error(404, 'not_found', '仓库不存在');
    }
    const items = state.artifacts.filter((a) => a.repoId === repo.id).map(toArtifactDto);
    return HttpResponse.json(items);
  }),

  http.get(`${API}/repositories/:id/artifacts/*`, ({ request, params }) => {
    const repo = state.repositories.find((r) => r.id === params.id);
    if (!repo) return error(404, 'not_found', '仓库不存在');
    if (repo.visibility === 'private' && currentUser(request) === null) {
      return error(404, 'not_found', '仓库不存在');
    }
    const path = decodeURIComponent((params as Record<string, string>)['0'] ?? '');
    const art = state.artifacts.find((a) => a.repoId === repo.id && a.path === path);
    if (!art) return error(404, 'not_found', '制品不存在');
    return HttpResponse.json(toArtifactDetail(art, repo));
  }),

  http.delete(`${API}/repositories/:id/artifacts/*`, ({ request, params }) => {
    const guard = requireUser(request);
    if (isResponse(guard)) return guard;
    const repo = state.repositories.find((r) => r.id === params.id);
    if (!repo) return error(404, 'not_found', '仓库不存在');
    const path = decodeURIComponent((params as Record<string, string>)['0'] ?? '');
    const idx = state.artifacts.findIndex((a) => a.repoId === repo.id && a.path === path);
    if (idx === -1) return error(404, 'not_found', '制品不存在');
    state.artifacts.splice(idx, 1);
    return new HttpResponse(null, { status: 204 });
  }),

  // —— 仓库 ACL（仅管理员） ——
  http.get(`${API}/repositories/:id/acl`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json(state.acls.get(params.id as string) ?? []);
  }),

  http.post(`${API}/repositories/:id/acl`, async ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const body = (await request.json()) as { user_id: string; permission: Permission };
    const entry: AclDto = {
      id: nextId('acl'),
      user_id: body.user_id,
      permission: body.permission,
    };
    const list = state.acls.get(params.id as string) ?? [];
    list.push(entry);
    state.acls.set(params.id as string, list);
    return HttpResponse.json(entry);
  }),

  http.delete(`${API}/repositories/:id/acl/:aclId`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const list = state.acls.get(params.id as string) ?? [];
    state.acls.set(
      params.id as string,
      list.filter((a) => a.id !== params.aclId),
    );
    return new HttpResponse(null, { status: 204 });
  }),

  // —— Token 管理（自助） ——
  http.get(`${API}/tokens`, ({ request }) => {
    const guard = requireUser(request);
    if (isResponse(guard)) return guard;
    return HttpResponse.json(state.tokens.map(toTokenView));
  }),

  http.post(`${API}/tokens`, async ({ request }) => {
    const guard = requireUser(request);
    if (isResponse(guard)) return guard;
    const { name } = (await request.json()) as { name: string };
    const plaintext = `jart_${nextId('tok')}_${Math.random().toString(36).slice(2, 10)}`;
    const token = {
      id: nextId('t'),
      name,
      created_at: new Date().toISOString(),
      last_used_at: null,
      revoked: false,
      plaintext,
    };
    state.tokens.push(token);
    // 签发响应回显明文（仅本次）。
    return HttpResponse.json({
      id: token.id,
      name: token.name,
      created_at: token.created_at,
      token: plaintext,
    });
  }),

  http.delete(`${API}/tokens/:id`, ({ request, params }) => {
    const guard = requireUser(request);
    if (isResponse(guard)) return guard;
    const token = state.tokens.find((t) => t.id === params.id);
    if (!token) return error(404, 'not_found', 'Token 不存在');
    token.revoked = true;
    return new HttpResponse(null, { status: 204 });
  }),

  // —— 跨仓库搜索 ——
  http.get(`${API}/search`, ({ request }) => {
    const url = new URL(request.url);
    const q = (url.searchParams.get('q') ?? '').toLowerCase();
    const user = currentUser(request);
    const offset = Number(url.searchParams.get('offset') ?? 0);
    const limit = Number(url.searchParams.get('limit') ?? 20);
    const hits: SearchHit[] = state.artifacts
      .filter((a) => {
        const repo = state.repositories.find((r) => r.id === a.repoId);
        if (!repo) return false;
        // 按读权限过滤：匿名只见 public 仓库制品。
        if (repo.visibility === 'private' && user === null) return false;
        return a.path.toLowerCase().includes(q);
      })
      .map((a) => {
        const repo = state.repositories.find((r) => r.id === a.repoId)!;
        return {
          repo_id: repo.id,
          repo_name: repo.name,
          format: repo.format,
          path: a.path,
          sha256: a.sha256,
          size: a.size,
          created_at: a.createdAt,
        };
      });
    const page = hits.slice(offset, offset + limit);
    const result: Paginated<SearchHit> = {
      items: page,
      total: hits.length,
      offset,
      limit,
      has_more: offset + limit < hits.length,
    };
    return HttpResponse.json(result);
  }),

  // —— 仪表盘概览（仅管理员） ——
  http.get(`${API}/dashboard/summary`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json(dashboardSummary());
  }),

  // —— 主机监控（仅管理员） ——
  http.get(`${API}/monitor/host`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json(hostMetrics());
  }),

  // —— 审计日志（仅管理员） ——
  http.get(`${API}/audit`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const url = new URL(request.url);
    const offset = Number(url.searchParams.get('offset') ?? 0);
    const limit = Number(url.searchParams.get('limit') ?? 20);
    const all = [...state.audit].sort((a, b) => b.id - a.id);
    return HttpResponse.json({
      items: all.slice(offset, offset + limit),
      total: all.length,
      offset,
      limit,
      has_more: offset + limit < all.length,
    });
  }),

  // —— 设置页（仅管理员） ——
  http.get(`${API}/settings`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json(state.settings);
  }),

  // —— 出站代理连通性测试（仅管理员，FR-128）：mock 下固定返回不可达结果（无真实出站代理）—— //
  http.post(`${API}/settings/proxy-test`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json({ ok: false, elapsed_ms: 0, error: '连接失败（Mock 模式）' });
  }),

  // —— 动态配置（仅管理员，FR-106；有状态 PATCH） ——
  http.get(`${API}/settings/dynamic`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json(state.dynamicConfig);
  }),

  http.patch(`${API}/settings/dynamic`, async ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const body = (await request.json()) as DynamicConfig;
    // 整体替换（与后端「全量写入」语义一致）。
    state.dynamicConfig = body;
    return HttpResponse.json(state.dynamicConfig);
  }),

  // —— 在线更新检查 / 应用 / 回滚 / 任务（仅管理员，FR-85/104 + FR-126 异步化） ——
  // GET /update/check：只读留存的上次检查结果（不联网）。Mock 下「已是最新」留存。
  http.get(`${API}/update/check`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const current = state.settings.current_version;
    return HttpResponse.json({
      result: {
        current_version: current,
        latest_version: current,
        update_available: false,
        asset_name: '',
        notes: '',
      },
      checked_at: 1_700_000_000,
    });
  }),

  // POST /update/check：触发异步检查 job，返回 job_id（202）。
  http.post(`${API}/update/check`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json({ job_id: 'mock-check-job' }, { status: 202 });
  }),

  http.post(`${API}/update/apply`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    // FR-126：立即返回 job_id（202），实际执行在后台。
    return HttpResponse.json({ job_id: 'mock-apply-job' }, { status: 202 });
  }),

  http.post(`${API}/update/rollback`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json({ job_id: 'mock-rollback-job' }, { status: 202 });
  }),

  // GET /update/jobs：列出活动 / 近期更新任务（Mock 下空）。
  http.get(`${API}/update/jobs`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json([]);
  }),

  // GET /update/jobs/{id}：查询某更新任务进度（Mock 下回「检查完成、已是最新」终态）。
  http.get(`${API}/update/jobs/:id`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const current = state.settings.current_version;
    return HttpResponse.json({
      job_id: String(params.id),
      kind: 'check',
      phase: 'done',
      current_version: current,
      latest_version: current,
      check: {
        current_version: current,
        latest_version: current,
        update_available: false,
        asset_name: '',
        notes: '',
      },
    });
  }),

  // —— 系统操作（仅管理员，FR-109；Mock 下不真重启 / 关闭） ——
  http.post(`${API}/system/restart`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json({ status: 'ok' });
  }),

  http.post(`${API}/system/shutdown`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json({ status: 'ok' });
  }),

  // —— 防护配置（仅管理员，FR-79；有状态 PATCH 整体替换） ——
  http.get(`${API}/protection/config`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json(state.protection);
  }),

  http.patch(`${API}/protection/config`, async ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const body = (await request.json()) as ProtectionConfig;
    state.protection = body;
    return HttpResponse.json(state.protection);
  }),

  // —— 防护状态 / 告警（仅管理员，FR-78） ——
  http.get(`${API}/protection/status`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json(protectionStatus());
  }),

  http.get(`${API}/protection/alerts`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const url = new URL(request.url);
    const offset = Number(url.searchParams.get('offset') ?? 0);
    const limit = Number(url.searchParams.get('limit') ?? 20);
    // Mock 下无告警：返回空分页。
    return HttpResponse.json({ items: [], total: 0, offset, limit, has_more: false });
  }),

  // —— 使用分析（仅管理员，FR-99） ——
  http.get(`${API}/analytics/usage`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const url = new URL(request.url);
    const top = url.searchParams.has('top') ? Number(url.searchParams.get('top')) : undefined;
    return HttpResponse.json(usageAnalytics(top));
  }),

  // —— 指标时序（仅管理员，FR-105） ——
  http.get(`${API}/monitor/metrics`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const url = new URL(request.url);
    const metric = url.searchParams.get('metric') ?? '';
    const from = url.searchParams.has('from') ? Number(url.searchParams.get('from')) : undefined;
    const to = url.searchParams.has('to') ? Number(url.searchParams.get('to')) : undefined;
    const step = url.searchParams.has('step') ? Number(url.searchParams.get('step')) : undefined;
    return HttpResponse.json({ metric, points: metricPoints(metric, { from, to, step }) });
  }),

  // —— 系统运行日志（仅管理员，FR-107；tail 最新在前） ——
  http.get(`${API}/system-logs`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const url = new URL(request.url);
    const level = url.searchParams.get('level');
    const offset = Number(url.searchParams.get('offset') ?? 0);
    const limit = Number(url.searchParams.get('limit') ?? 20);
    const filtered = level
      ? state.systemLogs.filter((l) => l.level === level.toUpperCase())
      : state.systemLogs;
    return HttpResponse.json({
      items: filtered.slice(offset, offset + limit),
      total: filtered.length,
      offset,
      limit,
      has_more: offset + limit < filtered.length,
    });
  }),

  // —— 用户组管理（仅管理员，FR-49；有状态 CRUD） ——
  http.get(`${API}/groups`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json(state.groups);
  }),

  http.post(`${API}/groups`, async ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const { name } = (await request.json()) as { name: string };
    if (state.groups.some((g) => g.name === name)) {
      return error(409, 'conflict', '组名已存在');
    }
    const group: GroupView = {
      id: nextId('g'),
      name,
      created_at: new Date().toISOString(),
    };
    state.groups.push(group);
    state.groupMembers.set(group.id, []);
    return HttpResponse.json(group);
  }),

  http.delete(`${API}/groups/:id`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const idx = state.groups.findIndex((g) => g.id === params.id);
    if (idx === -1) return error(404, 'not_found', '组不存在');
    const [removed] = state.groups.splice(idx, 1);
    // 级联清成员与组 ACL（与后端语义一致）。
    state.groupMembers.delete(removed.id);
    for (const [repoId, list] of state.groupAcls) {
      state.groupAcls.set(
        repoId,
        list.filter((a) => a.group_id !== removed.id),
      );
    }
    return new HttpResponse(null, { status: 204 });
  }),

  http.get(`${API}/groups/:id/members`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    if (!state.groups.some((g) => g.id === params.id)) {
      return error(404, 'not_found', '组不存在');
    }
    return HttpResponse.json(groupMemberViews(params.id as string));
  }),

  http.post(`${API}/groups/:id/members`, async ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    if (!state.groups.some((g) => g.id === params.id)) {
      return error(404, 'not_found', '组不存在');
    }
    const { user_id } = (await request.json()) as { user_id: string };
    const members = state.groupMembers.get(params.id as string) ?? [];
    if (members.includes(user_id)) {
      return error(409, 'conflict', '用户已在组内');
    }
    members.push(user_id);
    state.groupMembers.set(params.id as string, members);
    return new HttpResponse(null, { status: 204 });
  }),

  http.delete(`${API}/groups/:id/members/:userId`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const members = state.groupMembers.get(params.id as string) ?? [];
    state.groupMembers.set(
      params.id as string,
      members.filter((uid) => uid !== params.userId),
    );
    return new HttpResponse(null, { status: 204 });
  }),

  // —— 仓库组 ACL（仅管理员，FR-49；有状态） ——
  http.get(`${API}/repositories/:id/group-acl`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json(state.groupAcls.get(params.id as string) ?? []);
  }),

  http.post(`${API}/repositories/:id/group-acl`, async ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const body = (await request.json()) as { group_id: string; permission: Permission };
    const entry: GroupAclView = {
      id: nextId('gacl'),
      group_id: body.group_id,
      permission: body.permission,
    };
    const list = state.groupAcls.get(params.id as string) ?? [];
    list.push(entry);
    state.groupAcls.set(params.id as string, list);
    return HttpResponse.json(entry);
  }),

  http.delete(`${API}/repositories/:id/group-acl/:aclId`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const list = state.groupAcls.get(params.id as string) ?? [];
    state.groupAcls.set(
      params.id as string,
      list.filter((a) => a.id !== params.aclId),
    );
    return new HttpResponse(null, { status: 204 });
  }),

  // —— Nexus 迁移（仅管理员，FR-81/82/83/91；列表 / 预览 / 任务控制） ——
  http.get(`${API}/migrate/jobs`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const summaries = state.migrationJobs.map((j) => ({
      job_id: j.job_id,
      phase: j.phase,
      total_assets: j.total_assets,
      done_assets: j.done_assets,
      migrated: j.migrated,
      skipped: j.skipped,
      current_repo: j.current_repo,
      paused: j.paused,
    }));
    return HttpResponse.json(summaries);
  }),

  http.get(`${API}/migrate/jobs/:id`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const job = state.migrationJobs.find((j) => j.job_id === params.id);
    if (!job) return error(404, 'not_found', '任务不存在');
    return HttpResponse.json(job);
  }),

  http.post(`${API}/migrate/jobs/:id/cancel`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const job = state.migrationJobs.find((j) => j.job_id === params.id);
    if (!job) return error(404, 'not_found', '任务不存在');
    job.phase = 'cancelled';
    job.paused = false;
    return new HttpResponse(null, { status: 204 });
  }),

  http.post(`${API}/migrate/jobs/:id/pause`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const job = state.migrationJobs.find((j) => j.job_id === params.id);
    if (!job) return error(404, 'not_found', '任务不存在');
    job.paused = true;
    job.phase = 'paused';
    return new HttpResponse(null, { status: 204 });
  }),

  http.post(`${API}/migrate/jobs/:id/resume`, ({ request, params }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    const job = state.migrationJobs.find((j) => j.job_id === params.id);
    if (!job) return error(404, 'not_found', '任务不存在');
    job.paused = false;
    job.phase = 'downloading';
    return new HttpResponse(null, { status: 204 });
  }),

  http.post(`${API}/migrate/nexus/preview`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    // 在线预览：返回示例可迁移仓库列表。
    return HttpResponse.json([
      {
        name: 'maven-central',
        format: 'maven2',
        type: 'proxy',
        upstream_url: 'https://repo1.maven.org/maven2/',
      },
      { name: 'maven-internal', format: 'maven2', type: 'hosted', upstream_url: null },
    ]);
  }),

  http.post(`${API}/migrate/nexus/offline/preview`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json([
      {
        repo_name: 'maven-internal',
        blob_count: 1,
        blobs: [{ blob_name: 'com/example/app-1.0.0.jar', sha1: 'a1b2c3', size: 2048 }],
      },
    ]);
  }),

  http.post(`${API}/migrate/nexus/proxy/migrate`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json({ repos: [], skipped_repos: [] });
  }),

  http.post(`${API}/migrate/nexus/hosted/migrate`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    return HttpResponse.json({ repos: [], skipped_repos: [] });
  }),

  http.post(`${API}/migrate/nexus/online/migrate`, ({ request }) => {
    const guard = requireUser(request, { admin: true });
    if (isResponse(guard)) return guard;
    // 发起异步任务：建一个已完成的任务并返回句柄（202 Accepted）。
    const jobId = nextId('job');
    const job: OnlinePullJob = {
      job_id: jobId,
      phase: 'done',
      total_assets: 0,
      done_assets: 0,
      migrated: 0,
      skipped: 0,
      current_repo: null,
      current_path: null,
      paused: false,
      repos: [],
      skipped_repos: [],
      error: null,
    };
    state.migrationJobs.push(job);
    return HttpResponse.json({ job_id: jobId }, { status: 202 });
  }),

  http.get(`${API}/licenses`, () => {
    // 公开端点，匿名可读。
    return HttpResponse.json({
      generated: false,
      entries: [],
      summary: { total: 0, runtime: 0, dev: 0, licenses: 0 },
    });
  }),

  // —— 健康检查（根路径，公开） ——
  http.get('/health', () => {
    return HttpResponse.json({ status: 'ok', version: '0.5.0-mock', port: 8080 });
  }),
];
