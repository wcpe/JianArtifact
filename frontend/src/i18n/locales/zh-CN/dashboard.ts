// 仪表盘页文案命名空间（FR-111）。
export default {
  // 页头
  title: '仪表盘',
  welcome: '欢迎，{{username}}。',
  welcomeAdmin: '以下为本实例的全局状态概览。',
  welcomeBasic: '以下为当前可见范围内的基础信息。',
  // KPI 卡标题
  repoCount: '仓库数',
  artifactCount: '制品数',
  storageUsage: '存储用量',
  userCount: '用户数',
  visibleRepoCount: '可见仓库数',
  // 主机健康卡
  hostHealth: '主机健康',
  cpu: 'CPU',
  memory: '内存',
  disk: '磁盘',
  // 系统状态卡
  systemStatus: '系统状态',
  onlineUpdate: '在线更新',
  protection: '七层防护',
  vulnDb: '漏洞库',
  uptime: '运行时长',
  // 在线更新状态
  updateAvailable: '有新版 {{version}}',
  updateLatest: '已是最新',
  updateDisabled: '未启用',
  // 七层防护状态
  protectionOk: '正常',
  protectionAlert: '异常',
  // 漏洞库状态
  vulnEnabled: '已启用',
  vulnDisabled: '未启用',
  // 近期活动卡（已迁移，勿动）
  recentActivity: '近期活动',
  noActivity: '暂无活动记录',
} as const;
