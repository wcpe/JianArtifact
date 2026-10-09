// 集中式权限判断：把散落各页的 `user?.role === "admin"` 收敛到一处。
//
// 为什么抽它：同一句判断此前散落在 AppLayout、各列表页与仓库详情页里，新页面要各自复制
// 一遍；一旦角色口径再变（例如后续引入更细的角色），改动面会随 copy 数线性放大。这里提供
// 唯一入口——页面只读 `isAdmin`，不再自己解释 role 字段。
//
// 范围说明：本模块只做「是否是管理员」这一种判定；更细的资源级权限由后端 ACL 决定，
// 前端不做二次猜测（猜错会把越权操作暴露给用户，再被后端 403 打回）。
import { useAuth } from "../auth/AuthContext";

export type { UserRole } from "../api/types";

/** 当前登录用户是否拥有管理员角色（未登录恒为 false）。 */
export function useIsAdmin(): boolean {
  const { user } = useAuth();
  return user?.role === "admin";
}
