// audit 命名空间文案（FR-111）。
export default {
  // 页头
  title: '审计日志',
  description: '记录写 / 管理 / 授权拒绝类安全事件，按时间倒序。点击任意行查看详情。',
  // 过滤表单
  actorPlaceholder: '按用户名过滤',
  actionPlaceholder: '如 repo.create',
  repoPlaceholder: '按仓库名过滤',
  query: '查询',
  // 列表
  empty: '暂无审计记录。',
  totalRecords: '共 {{count}} 条记录',
  // 字段标签（表头与详情共用）
  time: '时间',
  actor: '操作者',
  actorKind: '身份种类',
  action: '动作',
  result: '结果',
  repo: '仓库',
  object: '对象',
  sourceIp: '来源 IP',
  requestId: '请求 ID',
  // 详情弹窗
  detailTitle: '审计详情',
  detail: '补充',
} as const;
