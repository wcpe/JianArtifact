# 功能规格：统一 tab 化监控页（FR-99）

> 状态：开发中　·　关联 PRD：FR-99　·　分支：feature/fr-99-monitor-page

## 1. 背景与目标

第四期（Web 控制台 UX 重构）epic 的一环（属 P2）。当前控制台把可观测性能力散在四处：使用分析（FR-58）、审计日志（FR-77）、防护监控（FR-78）各占一个独立导航入口，主机监控（FR-98 后端端点已落地）尚无前端入口。这导致导航条冗长、几类「看运行状况」的视图割裂。

目标：新建一个**统一「监控」页**，顶部 tab 切换四区，把上述三个已有独立页收进 tab 并新增「主机监控」tab；导航把原三个独立入口收敛为一个「监控」入口，给主机指标补手搓零依赖 SVG/CSS 图表。仅 Admin 可达；纯本机内部数据、不外发。

## 2. 需求（要什么）

- **统一监控页**：路由 `/monitor`，顶部 Mantine `Tabs` 切换四个区：主机监控 / 使用分析 / 审计 / 防护。仅 Admin。
- **主机监控 tab（新）**：消费 `GET /api/v1/monitor/host`（FR-98）。展示 CPU 使用率 + 逻辑核数、内存已用/总量、磁盘逐盘 + 汇总（手搓环形/进度图），系统 uptime；提供手动刷新（按请求采样，不做后台轮询）。
- **使用分析 tab**：复用既有 `AnalyticsPage` 组件（消费 `GET /api/v1/analytics/usage`），并补一组手搓图表（仓库用量条形复用既有 Progress，热门制品条形）。
- **审计 tab**：复用既有 `AuditPage` 组件（消费 `GET /api/v1/audit`）。
- **防护 tab**：复用既有 `ProtectionMonitorPage` 组件（消费 `GET /api/v1/protection/status|alerts`）。
- **导航整合**：`AppLayout` 把「使用分析 / 审计日志 / 防护监控」三个独立入口替换为单一「监控」入口（指向 `/monitor`），仅 Admin；保持 FR-92 折叠/角色门控/段精确高亮不回归。
- **手搓图表**：环形（CPU/内存/磁盘占比）、条形（热门制品下载）等用纯 SVG/CSS，零新增依赖。

- 范围内：新建 `MonitorPage` + 四 tab + 主机图表组件 + `getHostMonitor` 端点与类型 + 导航/路由整合。
- 不做（范围外）：不改后端（FR-98 端点已在 master）；不碰 FR-93 浏览页 / FR-96 设置页；不引图表库（@mantine/charts 等）；不做后台轮询主机指标的历史时序；不动被整合三页的数据逻辑（仅作为组件复用）。

## 3. 设计（怎么做）

- **复用而非重写**：`AnalyticsPage` / `AuditPage` / `ProtectionMonitorPage` 均为无 props、自带数据加载的自包含组件，直接作为 tab 面板内容挂载，数据层零改动 → 三页既有测试不回归。仅各自的旧路由从 `App.tsx` 移除（由 `/monitor` 统一承载）。
- **主机 tab**：新增 `HostMonitorPanel` 组件，`useEffect` 首次加载 `getHostMonitor()`，提供「刷新」按钮重新拉取（按请求采样，对齐 FR-98「不后台轮询」）。
- **手搓图表组件**（新建 `components/charts/`，纯 SVG/CSS、零依赖）：
  - `RingChart`：单值占比环形（用 SVG `circle` + `stroke-dasharray`），用于 CPU / 内存 / 磁盘占用率。
  - `BarList`：横向条形列表（CSS 宽度百分比），用于热门制品下载量对比（使用分析 tab 增强）。
- **类型对齐后端 DTO**（`src/monitor/mod.rs` 的 `HostMetrics`，serde 默认 snake_case）：
  - `HostMetrics { cpu: CpuMetrics, memory: MemoryMetrics, disk: DiskMetrics, uptime_secs: number }`
  - `CpuMetrics { usage_percent: number, logical_cores: number }`
  - `MemoryMetrics { total_bytes, used_bytes, swap_total_bytes, swap_used_bytes }`
  - `DiskMetrics { total_bytes, available_bytes, disks: DiskEntry[] }`
  - `DiskEntry { mount_point, total_bytes, available_bytes }`
- **端点**：`getHostMonitor(): Promise<HostMetrics>` → `GET /monitor/host`。
- **导航整合**：`AppLayout` 的 `NAV_ITEMS` 移除「使用分析 / 审计日志 / 防护监控」三项，新增一项「监控」`/monitor`（`adminOnly`）；`isNavActive` 段精确匹配逻辑不变（FR-92/fix-B 行为保持）。
- 无新 ADR：纯前端整合 + 复用既有端点 + 消费 master 已有 FR-98 端点，未引入新技术 / 新模式 / 推翻旧决策。

## 4. 任务拆分

- [x] `api/types.ts` 加 `HostMetrics` 系列类型；`api/endpoints.ts` 加 `getHostMonitor`
- [x] 测试先行：`MonitorPage` tab 切换 / 主机 tab 结构 / 三页复用可渲染 / 仅 Admin；`AppLayout` 导航收敛 + active 不串台
- [x] 实现 `MonitorPage` + `HostMonitorPanel` + 手搓图表组件；改 `App.tsx` 路由、`AppLayout` 导航
- [x] 文档同步：PRD 状态、ARCHITECTURE 一句、CHANGELOG 末尾追加一行

## 5. 验收标准

- `MonitorPage` 渲染四个 tab，默认主机监控 tab；点击其它 tab 切换到对应内容。
- 主机 tab mock `getHostMonitor` 后渲染 CPU / 内存 / 磁盘结构与 uptime；刷新按钮再次拉取。
- 使用分析 / 审计 / 防护 tab 复用既有组件，各自既有测试不回归（`pnpm -C frontend test` 全绿，含三页原用例）。
- `AppLayout`：导航不再有「使用分析 / 审计日志 / 防护监控」独立入口，出现单一「监控」入口（仅 Admin）；`/monitor` 时「监控」高亮，FR-92 折叠/角色门控/段精确高亮（含 `/protection` vs `/protection-monitor` 不串台的判定逻辑）不回归。
- `getHostMonitor` 返回类型字段与后端 `HostMetrics` DTO 一致。
- 验证门：`pnpm -C frontend test` + `pnpm -C frontend lint` + `pnpm -C frontend build` 全绿。

## 6. 风险 / 待定

- 被整合三页原各有顶层 `<Title order={2}>`（如「使用分析」），嵌进 tab 后与 tab 标签语义略重复——保留不改（避免改动三页正文，守复用不重写、不破坏其既有测试）。
- 主机 tab 首次采样 CPU 使用率可能为 0（FR-98 已知取舍），UI 照实展示、不特判。
