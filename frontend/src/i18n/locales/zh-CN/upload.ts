// upload 命名空间文案（FR-111）：通用制品上传页（选仓库 → 动态表单 → 选文件 → 上传）。
export default {
  // 页面标题
  title: '制品上传',
  // 目标仓库选择
  targetRepo: '目标仓库',
  repoPlaceholder: '选择一个 hosted 仓库（Maven / npm / Raw）',
  noRepoFound: '无可上传的 hosted 仓库',
  // Maven 坐标字段
  mavenGroupId: 'groupId',
  mavenArtifactId: 'artifactId',
  mavenVersion: 'version',
  // Maven 坐标可留空，由后端从 jar 内嵌 pom 自动识别（FR-123/120）
  mavenCoordsHint:
    '坐标可留空：上传含内嵌 pom 的 jar 时，服务端将自动识别 groupId / artifactId / version',
  // 可选 pom 文件（FR-123，pom 三级兜底「用户上传」层）
  mavenPomLabel: 'pom 文件（可选）',
  mavenPomPlaceholder: '可附带 {artifactId}-{version}.pom',
  mavenPomHint: '不附带时，服务端按 jar 内嵌 pom 或最小 pom 自动兜底',
  // 快照版提示（FR-122）：服务端生成时间戳唯一版本
  mavenSnapshotHint:
    '快照版：服务端将生成时间戳唯一版本（如 1.0-20260629.135300-1）并维护快照元数据',
  // npm 坐标字段
  npmName: '包名（name）',
  npmNamePlaceholder: 'lodash 或 @scope/pkg',
  npmVersion: '版本（version）',
  // raw 坐标字段
  rawPath: '目标路径（path）',
  // 文件选择
  fileLabel: '文件',
  filePlaceholder: '选择要上传的文件',
  // 上传进度
  progressAria: '上传进度',
  uploading: '上传中… {{progress}}%',
  // 主操作
  upload: '上传',
  // 操作结果提示
  uploadSuccess: '制品上传成功',
} as const;
