// 任务中心 + 通知中心文案命名空间（FR-132）。
export default {
  // 页面标题
  title: '任务中心',
  // 空状态
  empty: '暂无任务',
  // 各字段标签
  kind: '类型',
  state: '状态',
  label: '描述',
  startedAt: '开始时间',
  finishedAt: '完成时间',
  updatedAt: '更新时间',
  // 任务类型
  kindMigration: 'Nexus 迁移',
  kindUpdate: '在线更新',
  kindVuln: '漏洞库刷新',
  // 任务状态
  stateRunning: '运行中',
  statePaused: '已暂停',
  stateSucceeded: '已完成',
  stateFailed: '失败',
  stateCancelled: '已取消',
  // 分区标题
  sectionActive: '活跃任务',
  sectionRecent: '近期历史',
  // 导航提示
  viewAll: '查看全部',
  // 通知文案
  notifyStarted: '{{label}} 已开始',
  notifySucceeded: '{{label}} 已完成',
  notifyFailed: '{{label}} 失败',
  notifyCancelled: '{{label}} 已取消',
  // 通知中心弹出列表
  noRecentTasks: '暂无近期任务',
  notificationCenterAriaLabel: '任务通知中心',
} as const;
