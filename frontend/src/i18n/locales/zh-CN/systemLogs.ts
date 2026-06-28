// systemLogs 命名空间文案（FR-111，待各页迁移时填充）。
export default {
  // 页头
  title: '系统日志',
  subtitle:
    '应用运行时技术日志（按级别 ERROR / WARN / INFO / DEBUG），最新在前。与审计日志（业务留痕）不同。',
  // 级别过滤
  levelLabel: '级别',
  allLevels: '全部级别',
  // 列表
  empty: '暂无日志记录。',
  recordCount: '共 {{count}} 条记录',
  // 表头
  columnTimestamp: '时间',
  columnLevel: '级别',
  columnMessage: '消息',
} as const;
