// protectionMonitor 命名空间文案（FR-111）：防护监控页专属文案。
export default {
  title: '防护监控',
  description: '七层防护各维度窗内计数快照与告警历史；数据为本机内部统计，不外发。每 5 秒自动刷新。',
  // 统计卡标签
  activeBannedIps: '当前封禁 IP 数',
  alertsEval: '告警评估',
  windowSecs: '评估窗口（秒）',
  // 告警评估状态（已启用复用 common:enabled；已停用措辞不同，单列）
  alertsDisabled: '已停用',
  // 卡片标题
  windowCounts: '各维度窗内计数',
  alertList: '告警列表',
  // 告警总数（插值）
  total: '共 {{count}} 条',
  noAlerts: '暂无告警记录',
  // 告警表头
  thTime: '时间',
  thDimension: '维度',
  thSeverity: '严重度',
  thObserved: '观测值',
  thThreshold: '阈值',
  thDetail: '详情',
} as const;
