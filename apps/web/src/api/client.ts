// 轻量 typed API 客户端：统一 baseURL、Bearer 注入、JSON 解析与错误归一化。
// 开发态由 MSW worker 拦截（见 src/mocks），生产态直连同源后端 /api/v1。

/** 归一化的接口错误：承载后端 error.code / message 与 HTTP 状态。 */
export class ApiError extends Error {
  readonly code: string;
  readonly status: number;

  constructor(code: string, message: string, status: number) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.status = status;
  }
}

/**
 * 全局 401 事件名：当任何请求收到 HTTP 401 时触发，
 * AuthContext 监听此事件并自动清除本地会话（退出登录）。
 */
export const AUTH_EXPIRED_EVENT = "jianartifact:auth-expired";

const USER_KEY_FOR_CHECK = "jianartifact.user";

/**
 * 触发全局 401 事件。当存在 token 或 user 快照时触发（表明本地仍认为已登录），
 * 避免纯匿名端点的 401 重复触发。
 */
function emitAuthExpired() {
  const hasToken = Boolean(getToken());
  const hasUser = Boolean(localStorage.getItem(USER_KEY_FOR_CHECK));
  if (hasToken || hasUser) {
    window.dispatchEvent(new CustomEvent(AUTH_EXPIRED_EVENT));
  }
}

const TOKEN_KEY = "jianartifact.token";

/** 读取持久化的会话令牌（localStorage）。 */
export function getToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

/** 写入或清除会话令牌。 */
export function setToken(token: string | null): void {
  try {
    if (token) {
      localStorage.setItem(TOKEN_KEY, token);
    } else {
      localStorage.removeItem(TOKEN_KEY);
    }
  } catch {
    /* 隐私模式等场景下忽略存储失败 */
  }
}

interface RequestOptions {
  method?: string;
  body?: unknown;
  /** 查询参数（值为 undefined 时跳过）。 */
  query?: Record<string, string | number | undefined>;
  /** 可选中止信号（超时 / 用户取消）。 */
  signal?: AbortSignal;
}

function buildUrl(path: string, query?: RequestOptions["query"]): string {
  const url = `/api/v1${path}`;
  if (!query) {
    return url;
  }
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined) {
      params.set(key, String(value));
    }
  }
  const qs = params.toString();
  return qs ? `${url}?${qs}` : url;
}

async function parseError(response: Response): Promise<ApiError> {
  try {
    const data = (await response.json()) as { error?: { code?: string; message?: string } };
    const code = data.error?.code ?? "unknown";
    const message = data.error?.message ?? response.statusText;
    return new ApiError(code, message, response.status);
  } catch {
    return new ApiError("unknown", response.statusText || "请求失败", response.status);
  }
}

/** 发起请求；2xx 返回解析后的 JSON（204 返回 undefined），否则抛出 ApiError。 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = "GET", body, query, signal } = options;
  const headers: Record<string, string> = {};
  const token = getToken();
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  let payload: string | undefined;
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body);
  }

  let response: Response;
  try {
    response = await fetch(buildUrl(path, query), { method, headers, body: payload, signal });
  } catch (e) {
    if (signal?.aborted || (e instanceof DOMException && e.name === "AbortError")) {
      throw new ApiError("aborted", "请求已取消或超时", 0);
    }
    throw new ApiError("network", e instanceof Error ? e.message : "网络错误", 0);
  }
  if (!response.ok) {
    if (response.status === 401) {
      emitAuthExpired();
    }
    throw await parseError(response);
  }
  if (response.status === 204) {
    return undefined as T;
  }
  return (await response.json()) as T;
}

/** 协议层 PUT 上传（Raw hosted）：Bearer + 原始 body，非 /api/v1 JSON。 */
export async function putProtocolAsset(
  url: string,
  body: Blob | ArrayBuffer | File,
  contentType?: string,
): Promise<{ repository: string; path: string; hash: string; size: number; contentType: string }> {
  const headers: Record<string, string> = {};
  const token = getToken();
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  if (contentType) {
    headers["Content-Type"] = contentType;
  }
  let response: Response;
  try {
    response = await fetch(url, { method: "PUT", headers, body });
  } catch (e) {
    throw new ApiError("network", e instanceof Error ? e.message : "网络错误", 0);
  }
  if (!response.ok) {
    throw await parseError(response);
  }
  return (await response.json()) as {
    repository: string;
    path: string;
    hash: string;
    size: number;
    contentType: string;
  };
}
