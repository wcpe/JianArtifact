# 功能规格：仪表盘全局状态概览（FR-108）

> 状态：开发中　·　关联 PRD：FR-108（增强已交付 FR-18）　·　分支：feature/fr-108-dashboard

## 1. 背景与目标

控制台首页仪表盘（`frontend/src/pages/DashboardPage.tsx`）当前仅展示「当前用户 / 角色 / 可见仓库数 / 格式·类型分布」，信息过少，运维进首页无法一眼看清这台实例的全局健康。

目标（P2，UX 重构 epic）：把仪表盘做成**一眼看清全局状态**——顶部 KPI（仓库 / 制品 / 存储用量 / 用户数）+ 主机健康（CPU/内存/磁盘）+ 近期活动 + 系统状态（更新 / 防护 / 漏洞库 / 运行时长）。数字一律格式化（字节人类可读、计数千分位），**绝不出现 `121333 B` 这种原始字节**。

## 2. 需求（要什么）

范围内：
- **顶部 4 张 KPI 卡**：仓库数 · 制品数 · 存储用量（人类可读，如 `12.4 GB`）· 用户数。
- **主机健康**：CPU / 内存 / 磁盘 三条百分比进度条，取自 FR-98 `GET /api/v1/monitor/host`。
- **近期活动**：最近若干条审计事件（带相对时间），取自 FR-97 `GET /api/v1/audit`。
- **系统状态**：在线更新（有无新版）· 七层防护（正常 / 异常）· 漏洞库（启用 / 未启用）· 运行时长（uptime）。
- 数字格式化：字节走既有 `formatBytes`；计数千分位；uptime 人类可读（如 `3 天 4 小时`）。
- **按权限呈现**：KPI / 主机健康 / 近期活动 / 系统状态等数据源均为 **仅 Admin** 端点；非管理员仅见可降级展示的基础信息（当前用户 + 可见仓库数），不调管理端点、不报 403。

不做（范围外）：
- 不做富使用分析面板（访问 / 下载统计图表，属 FR-58 `/analytics` 独立页）。
- 不做时序折线（属 FR-99 `/monitor` 监控页）。
- 不加自动轮询 / 实时刷新（首页快照即可，避免 GitHub 限流与无谓负载）。
- 不为系统状态新增「整体正常 / 异常」后端判定字段；防护是否「正常」由前端据现有快照（活跃封禁数 / 窗内计数）判定。

## 3. 设计（怎么做）

复用既有端点优先；**仅** KPI 四元组（尤其制品总数与存储字节总和）无法低成本从现有前端端点拿到（`/repositories` 不含 size，逐仓库列制品是 N+1），故新增一个**薄聚合端点**。无架构决策（复用既有分层与 meta 计数）→ 不写 ADR。

### 3.1 后端：薄聚合端点 `GET /api/v1/dashboard/summary`（仅 Admin）

- 新增 `src/api/dashboard.rs`，handler 薄：`identity.require_admin()?` 后并发/顺序调 4 个 **既有 / 新增的 meta 计数方法**，组装 DTO 返回。只读、不入审计（GET）。
- DTO `DashboardSummaryDto { repo_count: i64, artifact_count: i64, total_bytes: i64, user_count: i64 }`。
- `meta` 计数方法：
  - `count_repositories()`（已存在，`src/meta/metrics.rs`）
  - `total_blob_bytes()`（已存在，按 sha256 去重求和）
  - `count_users()`（已存在，`src/meta/mod.rs`）
  - `count_artifacts()`（**新增**，`SELECT COUNT(*) FROM artifacts`，与现有 `count_distinct_blobs` 同形）
- 路由注册到 `src/api/mod.rs` 的 `/api/v1` 子路由表。

> 制品数语义：取 `artifacts` 表行数（制品索引条目数，含同一 blob 被多仓库引用的多条），与存储用量「去重字节」语义互补（一个是引用数、一个是占盘字节）。

### 3.2 前端：仪表盘重做

- `api/types.ts` 加 `DashboardSummary`；`api/endpoints.ts` 加 `getDashboardSummary()`。
- `lib/format.ts` 加纯函数：`formatCount`（千分位）、`formatUptime`（秒 → 人类可读时长）。相对时间（近期活动用）复用 `Intl` 或新增 `formatRelativeTime`。
- 重写 `DashboardPage.tsx`：
  - 顶部 4 KPI 卡（存储卡值走 `formatBytes(summary.total_bytes)`、计数走 `formatCount`）。
  - 主机健康：三条 `Progress`，百分比由 `monitor/host` 的 used/total 算（复用 FR-98 字段，前端算百分比）。
  - 近期活动：`audit` 最近 N 条，列出 action + actor + 相对时间。
  - 系统状态：四项徽章 / 文本——更新（`/update/check`，409=未启用静默）、防护（`/protection/status` 推导正常 / 异常）、漏洞库（`/settings/dynamic` 的 `vuln.enabled`）、运行时长（`monitor/host` 的 `uptime_secs` → `formatUptime`）。
  - **角色门控**：`useAuth().isAdmin` 为真才取上述管理端点并渲染富区；普通用户 / 匿名走降级（仅 `listRepositories` 的可见仓库数 + 当前用户）。各管理端点请求各自 `catch` 不互相阻断（某项失败只该卡显错 / 空，不拖垮整页）。

## 4. 任务拆分

- [ ] 后端：`meta` 加 `count_artifacts()` + 单测（空库 0 / 多制品计数）。
- [ ] 后端：新增 `src/api/dashboard.rs`（DTO + handler）+ 端点测试（匿名 401 / 普通用户 403 / Admin 200 且计数正确）。
- [ ] 后端：`src/api/mod.rs` 注册模块 + 路由。
- [ ] 前端：`api/{types,endpoints}` 加 `DashboardSummary` + `getDashboardSummary`。
- [ ] 前端：`lib/format.ts` 加 `formatCount` / `formatUptime`（+ 相对时间）+ 单测。
- [ ] 前端：重写 `DashboardPage.tsx`（KPI + 主机健康 + 近期活动 + 系统状态 + 角色门控）。
- [ ] 前端：`DashboardPage.test.tsx`（KPI 渲染、**存储字节格式化正确**、主机健康条、近期活动、系统状态、非 Admin 降级）。
- [ ] 文档同步：PRD FR-108 状态 计划 → 开发中；`docs/API.md` 加 `/dashboard/summary` 节；`CHANGELOG.md` 未发布段加一行。

## 5. 验收标准

- KPI 四卡渲染正确：存储用量显示人类可读体积（如 `12.4 GB`），**非原始字节串**；计数千分位。
- 主机健康三条进度条按 `monitor/host` 数据渲染百分比。
- 近期活动列出审计最近事件且带相对时间。
- 系统状态四项正确：更新（有更新显徽标、未启用 / 无更新静默）、防护（据快照显正常 / 异常）、漏洞库（启用 / 未启用）、运行时长（人类可读）。
- 非管理员进首页**不报 403**：仅见降级基础信息（当前用户 + 可见仓库数）。
- 后端端点鉴权矩阵：匿名 401 / 普通用户 403 / Admin 200，且 Admin 返回的四计数与库内实际一致（去重字节按 sha256 计一次）。
- 验证门：后端 `rustup run 1.96.0` fmt / clippy / test 全绿；前端 `pnpm -C frontend build` + `test`（含上述渲染与字节格式化用例）+ `lint` 全绿。
- 真机维度：本 FR 为纯展示聚合、无实机协议互通维度；端到端可用性由前端组件测试（mock 端点）+ 后端端点测试覆盖，无需额外手动实机验收。

## 6. 风险 / 待定

- 制品数语义采「索引行数」而非「去重 blob 数」（已在 §3.1 注明）；与存储「去重字节」并列时含义不同，属设计取舍、非 bug。
- 多个管理端点并发请求：各自独立 `catch`，单项失败不拖垮整页（已在设计中约束）。
