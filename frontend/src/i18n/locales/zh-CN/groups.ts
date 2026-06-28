// groups 命名空间文案（FR-111）：用户组管理页 + 仓库 ACL 面板 + 组 ACL 面板共用此 ns。
export default {
  // 用户组管理页：标题与主操作
  title: '用户组管理',
  createGroup: '新增用户组',
  empty: '暂无用户组。',
  // 用户组表头
  colName: '组名',
  colCreatedAt: '创建时间',
  colActions: '操作',
  // 行内操作
  members: '成员',
  deleteGroupAria: '删除用户组',
  // 操作结果提示
  groupDeleted: '用户组已删除',
  groupCreated: '用户组已创建',
  // 删除二次确认
  confirmDelete: '确认删除用户组「{{name}}」？将同时清理其成员与组授权。',
  // 新增用户组弹窗
  createModalTitle: '新增用户组',
  fieldName: '组名',
  namePlaceholder: '如 dev-team',
  // 组成员管理弹窗
  membersModalTitle: '「{{name}}」成员管理',
  membersModalTitleFallback: '成员管理',
  addMember: '添加成员',
  selectUserPlaceholder: '选择用户',
  join: '加入',
  memberAdded: '已加入成员',
  memberRemoved: '已移出成员',
  noMembers: '该组暂无成员。',
  colMember: '成员',
  removeMemberAria: '移出成员',
  // 仓库 ACL 面板（每仓库用户授权）
  aclUser: '用户',
  aclUserPlaceholder: '选择用户',
  aclPermission: '权限',
  aclGrant: '授权',
  aclGranted: '已新增授权',
  aclRemoved: '已移除授权',
  aclEmpty: '该仓库暂无 ACL 授权条目。',
  aclColUser: '用户',
  aclColPermission: '权限',
  aclColActions: '操作',
  aclRemoveAria: '移除授权',
  // 组 ACL 面板（每仓库用户组授权）
  groupAclGroup: '用户组',
  groupAclGroupPlaceholder: '选择用户组',
  groupAclPermission: '权限',
  groupAclGrant: '授权',
  groupAclGranted: '已对组新增授权',
  groupAclRemoved: '已撤销组授权',
  groupAclEmpty: '该仓库暂无组 ACL 授权条目。',
  groupAclColGroup: '用户组',
  groupAclColPermission: '权限',
  groupAclColActions: '操作',
  groupAclRemoveAria: '撤销组授权',
} as const;
