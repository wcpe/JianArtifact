// 验收站分区注册表：新增展台只需在此登记，Gallery 据此渲染导航与内容。
import type { ComponentType } from "react";

import { InteractionsSection } from "./InteractionsSection";
import { PageHeaderSection } from "./PageHeaderSection";
import { StatesSection } from "./StatesSection";
import { TokensSection } from "./TokensSection";

export interface Section {
  /** 稳定标识，用作导航 key。 */
  id: string;
  /** 导航与标题展示名。 */
  label: string;
  /** 一句话说明本展台验收什么。 */
  description: string;
  /** 展台内容组件。 */
  component: ComponentType;
}

export const sections: Section[] = [
  {
    id: "tokens",
    label: "设计令牌",
    description: "品牌色 / 圆角 / 间距令牌的可视化核对。",
    component: TokensSection,
  },
  {
    id: "page-header",
    label: "页头 PageHeader",
    description: "标题 / 描述 / 动作三态布局，与管理端页面顶部一致。",
    component: PageHeaderSection,
  },
  {
    id: "states",
    label: "状态态",
    description: "加载 / 空 / 错误 / 越权四类业务状态态。",
    component: StatesSection,
  },
  {
    id: "interactions",
    label: "关键交互",
    description: "表单校验与全局通知的“提交—反馈”业务模式。",
    component: InteractionsSection,
  },
];
