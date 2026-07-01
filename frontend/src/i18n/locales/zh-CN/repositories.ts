// repositories 命名空间文案（FR-111 + FR-135）：仓库管理页（列表 / 创建 / 删除 / 连通性测试）。
export default {
  // 页头与列表
  title: '仓库管理',
  createRepo: '创建仓库',
  emptyHint: '暂无可见仓库。',
  // 表头
  colName: '名称',
  colFormat: '格式',
  colType: '类型',
  colVisibility: '可见性',
  colArtifactCount: '制品数',
  colTotalSize: '总大小',
  colStatus: '状态',
  colUpstream: '上游',
  colActions: '操作',
  // 行内操作（无障碍标签）
  configBrowse: '配置 / 浏览',
  deleteRepo: '删除仓库',
  testConnectivity: '测试连通性',
  // 连通性测试（FR-135）
  connectivityModalTitle: '连通性测试：{{name}}',
  connectivitySuccess: '连通成功',
  connectivityFail: '连通失败',
  connectivityStatus: '状态码：{{status}}　',
  connectivityElapsed: '耗时：{{ms}} ms',
  connectivityUnknownError: '未知错误',
  // 删除确认与提示
  deleteConfirm: '确认删除仓库「{{name}}」？该操作不可撤销。',
  deleteSuccess: '仓库已删除',
  createSuccess: '仓库已创建',
  // 创建弹窗
  modalTitle: '创建仓库',
  nameLabel: '仓库名',
  namePlaceholder: '如 maven-releases',
  formatLabel: '格式',
  typeLabel: '类型',
  typeHosted: '托管（hosted）',
  typeProxy: '代理（proxy）',
  visibilityLabel: '可见性',
  visibilityPrivate: '私有（private）',
  visibilityPublic: '公开（public）',
  upstreamLabel: '上游地址',
  create: '创建',
  // 格式选项（下拉标签，含中文说明的逐字照搬）
  formats: {
    maven: 'Maven',
    npm: 'npm',
    docker: 'Docker / OCI',
    raw: 'Raw 通用文件',
    cargo: 'Cargo',
    go: 'Go 模块',
    pypi: 'PyPI',
    nuget: 'NuGet',
  },
} as const;
