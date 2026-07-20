# ADR-0006：单 monorepo，前端 pnpm+Turborepo、后端 Go Task，Makefile 顶层入口

## 状态

已接受

## 背景

项目含 Go 后端、React 管理端、组件验收站与多个前端共享包，且以单二进制协同交付。需决定代码仓库组织方式与构建编排，使跨工程改动可原子提交、任务可缓存、本地与 CI 入口统一。

## 决策

采用**单 monorepo**。前端用 **pnpm workspace + Turborepo** 组织 `apps/{web,wiki}` 与 `packages/{ui,devmock,eslint-config,typescript-config}` 的任务图与缓存；后端用 **Go Task**（`Taskfile.yml`）编排 `gen/lint/test/vet/vuln/build/migrate`；根 **Makefile** 为统一顶层入口（`install/dev/check/build/release/deploy/rollback`），委托到 Turborepo 与 Task。`go.work` 组织 Go 模块。

## 理由

- 前后端契约（ADR-0004）与单二进制交付（ADR-0005）高度耦合，monorepo 让契约变更与前后端适配在一次提交内完成。
- Turborepo 提供前端任务图与增量缓存；Go Task 贴合 Go 工具链；Makefile 给出与语言无关的统一命令面，本地与 CI 复用同一组脚本（质量不变量）。
- 四个前端子工程各自独立 typecheck/lint/test/build，职责清晰。

## 后果

- 正面：跨工程原子提交、统一入口、缓存加速；新贡献者只需记 `make` 目标。
- 负面 / 约束：仓库较大、工具链需 pnpm + Go + Task 齐备（由 Docker 质量门统一封装）；层次依赖方向须守约（`packages/ui` 不反向依赖 `apps/*`）。

## 备选方案

- **多仓库（前后端分仓）**：契约变更需跨仓协调、易漂移，落选。
- **仅用 npm/yarn workspaces 无 Turborepo**：缺任务图与缓存，大仓构建慢，落选。
- **仅用 Makefile 不引入 Task**：Go 侧任务表达力弱、可读性差，落选。
