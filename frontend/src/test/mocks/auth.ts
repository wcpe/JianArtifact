// 测试鉴权夹具（FR-116，ADR-0035）：经真实 login 端点登录并把令牌写入 client 的存储，
// 使后续组件请求自动带上 Bearer 头、被 MSW 有状态 handlers 接受。

import { login } from '../../api/endpoints';
import { setToken } from '../../api/client';
import type { UserInfo } from '../../api/types';

/** 以指定凭据登录（默认内置管理员），写入令牌供后续请求使用，返回当前用户。 */
export async function loginAs(username = 'admin', password = 'admin123'): Promise<UserInfo> {
  const resp = await login(username, password);
  setToken(resp.access_token);
  return resp.user;
}
