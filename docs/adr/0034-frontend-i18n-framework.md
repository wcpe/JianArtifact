# ADR-0034：前端国际化（i18n）框架与文案组织

## 状态

已接受　·　FR-111

## 背景

前端文案此前全部硬编码中文，散落各组件；审计 / 近期活动的动作 key（`artifact.upload` 等）直接显原始串、未翻译。需要一套 i18n 机制：① 把文案集中到 locale、可维护；② 把动作 key 译成中文标签；③ 为将来多语言留扩展位（本期只交付中文）；④ 不破坏现有测试与构建。

## 决策

1. **框架**：引入 `i18next` + `react-i18next`（React 生态事实标准、按 ns 懒加载 / 分文件、与 vitest 兼容）。在 `src/i18n/index.ts` 初始化全局单例，`main.tsx` 与测试 `setup.ts` 顶部 `import './i18n'` 装载；组件经 `useTranslation(ns)` 取 `t`。
2. **本期只交付 zh-CN**：`lng=fallbackLng='zh-CN'`、默认且唯一语言。**不做语言切换 UI / 多语言文案**（YAGNI）——但结构上为扩展预留：加语言只需在 `locales/<lng>/` 下补同名 ns 文件。
3. **按命名空间分文件**：`src/i18n/locales/zh-CN/<ns>.ts`，**每页一个 ns 文件**（`dashboard` / `settings` / `repositories` …）+ 共享 `common` + 导航 `nav` + 错误 `errors` + 审计动作 `auditActions`。**分文件是为并行迁移防写冲突**：各页迁移只动自己的 ns 文件，不抢一个大 locale。
4. **key 规范**：`useTranslation('<ns>')` 后 `t('<key>')`；跨命名空间用 `t('common:save')` 前缀。`returnNull:false`、缺 key 回落 key 本身，便于发现未迁移项。
5. **审计动作 key**：后端 action 含 `.`（与 i18next 默认 keySeparator `.` 冲突），故 `auditActions` ns 的键名把 `.` 归一为 `_`，经 `tAuditAction(action)`（`i18n/index.ts`）归一后查，未知动作回落原始串。
6. **测试**：`setup.ts` 装载 i18n，组件渲染真实中文；迁移**保持中文文案逐字不变**（locale value = 原硬编码串），故断言可视中文的既有测试不破；断言原始 key 的测试改断言译后标签。

## 理由

- i18next 是成熟标准，ns 分文件天然支持并行维护与按需加载；纯前端、不碰后端 / API 契约。
- 只交付中文 + 不做切换 UI：满足当前需求、不镀金；分文件 + key 规范让未来加语言成本低。
- 文案逐字不变：把"全站迁移"的风险降到最低（视觉零变化、测试基本不动）。

## 后果

- 正面：文案集中可维护、动作 key 中文化、为多语言预留；后续 FR（115~119）在此之上写 i18n-native 代码。
- 负面 / 约束：新增运行时依赖 `i18next` + `react-i18next`（体积可控、纯前端）；今后新增前端文案**应进 locale、不再硬编码**（CI lint 暂不强制，靠评审 + 本 ADR 约定）。
- 迁移为渐进式：框架落地后分批把各页文案搬入 ns；完成判据为全站无残留硬编码业务文案 + 全量 vitest 绿 + build 绿。

## 备选方案

- **纯 TS 文案字典（不引框架）**：自造 `t()` + 字典，省依赖但缺插值 / 复数 / 多语言切换 / 生态工具，且仍要自管 ns，落选。
- **单一大 locale 文件**：简单但并行迁移必抢写冲突、文件巨大难维护，落选（选按 ns 分文件）。
- **现在就做多语言 + 切换 UI**：本期无多语言需求，属镀金，落选（只交付中文、结构预留）。
