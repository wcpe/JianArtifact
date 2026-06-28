// 侧栏导航文案命名空间（FR-111）：分段标题 + 各导航项 + 外壳通用。
export default {
  // 分段标题
  sectionBrowse: '浏览',
  sectionManage: '管理',
  sectionSystem: '系统 · 监控',
  // 导航项
  dashboard: '仪表盘',
  repositories: '仓库',
  search: '搜索',
  usersGroups: '用户与组',
  tokens: '访问令牌',
  upload: '上传',
  migration: 'Nexus 迁移',
  monitor: '监控',
  audit: '审计日志',
  systemLogs: '系统日志',
  system: '系统',
  settings: '设置',
  // 外壳通用
  searchPlaceholder: '搜索制品（回车或停顿即搜）',
  searchAriaLabel: '全局搜索',
  signIn: '登录',
  signOut: '登出',
  collapse: '收起',
  expand: '展开',
  collapseNav: '收起导航',
  expandNav: '展开导航',
  toggleNav: '切换导航展开收起',
  openSourceLicenses: '开源许可',
  // 用户身份后缀（FR-95 页眉）：「<用户名>（<角色>）」
  userSuffix: '（{{role}}）',
  // 更新徽标（FR-101，仅 Admin 且确有可更新时显）
  updateBadge: '更新: {{current}} → {{latest}}',
  updateBadgeAriaLabel: '有可用更新，点击前往设置页升级',
} as const;
