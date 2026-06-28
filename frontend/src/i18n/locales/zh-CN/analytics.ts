// 使用分析页文案命名空间（FR-111）。
export default {
  // 页头
  title: '使用分析',
  subtitle: '访问量 / 下载量、热门制品与仓库用量；数据为本机内部统计，不外发。',
  // 统计卡
  totalAccess: '累计访问量',
  totalDownload: '累计下载量',
  // 卡片标题
  topDownloads: '热门制品（按下载量）',
  repoUsage: '仓库用量（按下载量）',
  // 空态
  noDownloadRecords: '暂无下载记录',
  // 热门制品表头
  colRepo: '仓库',
  colArtifactPath: '制品路径',
  colDownloadCount: '下载量',
  // 制品路径占位（仓库级聚合，无具体路径时）
  repoLevel: '（仓库级）',
} as const;
