// search 命名空间文案（FR-111）：跨仓库制品搜索页。
export default {
  title: '制品搜索',
  keywordLabel: '关键字 / 坐标',
  keywordPlaceholder: '按制品路径关键字搜索',
  formatLabel: '格式',
  allFormats: '全部格式',
  // 仓库分组无障碍名（格式 + 仓库名）
  repoGroupAria: '{{format}} 仓库 {{repoName}}',
  // 分组节点命中数量后缀
  fileCount: '{{count}} 项',
  // 结果总数提示
  totalResults: '共 {{total}} 条结果',
  empty: '未找到匹配的制品。',
  initialHint: '输入关键字开始搜索。',
} as const;
