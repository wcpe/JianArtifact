// users 命名空间文案（FR-111）：用户管理页（列表 / 新增 / 改角色 / 启用禁用 / 删除）。
export default {
  // 页面标题与主操作
  title: '用户管理',
  createUser: '新增用户',
  // 表头
  colUsername: '用户名',
  colRole: '角色',
  colStatus: '状态',
  colCreatedAt: '创建时间',
  colActions: '操作',
  // 状态徽章
  statusDisabled: '已禁用',
  statusNormal: '正常',
  // 行内操作
  enable: '启用',
  disable: '禁用',
  deleteUserAria: '删除用户',
  // 操作结果提示
  roleUpdated: '已更新角色',
  userEnabled: '已启用用户',
  userDisabled: '已禁用用户',
  userDeleted: '用户已删除',
  userCreated: '用户已创建',
  // 删除二次确认
  confirmDelete: '确认删除用户「{{username}}」？',
  // 新增用户弹窗
  createModalTitle: '新增用户',
  fieldUsername: '用户名',
  fieldPassword: '口令',
  fieldRole: '角色',
} as const;
