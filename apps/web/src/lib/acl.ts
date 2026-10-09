// ACL 编辑的共享口径（FR-36）：六档动作、主体类型判定与展示名解析。
//
// 抽成模块的原因：仓库详情页的 ACL 面板与（兼容重定向前的）ACL 页都要用同一套
// 「主体 → 展示名」「动作 → 文案」映射，两处各写一遍就会出现同一条授权在两个页面
// 显示成不同名字的情况。
import type { AclAction, AclEntry, AclSubjectType } from "../api/types";

/** FR-36 六档动作，由粗到细；顺序即下拉里的展示顺序。 */
export const ACL_ACTIONS: AclAction[] = [
  "read",
  "publish",
  "write",
  "delete",
  "acl_manage",
  "admin",
];

/** 每条动作对应的中文说明文案键（放进「说明」气泡，不常驻占版）。 */
export const ACL_ACTION_HINT_KEYS: Record<AclAction, string> = {
  read: "acl.actionHintRead",
  publish: "acl.actionHintPublish",
  write: "acl.actionHintWrite",
  delete: "acl.actionHintDelete",
  acl_manage: "acl.actionHintAclManage",
  admin: "acl.actionHintAdmin",
};

/** 每条动作对应的动作名文案键。 */
export const ACL_ACTION_LABEL_KEYS: Record<AclAction, string> = {
  read: "acl.actionRead",
  publish: "acl.actionPublish",
  write: "acl.actionWrite",
  delete: "acl.actionDelete",
  acl_manage: "acl.actionAclManage",
  admin: "acl.actionAdmin",
};

/**
 * 条目主体类型：缺省按 user 解释。
 *
 * 契约允许不传 subjectType（既有三档时代的请求体只有 subjectId），沿用该缺省即可
 * 让老数据无改动继续工作；这里补出显式类型，供 UI 与去重判定使用。
 */
export function aclSubjectType(entry: AclEntry): AclSubjectType {
  return entry.subjectType === "group" ? "group" : "user";
}

/**
 * 条目主体在 UI 里的稳定标识：`user:12` / `group:3`。
 *
 * 为什么不用数组下标：保存是整份覆盖写，列表会随增/删/重排变化，用下标会让 React 复用
 * 错行（改 A 行的权限却看到 B 行变了）。
 */
export function aclSubjectKey(entry: AclEntry): string {
  return aclSubjectType(entry) === "group"
    ? `group:${entry.subjectGroupId ?? 0}`
    : `user:${entry.subjectId ?? 0}`;
}

/**
 * 渲染主体的展示名：用户主体用用户名、组主体用组名。
 *
 * 查不到（用户已删 / 组已删 / 列表未加载完）时回退到「用户 #id」「用户组 #id」——
 * 不能回退到 `#0`：组主体条目本来就没有 subjectId，回退 0 会把所有未知组显示成同一行。
 */
export function aclSubjectLabel(
  entry: AclEntry,
  names: { userNames: Map<number, string>; groupNames: Map<number, string> },
  fallback: { user: (id: number) => string; group: (id: number) => string },
): string {
  if (aclSubjectType(entry) === "group") {
    const id = entry.subjectGroupId ?? 0;
    return names.groupNames.get(id) ?? fallback.group(id);
  }
  const id = entry.subjectId ?? 0;
  return names.userNames.get(id) ?? fallback.user(id);
}

/**
 * 按契约构造一条主体条目：两个 ID 字段互斥，只填当前类型对应那一列。
 *
 * 另一列显式置 undefined（而不是留着旧值）是为了避免「用户条目被改成组条目后仍带着
 * subjectId」——服务端虽按 subjectType 只读一列，但带着脏值会让去重与展示判定误判。
 */
export function buildAclEntry(
  subjectType: AclSubjectType,
  subjectId: number,
  action: AclAction,
): AclEntry {
  return subjectType === "group"
    ? { subjectType: "group", subjectGroupId: subjectId, subjectId: undefined, action }
    : { subjectType: "user", subjectId, subjectGroupId: null, action };
}
