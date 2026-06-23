// 登录态上下文：持有当前用户、登录 / 登出动作，并在刷新后据令牌恢复会话。

import { createContext, useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { clearToken, getToken, setToken, setUnauthorizedHandler } from '../api/client';
import * as api from '../api/endpoints';
import type { UserInfo } from '../api/types';

/** 认证上下文取值。 */
export interface AuthContextValue {
  /** 当前用户；未登录为 null。 */
  user: UserInfo | null;
  /** 是否仍在恢复会话（首屏据令牌探测 /me）。 */
  loading: boolean;
  /** 是否为管理员。 */
  isAdmin: boolean;
  /** 登录：成功后写入令牌与用户。 */
  signIn: (username: string, password: string) => Promise<void>;
  /** 登出：清理令牌与用户。 */
  signOut: () => Promise<void>;
}

// eslint-disable-next-line react-refresh/only-export-components
export const AuthContext = createContext<AuthContextValue | null>(null);

/** 认证上下文 Provider：管理登录态生命周期。 */
export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<UserInfo | null>(null);
  const [loading, setLoading] = useState(true);

  // 注册 401 回调：会话失效时清理令牌与用户，路由守卫据此跳登录
  useEffect(() => {
    setUnauthorizedHandler(() => {
      clearToken();
      setUser(null);
    });
  }, []);

  // 首屏据已存令牌探测当前用户，恢复刷新前的会话
  useEffect(() => {
    let cancelled = false;
    const token = getToken();
    if (!token) {
      setLoading(false);
      return;
    }
    api
      .me()
      .then((info) => {
        if (!cancelled) setUser(info);
      })
      .catch(() => {
        // 令牌无效 / 过期：清理（401 回调已处理，这里兜底）
        if (!cancelled) {
          clearToken();
          setUser(null);
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const signIn = useCallback(async (username: string, password: string) => {
    const resp = await api.login(username, password);
    setToken(resp.access_token);
    setUser(resp.user);
  }, []);

  const signOut = useCallback(async () => {
    try {
      await api.logout();
    } catch {
      // 登出失败不阻断本地清理（无状态 JWT 下服务端无须配合）
    }
    clearToken();
    setUser(null);
  }, []);

  const value = useMemo<AuthContextValue>(
    () => ({
      user,
      loading,
      isAdmin: user?.role === 'admin',
      signIn,
      signOut,
    }),
    [user, loading, signIn, signOut],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}
