# ADR-0003：管理端沿用 Mantine 7 + i18next 前端栈

## 状态

已接受

## 背景

后端从 Rust 改为 Go，但产品的管理端 UI 已在旧项目中成型并经过验证。重写需决定前端是另起炉灶还是复用旧项目的组件库与设计体系。约束：TypeScript strict、国际化、组件可复用、与 monorepo 工程化协同。

## 决策

前端沿用旧项目 UI 栈：**React + TypeScript(strict) + Vite + Mantine 7 + @tabler/icons-react + i18next**，共享组件与设计令牌下沉到 `packages/ui`。

## 理由

- 复用旧项目已验证的组件库与交互模式，显著降低重写风险与工时，产品观感一致。
- Mantine 7 组件完备、可主题化，配合 `@tabler/icons-react` 覆盖管理端所需控件；i18next 满足多语言。
- Vite 提供快构建与现代开发体验，天然融入 pnpm + Turborepo 任务图（ADR-0006）。
- TypeScript strict + `noUncheckedIndexedAccess` + `isolatedModules` 作为前端质量不变量。

## 后果

- 正面：UI 交付快、风格统一；`packages/ui` 集中主题与令牌，`apps/web` 与 `apps/wiki` 共享。
- 负面 / 约束：绑定 Mantine 大版本，升级需跟随其破坏性变更；`packages/ui` **不得反向依赖** `apps/*`（架构不变量）。

## 备选方案

- **换用其他组件库（MUI / Ant Design / shadcn）**：无复用收益且需重做设计体系，落选。
- **后端模板渲染无 SPA**：交互复杂度（迁移向导、仓库管理）不适合，且丢弃旧项目资产，落选。
