// tokens 命名空间文案（FR-111）：Token 管理页（自助签发 / 列表 / 吊销 API Token）。
export default {
  // 页面标题与主操作
  title: 'Token 管理',
  issue: '签发 Token',
  intro: 'API Token 供 CLI 与包管理器客户端鉴权使用；明文仅在签发时显示一次，请妥善保存。',
  empty: '暂无 Token。',
  // 表头
  colName: '名称',
  colCreatedAt: '创建时间',
  colLastUsed: '最近使用',
  colStatus: '状态',
  colActions: '操作',
  // 列表内容
  neverUsed: '从未使用',
  statusRevoked: '已吊销',
  statusActive: '有效',
  revoke: '吊销',
  // 操作结果提示
  tokenRevoked: 'Token 已吊销',
  // 吊销二次确认
  confirmRevoke: '确认吊销 Token「{{name}}」？吊销后立即失效。',
  // 签发弹窗
  createModalTitle: '签发 API Token',
  fieldName: '名称',
  namePlaceholder: '如 ci-pipeline',
  issueSubmit: '签发',
  // 已签发明文展示弹窗
  issuedModalTitle: 'Token 已签发',
  issuedWarning: '请立即复制并妥善保存。该明文仅显示这一次，关闭后将无法再次查看。',
  copyToken: '复制 Token',
  copied: '已复制',
  saved: '我已保存',
} as const;
