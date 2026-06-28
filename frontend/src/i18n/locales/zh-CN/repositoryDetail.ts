// repositoryDetail 命名空间文案（FR-111）：仓库详情页（浏览 / 配置 / ACL）。
export default {
  // 缺失 / 错误态
  missingId: '缺少仓库标识',
  notFound: '仓库不存在',
  backToList: '返回仓库列表',
  // 页签
  tabBrowse: '浏览',
  tabConfig: '配置',
  tabAcl: '权限（ACL）',
  // ACL 区
  aclUsers: '用户授权',
  aclGroups: '用户组授权',
  // 浏览页签
  emptyArtifacts: '该仓库暂无制品。',
  selectFileHint: '从左侧选择一个文件查看详情。',
  artifactNotFound: '制品不存在',
  deleteArtifact: '删除制品',
  deleteConfirm: '确认删除制品「{{path}}」？',
  deleteSuccess: '制品已删除',
  // 配置页签
  configSaved: '仓库配置已更新',
  visibilityLabel: '可见性',
  visibilityPrivateOption: '私有（private）',
  visibilityPublicOption: '公开（public）',
  upstreamUrlLabel: '上游地址',
} as const;
