// 鉴权上下文：持有当前用户与会话令牌，暴露登录 / 自举 / 登出。
// 令牌与用户快照持久化到 localStorage，刷新后免重新登录（后端无 /me，故快照用户）。
// 监听全局 401 事件（AUTH_EXPIRED_EVENT），token 过期时自动清除本地会话。
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import type { ReactNode } from "react";

import * as api from "../api/endpoints";
import { AUTH_EXPIRED_EVENT, setToken } from "../api/client";
import { clearAsyncCache } from "../hooks/useAsync";
import type { User } from "../api/types";

const USER_KEY = "jianartifact.user";

function readStoredUser(): User | null {
  try {
    const raw = localStorage.getItem(USER_KEY);
    return raw ? (JSON.parse(raw) as User) : null;
  } catch {
    return null;
  }
}

function writeStoredUser(user: User | null): void {
  try {
    if (user) {
      localStorage.setItem(USER_KEY, JSON.stringify(user));
    } else {
      localStorage.removeItem(USER_KEY);
    }
  } catch {
    /* 忽略存储失败 */
  }
}

export interface AuthContextValue {
  user: User | null;
  isAuthenticated: boolean;
  login: (username: string, password: string) => Promise<void>;
  bootstrap: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  /** FR-34：OIDC 回调失败时带回的 i18n 键；登录框消费后清空。 */
  oidcError: string | null;
  clearOidcError: () => void;
}

/** oidcErrorKey 把服务端片段错误码映射为 i18n 键（未知码归入通用失败）。 */
function oidcErrorKey(code: string): string {
  switch (code) {
    case "flow_expired":
    case "state_mismatch":
    case "missing_code":
      return "auth.oidcErrorExpired";
    case "not_allowed":
    case "login_rejected":
      return "auth.oidcErrorNotAllowed";
    case "provider_unavailable":
    case "provider_error":
      return "auth.oidcErrorUnavailable";
    default:
      return "auth.oidcErrorFailed";
  }
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(() => readStoredUser());
  const [oidcError, setOidcError] = useState<string | null>(null);
  // 页面数据缓存按会话身份隔离：登录/登出/切换账号即清空，避免回放他人数据。
  const identityRef = useRef<string | null>(null);
  useEffect(() => {
    const identity = user ? `user:${user.id}` : null;
    if (identityRef.current !== identity) {
      identityRef.current = identity;
      clearAsyncCache();
    }
  }, [user]);

  const persist = useCallback((token: string, nextUser: User) => {
    setToken(token);
    writeStoredUser(nextUser);
    setUser(nextUser);
  }, []);

  const login = useCallback(
    async (username: string, password: string) => {
      const res = await api.login(username, password);
      persist(res.token, res.user);
    },
    [persist],
  );

  const bootstrap = useCallback(
    async (username: string, password: string) => {
      const res = await api.bootstrap(username, password);
      persist(res.token, res.user);
    },
    [persist],
  );

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } catch {
      /* 即便后端登出失败也清理本地会话 */
    }
    setToken(null);
    writeStoredUser(null);
    setUser(null);
  }, []);

  // 监听全局 401 事件：token 过期时自动清除本地会话（不再调后端 logout）
  useEffect(() => {
    const handleExpired = () => {
      setToken(null);
      writeStoredUser(null);
      setUser(null);
    };
    window.addEventListener(AUTH_EXPIRED_EVENT, handleExpired);
    return () => window.removeEventListener(AUTH_EXPIRED_EVENT, handleExpired);
  }, []);

  // FR-34：OIDC 回调以 URL 片段送回会话或错误码——先消费片段并清除（避免残留在地址栏与
  // 浏览器历史），再据结果建立会话或把错误交给登录框展示。
  useEffect(() => {
    const fragment = window.location.hash.startsWith("#") ? window.location.hash.slice(1) : "";
    const params = new URLSearchParams(fragment);
    const token = params.get("token");
    const error = params.get("error");
    if (!token && !error) {
      return;
    }
    window.history.replaceState({}, "", window.location.pathname + window.location.search);
    if (!token) {
      setOidcError(oidcErrorKey(error ?? ""));
      return;
    }
    setToken(token);
    // 片段只携带令牌：身份快照需另行取回（后端 /auth/me），取不到则视为登录失败。
    api
      .currentUser()
      .then((snapshot) => {
        writeStoredUser(snapshot);
        setUser(snapshot);
      })
      .catch(() => {
        setToken(null);
        setOidcError(oidcErrorKey("verify_failed"));
      });
  }, []);

  const clearOidcError = useCallback(() => setOidcError(null), []);

  const value = useMemo<AuthContextValue>(
    () => ({
      user,
      isAuthenticated: user !== null,
      login,
      bootstrap,
      logout,
      oidcError,
      clearOidcError,
    }),
    [user, login, bootstrap, logout, oidcError, clearOidcError],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

/** 读取鉴权上下文；必须在 AuthProvider 内使用。 */
export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error("useAuth 必须在 AuthProvider 内使用");
  }
  return ctx;
}
