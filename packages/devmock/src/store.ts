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
export type AssetSummary = Schemas["AssetSummary"];
export type AssetList = Schemas["AssetList"];
export type UsageInfo = Schemas["UsageInfo"];
export type UsageSnippet = Schemas["UsageSnippet"];
export type MigrationTask = Schemas["MigrationTask"];
export type MigrationPlan = Schemas["MigrationPlan"];
export type MigrationReport = Schemas["MigrationReport"];

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
  assets: Record<string, AssetSummary[]>;
  migrations: MigrationTask[];
  seq: { user: number; token: number; repo: number; migration: number };
  /** FR-66：实例级匿名访问开关（默认开）。 */
  anonymousAccessEnabled: boolean;
}

const MOCK_VERSION = "0.2.0-mock";

function seed(): State {
  return {
    initialized: true,
    version: MOCK_VERSION,
    migrationVersion: "0001_init",
    users: [
      {
        id: 1,
        username: "admin",
        role: "admin",
        status: "active",
        createdAt: "2026-01-01T00:00:00Z",
      },
      {
        id: 2,
        username: "developer",
        role: "user",
        status: "active",
        createdAt: "2026-01-02T00:00:00Z",
      },
    ],
    tokens: [{ id: 1, name: "ci", createdAt: "2026-01-03T00:00:00Z", plaintext: "jat_seedci" }],
    repositories: [
      {
        id: 1,
        name: "maven-releases",
        format: "maven",
        type: "hosted",
        visibility: "private",
        createdAt: "2026-01-01T00:00:00Z",
      },
      {
        id: 2,
        name: "npm-proxy",
        format: "npm",
        type: "proxy",
        visibility: "public",
        remoteUrl: "https://registry.npmjs.org",
        createdAt: "2026-01-02T00:00:00Z",
      },
      {
        id: 3,
        name: "raw-hosted",
        format: "raw",
        type: "hosted",
        visibility: "private",
        createdAt: "2026-01-03T00:00:00Z",
      },
    ],
    acls: { "maven-releases": [{ subjectId: 2, action: "read" }] },
    assets: {
      "maven-releases": [
        {
          path: "com/example/app/1.0.0/app-1.0.0.jar",
          size: 20480,
          hash: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
          contentType: "application/java-archive",
          updatedAt: "2026-01-04T00:00:00Z",
        },
        {
          path: "com/example/app/1.0.0/app-1.0.0.pom",
          size: 512,
          hash: "b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3",
          contentType: "application/xml",
          updatedAt: "2026-01-04T00:00:00Z",
        },
      ],
    },
    migrations: [],
    seq: { user: 2, token: 1, repo: 3, migration: 0 },
    anonymousAccessEnabled: true,
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
  state.migrations = [];
  state.seq = { user: 0, token: 0, repo: 3, migration: 0 };
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
    const user: User = {
      id: ++state.seq.user,
      username,
      role,
      status: "active",
      createdAt: nowIso(),
    };
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
    return {
      items: pageSlice(state.repositories, page, pageSize),
      total: state.repositories.length,
    };
  },

  /** FR-66：匿名可读仓库列表（mock 近似：public 即匿名可读）。 */
  listAnonymousRepositories(
    page: number,
    pageSize: number,
  ): { items: Repository[]; total: number } {
    const readable = state.repositories.filter((r) => r.visibility === "public");
    return {
      items: pageSlice(readable, page, pageSize),
      total: readable.length,
    };
  },

  /** FR-66：匿名访问全局开关。 */
  anonymousAccess(): boolean {
    return state.anonymousAccessEnabled;
  },

  setAnonymousAccess(enabled: boolean): boolean {
    state.anonymousAccessEnabled = enabled;
    return state.anonymousAccessEnabled;
  },

  findRepository(name: string): Repository | undefined {
    return state.repositories.find((r) => r.name === name);
  },

  createRepository(
    input: Pick<Repository, "name" | "format" | "type"> & {
      visibility?: Repository["visibility"];
      remoteUrl?: string;
      members?: string[];
    },
  ): Repository | null {
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
    if (input.remoteUrl) {
      repo.remoteUrl = input.remoteUrl;
    }
    if (input.members && input.members.length > 0) {
      repo.members = input.members;
    }
    state.repositories.push(repo);
    return repo;
  },

  updateRepository(
    name: string,
    patch: { visibility?: Repository["visibility"]; remoteUrl?: string; members?: string[] },
  ): Repository | null {
    const repo = state.repositories.find((r) => r.name === name);
    if (!repo) {
      return null;
    }
    if (patch.visibility) {
      repo.visibility = patch.visibility;
    }
    if (patch.remoteUrl !== undefined) {
      repo.remoteUrl = patch.remoteUrl;
    }
    if (patch.members !== undefined) {
      repo.members = patch.members;
    }
    return repo;
  },

  deleteRepository(name: string): boolean {
    const before = state.repositories.length;
    state.repositories = state.repositories.filter((r) => r.name !== name);
    delete state.acls[name];
    delete state.assets[name];
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

  /** 列出仓库制品（分页 + 可选路径前缀过滤），仓库不存在返回 null。 */
  listAssets(name: string, prefix: string, page: number, pageSize: number): AssetList | null {
    if (!state.repositories.some((r) => r.name === name)) {
      return null;
    }
    const all = (state.assets[name] ?? [])
      .filter((a) => (prefix ? a.path.startsWith(prefix) : true))
      .sort((a, b) => a.path.localeCompare(b.path));
    return { items: pageSlice(all, page, pageSize), total: all.length };
  },

  /** FR-54：按目录懒加载——返回指定前缀下当前层的目录（全路径带尾斜杠）与文件，仓库不存在返回 null。 */
  listDirectory(
    name: string,
    prefix: string,
  ): { directories: string[]; files: AssetSummary[] } | null {
    if (!state.repositories.some((r) => r.name === name)) {
      return null;
    }
    const dirs = new Set<string>();
    const files: AssetSummary[] = [];
    for (const asset of state.assets[name] ?? []) {
      if (prefix && !asset.path.startsWith(prefix)) {
        continue;
      }
      const rest = asset.path.slice(prefix.length);
      const slash = rest.indexOf("/");
      if (slash >= 0) {
        dirs.add(prefix + rest.slice(0, slash + 1));
      } else {
        files.push(asset);
      }
    }
    return {
      directories: [...dirs].sort(),
      files: files.sort((a, b) => a.path.localeCompare(b.path)),
    };
  },

  /** 据仓库 format/type 与对外基址组装客户端接入片段，仓库不存在返回 null。 */
  usage(name: string, baseURL: string): UsageInfo | null {
    const repo = state.repositories.find((r) => r.name === name);
    if (!repo) {
      return null;
    }
    return { format: repo.format, type: repo.type, snippets: buildUsage(repo, baseURL) };
  },

  listMigrations(page: number, pageSize: number): { items: MigrationTask[]; total: number } {
    const sorted = [...state.migrations].sort((a, b) => b.id - a.id);
    return { items: pageSlice(sorted, page, pageSize), total: state.migrations.length };
  },

  findMigration(id: number): MigrationTask | undefined {
    return state.migrations.find((m) => m.id === id);
  },

  createMigration(input: {
    sourceType: MigrationTask["sourceType"];
    sourceConfig?: Record<string, unknown>;
    credentialRef?: string;
    conflictPolicy?: MigrationTask["conflictPolicy"];
    plan?: MigrationPlan;
  }): MigrationTask {
    const now = nowIso();
    const task: MigrationTask = {
      id: ++state.seq.migration,
      status: "planned",
      sourceType: input.sourceType,
      conflictPolicy: input.conflictPolicy ?? "skip",
      createdAt: now,
      updatedAt: now,
    };
    if (input.sourceConfig) {
      task.sourceConfig = input.sourceConfig;
    }
    if (input.credentialRef) {
      task.credentialRef = input.credentialRef;
    }
    if (input.plan) {
      task.plan = input.plan;
    }
    state.migrations.push(task);
    return task;
  },

  startMigration(
    id: number,
    includeRepositories?: string[],
  ): MigrationTask | "not_found" | "conflict" {
    const task = state.migrations.find((m) => m.id === id);
    if (!task) {
      return "not_found";
    }
    if (task.status !== "planned") {
      return "conflict";
    }
    // 可选收窄 plan
    if (includeRepositories && includeRepositories.length > 0 && task.plan) {
      const allow = new Set(includeRepositories);
      task.plan = {
        ...task.plan,
        repositories: task.plan.repositories.filter((r) => allow.has(r.name)),
      };
      if (task.sourceConfig && typeof task.sourceConfig === "object") {
        task.sourceConfig = {
          ...(task.sourceConfig as Record<string, unknown>),
          includeRepositories,
        };
      }
    }
    task.status = "running";
    task.startedAt = nowIso();
    task.updatedAt = nowIso();
    return task;
  },

  resumeMigration(id: number): MigrationTask | "not_found" | "conflict" {
    const task = state.migrations.find((m) => m.id === id);
    if (!task) {
      return "not_found";
    }
    if (task.status !== "failed" && task.status !== "cancelled") {
      return "conflict";
    }
    task.status = "running";
    task.startedAt = task.startedAt ?? nowIso();
    task.updatedAt = nowIso();
    delete task.errorMessage;
    return task;
  },

  cancelMigration(id: number): MigrationTask | "not_found" | "conflict" {
    const task = state.migrations.find((m) => m.id === id);
    if (!task) {
      return "not_found";
    }
    if (task.status !== "planned" && task.status !== "running") {
      return "conflict";
    }
    task.status = "cancelled";
    task.finishedAt = nowIso();
    task.updatedAt = nowIso();
    task.errorMessage = "用户取消";
    return task;
  },

  migrationReport(id: number): MigrationReport | null {
    const task = state.migrations.find((m) => m.id === id);
    if (!task) {
      return null;
    }
    return {
      taskId: task.id,
      status: task.status,
      sourceType: task.sourceType,
      conflictPolicy: task.conflictPolicy,
      startedAt: task.startedAt,
      finishedAt: task.finishedAt,
      totals: { copied: 0, skipped: 0, failed: 0 },
      cutover: {
        checklist: [
          "将 CI / 客户端 registry 指向本 JianArtifact 实例",
          "将源 Nexus 置为只读（或断开写入）",
          "执行 finalize 增量补齐切换窗口新增制品",
        ],
        delta: null,
      },
      raw: {},
    };
  },

  finalizeMigration(id: number): MigrationTask | "not_found" | "conflict" {
    const task = state.migrations.find((m) => m.id === id);
    if (!task) {
      return "not_found";
    }
    if (task.status !== "completed") {
      return "conflict";
    }
    task.updatedAt = nowIso();
    return task;
  },

  /** discover：同步假计划并落库 planned。 */
  discoverMigration(input: {
    sourceType: MigrationTask["sourceType"];
    sourceConfig?: Record<string, unknown>;
    credentialRef?: string;
    conflictPolicy?: MigrationTask["conflictPolicy"];
  }): { taskId: number; plan: MigrationPlan } {
    const plan: MigrationPlan = {
      repositories: [
        { name: "maven-releases", format: "maven", type: "hosted", estimatedAssets: 2 },
        { name: "npm-hosted", format: "npm", type: "hosted", estimatedAssets: 1 },
      ],
      warnings: ["docker 仓库已忽略"],
      stats: { repositoryCount: 2, estimatedAssets: 3 },
      estimated: true,
    };
    const task = this.createMigration({
      sourceType: input.sourceType,
      sourceConfig: input.sourceConfig,
      credentialRef: input.credentialRef,
      conflictPolicy: input.conflictPolicy,
      plan,
    });
    return { taskId: task.id, plan };
  },
};

/** buildUsage 依仓库 format/type 与对外基址组装接入片段（与后端 domain 层一致）。 */
function buildUsage(repo: Repository, base: string): UsageSnippet[] {
  const writable = repo.type === "hosted";
  const repoURL = `${base}/repository/${repo.name}`;
  if (repo.format === "maven") {
    const snippets: UsageSnippet[] = [
      {
        title: "认证（~/.m2/settings.xml）",
        description: "在 <servers> 中配置凭据。",
        code: `<server>\n  <id>${repo.name}</id>\n  <username><user></username>\n  <password><token></password>\n</server>`,
      },
      {
        title: "解析依赖（pom.xml）",
        description: "在 <repositories> 中声明该仓库。",
        code: `<repository>\n  <id>${repo.name}</id>\n  <url>${repoURL}</url>\n</repository>`,
      },
    ];
    if (writable) {
      snippets.push({
        title: "发布制品（pom.xml + mvn deploy）",
        description: "在 <distributionManagement> 声明部署目标（仅 hosted 可写）。",
        code: `<distributionManagement>\n  <repository>\n    <id>${repo.name}</id>\n    <url>${repoURL}</url>\n  </repository>\n</distributionManagement>`,
      });
    }
    return snippets;
  }
  if (repo.format === "npm") {
    const registryURL = `${base}/npm/${repo.name}/`;
    const snippets: UsageSnippet[] = [
      {
        title: "配置 registry",
        description: "将该仓库设为 npm registry。",
        code: `npm config set registry ${registryURL}`,
      },
      {
        title: "安装依赖",
        description: "从该 registry 安装包。",
        code: `npm install <package> --registry ${registryURL}`,
      },
    ];
    if (writable) {
      snippets.push({
        title: "发布包（npm publish）",
        description: "发布到该仓库（仅 hosted 可写）。",
        code: `npm publish --registry ${registryURL}`,
      });
    }
    return snippets;
  }
  const snippets: UsageSnippet[] = [
    {
      title: "下载制品（curl）",
      description: "以 API Token 作口令（公开仓库可匿名读）。",
      code: `curl -u <user>:<token> -O ${repoURL}/path/to/artifact`,
    },
  ];
  if (writable) {
    snippets.push({
      title: "上传制品（curl）",
      description: "PUT 上传到指定路径（仅 hosted 可写）。",
      code: `curl -u <user>:<token> --upload-file ./artifact ${repoURL}/path/to/artifact`,
    });
  }
  return snippets;
}
