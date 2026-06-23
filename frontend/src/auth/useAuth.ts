// 取认证上下文的便捷 hook：脱离 Provider 使用即报错，避免空值蔓延。

import { useContext } from 'react';
import { AuthContext, type AuthContextValue } from './AuthContext';

/** 读取认证上下文；必须在 AuthProvider 内使用。 */
export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error('useAuth 必须在 AuthProvider 内使用');
  }
  return ctx;
}
