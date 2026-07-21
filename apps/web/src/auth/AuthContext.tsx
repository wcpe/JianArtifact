// 鉴权上下文：持有当前用户与会话令牌，暴露登录 / 自举 / 登出。
// 令牌与用户快照持久化到 localStorage，刷新后免重新登录（后端无 /me，故快照用户）。
import { createContext, useCallback, useContext, useMemo, useState } from "react";
import type { ReactNode } from "react";

import * as api from "../api/endpoints";
import { setToken } from "../api/client";
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
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(() => readStoredUser());

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

  const value = useMemo<AuthContextValue>(
    () => ({ user, isAuthenticated: user !== null, login, bootstrap, logout }),
    [user, login, bootstrap, logout],
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
