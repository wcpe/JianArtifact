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
  Paginated,
  Permission,
  SearchHit,
  UpdateRepositoryRequest,
  UpdateUserRequest,
} from '../../api/types';
import {
  dashboardSummary,
  hostMetrics,
  nextId,
  state,
  toArtifactDetail,
  toArtifactDto,
  toTokenView,
  toUserInfo,
  toUserView,
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
