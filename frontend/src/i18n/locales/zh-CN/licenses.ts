// 开源许可页文案命名空间（FR-111）。
export default {
  // 页头
  title: '开源许可',
  description:
    '本产品依赖的开源组件及其许可证与作者；清单由构建期扫描生成，数据为本机内部、不外发。',
  // 清单未生成提示
  notGeneratedTitle: '许可清单未生成',
  notGeneratedBody:
    '当前二进制未嵌入开源许可清单（本地开发未运行生成脚本）。正式发布版会在构建期自动生成并嵌入。',
  // 统计卡片
  statTotal: '依赖总数',
  runtimeDeps: '运行时依赖',
  devDeps: '开发依赖',
  statLicenses: '许可证种类',
  // 过滤搜索框
  filterPlaceholder: '按包名过滤…',
  filterAriaLabel: '按包名过滤',
  // 依赖表格
  noMatch: '无匹配依赖',
  colName: '包名',
  colVersion: '版本',
  colLicense: '许可证',
  colAuthor: '作者',
} as const;
