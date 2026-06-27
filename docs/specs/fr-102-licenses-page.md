# 功能规格：开源许可页

> 状态：开发中　·　关联 PRD：FR-102　·　分支：feature/fr-102-licenses

## 1. 背景与目标

本产品是把众多 Rust crate 与前端 npm 包打进单一二进制的自包含程序。多数开源许可（MIT / Apache-2.0 / BSD 等）要求**分发时附带归因与许可证文本**。当前控制台无任何开源许可展示，存在合规缺口。

本 FR（P2，UX 重构 epic 收尾）提供一个**公开（匿名可访问）**的 `/licenses` 开源许可页，列出本产品全部依赖（Rust crates + 前端 npm，含运行时与开发依赖）的 包名 / 版本 / 许可证 / 作者。数据在**构建期自动扫描生成**并嵌入二进制，运行时只读、绝不外发（守 ADR-0009 数据不外发基调）。

## 2. 需求（要什么）

- **公开页 `/licenses`**：匿名可直达（参照 FR-95 公开路由层），版式为顶部四张统计卡（依赖总数 / 运行时依赖 / 开发依赖 / 许可证种类）+ 按包名过滤搜索框 + 按 运行时 / 开发 分组的表格（列：包名、版本、许可证、作者）。
- **公开端点 `GET /api/v1/licenses`**：匿名可读、不经鉴权门，返回结构化清单 + 汇总。
- **构建期生成、嵌入二进制**：Rust 侧用 `cargo-about` 扫描运行时（normal + build）crate 许可，dev-only crate 经 `cargo metadata` 差集补齐；前端侧用 `pnpm licenses list`（`--prod` 区分运行时 / 开发）。合并为一份结构化 JSON 嵌入二进制（`include_str!`）。
- **生成可复现 + 本地优雅降级**：提供 `scripts/gen-licenses.mjs` 生成脚本；CI 在编译二进制**前**跑它。本地未生成时端点返回空清单 + `generated=false` 标记，页面显「未生成」空态而非崩溃——缺数据不阻断编译 / 启动。
- 范围内：后端 `licenses` 薄模块 + 公开端点；前端 `LicensesPage` + 公开路由；生成脚本 + `about.toml` 配置 + CI 生成步骤。
- 不做（范围外）：不改导航入口（导航底部「开源许可」入口是 FR-101 的事，本 FR 只保证路由可直达）；不引运行时新依赖；不引前端图表库；许可数据不外发、不 phone-home；不做许可证文本全文展示（仅归因元数据：名 / 版本 / 许可证 / 作者）。

## 3. 设计（怎么做）

架构决策见 **ADR-0025**（构建期扫描 + 数据嵌入二进制 + 公开页），此处不重复决策正文。

- **数据模型（嵌入 JSON）**：`{ generated: bool, entries: [{ name, version, license, author, kind: "runtime"|"dev", source: "rust"|"frontend" }], summary: { total, runtime, dev, licenses } }`。生成脚本产出落 `src/licenses/data.generated.json`；仓库内提交一份 `generated=false` 的占位（空 entries），生成脚本覆盖之——使 `include_str!` 恒可编译（仿 `frontend/dist/.gitkeep` 占位思路）。
- **生成脚本 `scripts/gen-licenses.mjs`（Node，CI 已有 Node 20）**：
  - Rust 运行时：`cargo about generate --format json --offline`（按 `about.toml` 的 accepted 清单）→ 取 `crates[].package` 的 name/version/authors/license。
  - Rust 开发：`cargo metadata --format-version 1` 全图包集合 − 上述运行时集合（按 name@version 差集）= dev-only crate，license/authors 取 metadata 字段。
  - 前端：`pnpm -C frontend licenses list --json`（全量）与 `--prod --json`（运行时）；全量 − 运行时（按 name@version）= 开发依赖；author / license 取 pnpm 输出。
  - 合并去重、按 kind + name 排序，写 `src/licenses/data.generated.json`。
- **后端 `licenses` 模块（薄、静态嵌入资源，不读 DB）**：`include_str!` 嵌入 JSON，启动 / 首次访问惰性解析为 DTO；解析失败或 `generated=false` 时返回空清单 + `generated=false`（降级，不 panic）。守分层：`licenses` 不依赖 `meta` / DB / 网络（同 `monitor` 的「不碰 meta / DB」定位）。`api::licenses` 薄 handler 暴露公开 `GET /api/v1/licenses`（不调 `require_*`，匿名可读）。
- **前端 `LicensesPage`**：`App.tsx` 公开层加 `/licenses` 路由（匿名可达，复用 `AppLayout` 公开 shell）。页面调 `getLicenses()`（`api/endpoints`，匿名无 token 亦可）；四统计卡（依赖总数 / 运行时 / 开发 / 许可证种类）、按包名过滤搜索框、运行时 / 开发分组表格（包名 / 版本 / 许可证 / 作者），复用 Mantine 与现有 `StatCard` 风格；`generated=false` 显空态提示「许可清单未生成」。

## 4. 任务拆分

- [x] 复制模板 → `docs/specs/fr-102-licenses-page.md` 写规格
- [x] 写 ADR-0025 + 同步 `docs/adr/README.md`
- [x] PRD FR-102 行 计划→开发中
- [x] `about.toml`：cargo-about accepted 许可清单
- [ ] 测试先行（后端）：`licenses` 模块解析 / 降级纯逻辑 + `api::licenses` 端点匿名可读返回结构
- [ ] 测试先行（前端）：`LicensesPage` 统计卡数值、搜索过滤、分组表格渲染、空态降级（Vitest）
- [ ] 实现：`scripts/gen-licenses.mjs` + 占位 `src/licenses/data.generated.json` + `licenses` 模块 + `api::licenses` 端点 + `LicensesPage` + `/licenses` 公开路由 + `api/{types,endpoints}`
- [ ] CI：`release.yml` 在「构建前端 / 编译二进制」前加生成步骤
- [ ] 文档同步：PRD 状态、ARCHITECTURE（licenses 模块 + 公开端点）、API.md（GET /api/v1/licenses）、CHANGELOG 未发布段末尾追加一行
- [ ] 实跑生成脚本拿真实数据，验证端点 / 页面；恢复 `frontend/dist/.gitkeep`

## 5. 验收标准

- 后端 `cargo fmt --check` 干净、`clippy -D warnings` 零警告、`cargo test` 相关全绿：`licenses` 解析 / 降级纯逻辑、`GET /api/v1/licenses` **匿名可读**（200，非 401/403）且返回 `{generated, entries, summary}` 结构。
- 前端 `pnpm -C frontend build` + `pnpm -C frontend test` + lint 过：`LicensesPage` 四统计卡数值正确、搜索按包名过滤、运行时 / 开发分组表格渲染、`generated=false` 显空态。
- `/licenses` 路由匿名可直达（不跳登录）。
- **构建期真实扫描（实机维度，需用户确认）**：实跑 `node scripts/gen-licenses.mjs` 产出真实 `data.generated.json`（Rust + 前端、运行时 + 开发），端点 / 页面展示真实清单。本地 cargo-about 已装可现验；CI 维度（release 流水线生成步骤）待发版时复验。
- 未引入运行时新依赖、未引前端图表库；许可数据不外发、不 phone-home。

## 6. 风险 / 待定

- Rust 运行时 / 开发的切分依据「cargo-about 默认图（normal+build，排除 dev-dependencies） vs cargo metadata 全图差集」——dev-only crate 的 license 取自 `cargo metadata` 的 `license` 字段（SPDX 表达式原样透传，不做 cargo-about 的本地许可证文件确认）。
- `cargo about generate` 需联网解析部分许可（clearlydefined.io）；脚本默认 `--offline` 走本地，少数复杂许可可能落回默认，属可接受；`about.toml` 的 accepted 清单随依赖变动需维护（新依赖引入新许可时生成会报错，提示补 accepted）。
- 占位 `data.generated.json` 提交入库（非生成产物语义，是编译占位），与 `frontend/dist/.gitkeep` 同属「保证可编译的占位」，不违反「生成产物不入库」。
