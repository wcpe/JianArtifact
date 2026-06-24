// 权限动作（四级，FR-48）展示辅助：下拉选项、中文标签、徽章配色。
// AclPanel 与组 ACL 面板共用，避免四级动作的展示逻辑重复散落。

import type { Permission } from '../api/types';

/** 四级动作下拉选项（高动作蕴含低动作）。 */
export const PERMISSION_OPTIONS: { value: Permission; label: string }[] = [
  { value: 'read', label: '读（read）' },
  { value: 'write', label: '写（write）' },
  { value: 'delete', label: '删除（delete）' },
  { value: 'admin', label: '管理（admin）' },
];

/** 动作 → 中文短标签。 */
const PERMISSION_LABELS: Record<Permission, string> = {
  read: '读',
  write: '写',
  delete: '删除',
  admin: '管理',
};

/** 动作 → 徽章配色（动作越高越醒目）。 */
const PERMISSION_COLORS: Record<Permission, string> = {
  read: 'blue',
  write: 'orange',
  delete: 'red',
  admin: 'grape',
};

/** 取动作的中文短标签。 */
export function permissionLabel(permission: Permission): string {
  return PERMISSION_LABELS[permission] ?? permission;
}

/** 取动作的徽章配色。 */
export function permissionColor(permission: Permission): string {
  return PERMISSION_COLORS[permission] ?? 'gray';
}
