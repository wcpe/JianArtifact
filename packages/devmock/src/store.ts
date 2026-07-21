// 内存态数据存储：为 MSW 处理器提供可增删改查的 0.2.0 管理面数据。
// 仅用于开发态（浏览器 worker）与测试（Node server）Mock，不进入生产构建。
// 类型绑定 schema.gen.ts（与 api/openapi.yaml 同源），保证 mock 数据不偏离契约。
import type { components } from "./schema.gen";

type Schemas = components["schemas"];

export type User = Schemas["User"];
export type Token = Schemas["Token"];
export type TokenCreated = Schemas["TokenCreated"];
export type Repository = Schemas["Repository"];
export type AclEntry = Schemas["AclEntry"];
export type StatusInfo = Schemas["StatusInfo"];

/** 开发/测试态签发的固定会话令牌明文；鉴权守卫据此放行。 */
export const MOCK_TOKEN = "mock.jwt.token";

interface StoredToken extends Token {
  /** 明文令牌仅签发时返回一次，此处留存以模拟“再次列表不含明文”。 */
  plaintext: string;
}

interface State {
  initialized: boolean;
  version: string;
  migrationVersion: string;
  users: User[];
  tokens: StoredToken[];
  repositories: Repository[];
  acls: Record<string, AclEntry[]>;
  seq: { user: number; token: number; repo: number };
}

const MOCK_VERSION = "0.2.0-mock";

function seed(): State {
  return {
    initialized: true,
    version: MOCK_VERSION,
    migrationVersion: "0001_init",
    users: [
      { id: 1, username: "admin", role: "admin", status: "active", createdAt: "2026-01-01T00:00:00Z" },
      { id: 2, username: "developer", role: "user", status: "active", createdAt: "2026-01-02T00:00:00Z" },
    ],
    tokens: [{ id: 1, name: "ci", createdAt: "2026-01-03T00:00:00Z", plaintext: "jat_seedci" }],
    repositories: [
      { id: 1, name: "maven-releases", format: "maven", type: "hosted", visibility: "private", createdAt: "2026-01-01T00:00:00Z" },
      { id: 2, name: "npm-proxy", format: "npm", type: "proxy", visibility: "public", createdAt: "2026-01-02T00:00:00Z" },
      { id: 3, name: "raw-hosted", format: "raw", type: "hosted", visibility: "private", createdAt: "2026-01-03T00:00:00Z" },
    ],
    acls: { "maven-releases": [{ subjectId: 2, action: "read" }] },
    seq: { user: 2, token: 1, repo: 3 },
  };
}

let state: State = seed();

/** 重置为初始种子数据；测试用例间隔离状态时调用。 */
export function resetStore(): void {
  state = seed();
}

/** 清空 user 表并复位为未初始化，用于验收“空库自举”路径。 */
export function emptyStore(): void {
  state = seed();
  state.users = [];
  state.tokens = [];
  state.initialized = false;
  state.seq = { user: 0, token: 0, repo: 3 };
}

function nowIso(): string {
  return new Date().toISOString();
}

function pageSlice<T>(items: T[], page: number, pageSize: number): T[] {
  const start = (page - 1) * pageSize;
  return items.slice(start, start + pageSize);
}

export const store = {
  status(): StatusInfo {
    return {
      version: state.version,
      ready: true,
      initialized: state.initialized,
      migrationVersion: state.migrationVersion,
      userCount: state.users.length,
    };
  },

  isInitialized(): boolean {
    return state.initialized;
  },

  /** 空库自举：仅在未初始化时创建首个管理员。返回创建的用户或 null（已初始化）。 */
  bootstrap(username: string): User | null {
    if (state.initialized || state.users.length > 0) {
      return null;
    }
    const user: User = {
      id: ++state.seq.user,
      username,
      role: "admin",
      status: "active",
      createdAt: nowIso(),
    };
    state.users.push(user);
    state.initialized = true;
    return user;
  },

  /** 登录：校验用户名存在且启用即视为成功（口令在 mock 中不校验）。 */
  login(username: string): User | null {
    const user = state.users.find((u) => u.username === username && u.status === "active");
    return user ?? null;
  },

  listUsers(page: number, pageSize: number): { items: User[]; total: number } {
    return { items: pageSlice(state.users, page, pageSize), total: state.users.length };
  },

  findUser(id: number): User | undefined {
    return state.users.find((u) => u.id === id);
  },

  createUser(username: string, role: User["role"]): User | null {
    if (state.users.some((u) => u.username === username)) {
      return null;
    }
    const user: User = { id: ++state.seq.user, username, role, status: "active", createdAt: nowIso() };
    state.users.push(user);
    return user;
  },

  updateUser(id: number, patch: Partial<Pick<User, "role" | "status">>): User | null {
    const user = state.users.find((u) => u.id === id);
    if (!user) {
      return null;
    }
    if (patch.role) {
      user.role = patch.role;
    }
    if (patch.status) {
      user.status = patch.status;
    }
    return user;
  },

  deleteUser(id: number): boolean {
    const before = state.users.length;
    state.users = state.users.filter((u) => u.id !== id);
    return state.users.length < before;
  },

  listTokens(): { items: Token[] } {
    // 列表不含明文：仅回显契约字段，规避明文泄漏。
    return {
      items: state.tokens.map((token) => ({
        id: token.id,
        name: token.name,
        createdAt: token.createdAt,
      })),
    };
  },

  createToken(name: string): TokenCreated {
    const id = ++state.seq.token;
    const plaintext = `jat_${Math.random().toString(36).slice(2, 12)}`;
    const created: StoredToken = { id, name, createdAt: nowIso(), plaintext };
    state.tokens.push(created);
    return { id, name, token: plaintext, createdAt: created.createdAt };
  },

  deleteToken(id: number): boolean {
    const before = state.tokens.length;
    state.tokens = state.tokens.filter((t) => t.id !== id);
    return state.tokens.length < before;
  },

  listRepositories(page: number, pageSize: number): { items: Repository[]; total: number } {
    return { items: pageSlice(state.repositories, page, pageSize), total: state.repositories.length };
  },

  findRepository(name: string): Repository | undefined {
    return state.repositories.find((r) => r.name === name);
  },

  createRepository(input: Pick<Repository, "name" | "format" | "type"> & { visibility?: Repository["visibility"] }): Repository | null {
    if (state.repositories.some((r) => r.name === input.name)) {
      return null;
    }
    const repo: Repository = {
      id: ++state.seq.repo,
      name: input.name,
      format: input.format,
      type: input.type,
      visibility: input.visibility ?? "private",
      createdAt: nowIso(),
    };
    state.repositories.push(repo);
    return repo;
  },

  updateRepository(name: string, patch: { visibility?: Repository["visibility"] }): Repository | null {
    const repo = state.repositories.find((r) => r.name === name);
    if (!repo) {
      return null;
    }
    if (patch.visibility) {
      repo.visibility = patch.visibility;
    }
    return repo;
  },

  deleteRepository(name: string): boolean {
    const before = state.repositories.length;
    state.repositories = state.repositories.filter((r) => r.name !== name);
    delete state.acls[name];
    return state.repositories.length < before;
  },

  getAcl(name: string): AclEntry[] | null {
    if (!state.repositories.some((r) => r.name === name)) {
      return null;
    }
    return state.acls[name] ?? [];
  },

  setAcl(name: string, items: AclEntry[]): AclEntry[] | null {
    if (!state.repositories.some((r) => r.name === name)) {
      return null;
    }
    state.acls[name] = items;
    return items;
  },
};
