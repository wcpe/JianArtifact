// @jianartifact/ui —— 共享设计令牌、Mantine 7 主题与组件库入口。
// 供 apps/web（管理端）与 apps/wiki（验收站）消费；不得反向依赖 apps/*。
export { brandColor, tokens } from "./tokens";
export type { Tokens } from "./tokens";
export { theme } from "./theme";
export { AppProvider } from "./AppProvider";
export type { AppProviderProps } from "./AppProvider";
export { PageHeader } from "./components/PageHeader";
export type { PageHeaderProps } from "./components/PageHeader";
export { LoadingState, EmptyState, ErrorState, ForbiddenState } from "./components/StateMessage";
