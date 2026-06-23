// API 客户端：统一处理 Bearer 鉴权头、错误结构解析、401 跳登录。
// 同源部署，所有管理 API 走相对路径 /api/v1，不硬编码后端地址。

/** localStorage 中存放访问令牌的键名。 */
const TOKEN_STORAGE_KEY = 'jianartifact.token';

/** 管理 API 基础前缀。 */
const API_BASE = '/api/v1';

/** 后端统一错误结构。 */
interface ApiErrorBody {
  error?: {
    code?: string;
    message?: string;
  };
}

/** 调用方可识别的 API 错误：携带 HTTP 状态码与后端错误码 / 文案。 */
export class ApiError extends Error {
  /** HTTP 状态码。 */
  readonly status: number;
  /** 后端稳定错误码（如 unauthorized / forbidden / conflict）。 */
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }
}

/** 401 未认证时的回调（由应用注入，用于清理会话并跳转登录）。 */
let onUnauthorized: (() => void) | null = null;

/** 注册 401 处理回调。 */
export function setUnauthorizedHandler(handler: () => void): void {
  onUnauthorized = handler;
}

/** 读取已保存的访问令牌；无则返回 null。 */
export function getToken(): string | null {
  return localStorage.getItem(TOKEN_STORAGE_KEY);
}

/** 保存访问令牌。 */
export function setToken(token: string): void {
  localStorage.setItem(TOKEN_STORAGE_KEY, token);
}

/** 清除访问令牌（登出 / 会话失效）。 */
export function clearToken(): void {
  localStorage.removeItem(TOKEN_STORAGE_KEY);
}

/** 请求选项。 */
interface RequestOptions {
  method?: string;
  body?: unknown;
  query?: Record<string, string | number | undefined>;
}

/** 拼接查询字符串（跳过 undefined 值）。 */
function buildQuery(query?: Record<string, string | number | undefined>): string {
  if (!query) return '';
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined && value !== '') {
      params.set(key, String(value));
    }
  }
  const qs = params.toString();
  return qs ? `?${qs}` : '';
}

/**
 * 发起一次管理 API 请求并解析响应。
 *
 * - 自动附加 Bearer 令牌（若已登录）。
 * - 解析后端 `{error:{code,message}}` 错误结构，抛出 ApiError。
 * - 401 时触发已注册的未认证回调（清理会话并跳登录）。
 * - 204 / 空响应体返回 undefined。
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, query } = options;
  const headers: Record<string, string> = {};
  const token = getToken();
  if (token) {
    headers['Authorization'] = `Bearer ${token}`;
  }
  let payload: string | undefined;
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    payload = JSON.stringify(body);
  }

  const response = await fetch(`${API_BASE}${path}${buildQuery(query)}`, {
    method,
    headers,
    body: payload,
  });

  if (response.status === 401) {
    // 未认证 / 会话失效：触发回调清理并跳登录
    if (onUnauthorized) {
      onUnauthorized();
    }
  }

  if (!response.ok) {
    let code = 'error';
    let message = `请求失败（HTTP ${response.status}）`;
    try {
      const data = (await response.json()) as ApiErrorBody;
      if (data.error) {
        code = data.error.code ?? code;
        message = data.error.message ?? message;
      }
    } catch {
      // 响应体非 JSON：保留默认文案
    }
    throw new ApiError(response.status, code, message);
  }

  if (response.status === 204) {
    return undefined as T;
  }
  const text = await response.text();
  if (!text) {
    return undefined as T;
  }
  return JSON.parse(text) as T;
}
