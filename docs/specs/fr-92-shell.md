# 功能规格：折叠图标导航条 + 全站信息密度重构（骨架）

> 状态：开发中　·　关联 PRD：FR-92　·　分支：feature/fr-92-shell

## 1. 背景与目标

控制台 UX 重构 epic（PRD §7 第四期）的**地基**，纯前端。当前侧栏是固定宽度的图标+文字 `NavLink` 列表，页面普遍「卡片一味纵向铺开、密度低」。本 FR 把侧栏改成**可折叠的图标导航条**（默认窄、可展开），并立一套**信息密度基线**，应用到 shell 外壳与仪表盘页作示范。属 P2（第四期 epic 第一砖）。

后续页面级重构（FR-93 浏览树/详情、FR-94 页眉搜索、FR-95 角色页、FR-96 设置页、FR-99 监控页）各自跟进自身密度细化；**本 FR 只动 shell + 导航 + 密度基线，不逐页改全站**。

## 2. 需求（要什么）

- 范围内：
  - **折叠图标导航条**：侧栏默认**窄**（仅图标 + tooltip），可点击切换展开为「图标 + 文字」。导航顶部一个展开/收起切换控件。各导航项一个 Tabler 风格图标（沿用现有图标）。
  - **窄态可访问性**：折叠时每项有可访问的 `aria-label`/label 与 hover tooltip，键盘与读屏可用。
  - **角色门控入口**：管理类入口（用户管理、用户组管理、使用分析、防护配置、审计日志、防护监控、Nexus 迁移、设置）**仅 Admin 可见**，沿用现有 `useAuth().isAdmin` + `adminOnly` 标记。非 Admin 仅见仪表盘 / 仓库管理 / 制品搜索 / Token 管理 / 制品上传。
  - **active 高亮按路径段精确匹配**：保持 master 上 fix-B 行为（`/protection` 不被 `/protection-monitor` 串台），不退回前缀匹配。
  - **信息密度基线**：一套可复用的密度约定（间距 / 字号 / 卡片瘦身）集中一处，应用到 shell（AppShell padding、navbar）+ 仪表盘页（卡片瘦身、更紧的栅格间距）作示范。
  - 页眉可为 FR-94 全局搜索框**留位置/占位**（不实现搜索逻辑）。
- 不做（范围外）：
  - FR-93 浏览树/详情、FR-94 页眉搜索逻辑、FR-95 角色页内容、FR-96 设置页、FR-99 监控页 —— 那些是后续 FR 的页面逻辑，本 FR **不碰**（除导航入口的角色门控外）。
  - 不逐页重排全站其余页面的密度（交后续 FR 各自跟进）。
  - 不新增前端依赖（用现有 Mantine + @tabler/icons-react）。
  - 折叠状态持久化（localStorage）等增强非必需，本 FR 用组件内状态即可，不镀金。

## 3. 设计（怎么做）

- `frontend/src/components/AppLayout.tsx`：
  - 用 Mantine `AppShell` 的 `navbar.width` 在**窄（图标条，约 64px）/ 宽（图标+文字，约 240px）**间切换；桌面折叠态用一个 `useDisclosure`（默认折叠=窄）控制，移动端沿用既有 `opened` 抽屉逻辑。
  - 导航项渲染：宽态用 `NavLink`（图标 + label）；窄态用 `Tooltip`(label, position=right) 包裹一个仅图标、带 `aria-label` 的可点击控件（`NavLink` 不传 label，靠 `aria-label` 提供无障碍名）。两态共用同一 `NAV_ITEMS` 与 `isNavActive`（段精确匹配，原样保留）。
  - 导航顶部加展开/收起切换按钮（`IconLayoutSidebar*` 或现有图标），带 `aria-label`。
  - 角色门控不变：`visibleItems = NAV_ITEMS.filter((i) => !i.adminOnly || isAdmin)`。
  - 页眉为 FR-94 留一个占位区域（禁用的搜索框占位，注明 FR-94 实现），不接逻辑。
- 密度基线：新增 `frontend/src/theme/density.ts`（或等价共享常量模块），集中导出间距 / 卡片 padding / 栅格 gap 等密度 token；shell 与仪表盘引用，避免魔法值散落。
- `frontend/src/pages/DashboardPage.tsx`：按密度基线把卡片 padding 收紧（`lg`→更紧）、栅格 `spacing` 收紧、统计卡瘦身，作密度示范。不改其数据逻辑与 FR-18 范围（仍只展示基础信息）。
- 无新 ADR：UI 重排、行为不变（导航语义、角色门控、路由均不变）。如评估需 ADR 先停下汇报。

## 4. 任务拆分

- [x] 写本 spec + PRD FR-92 状态 计划→开发中（只改该行）
- [x] 测试先行（Vitest，仿现有 AppLayout.test.tsx）：默认窄 / 可展开切换；active 按段精确高亮（保持 fix-B）；非 Admin 看不到管理入口、Admin 看得到；窄态有可访问 label/aria
- [x] 实现：折叠图标导航 + 角色门控 + 密度基线模块 + 仪表盘密度示范 + 页眉搜索占位
- [x] 文档同步：ARCHITECTURE 前端 shell 一句（如有结构变化）、CHANGELOG 未发布段末尾追加一行、本 spec 勾选
- [x] 验证门：pnpm test / lint / build 全过

## 5. 验收标准

- 侧栏默认窄（图标条），点击切换可展开为图标+文字、再切回窄（有自动化测试断言两态）。
- 窄态每个导航项有可访问名（`aria-label` 或 tooltip label），读屏 / 键盘可用（测试断言 `aria-label` 存在）。
- 角色门控：非 Admin 上下文下管理入口（用户管理 / 用户组管理 / 使用分析 / 防护配置 / 审计日志 / 防护监控 / Nexus 迁移 / 设置）均不可见；Admin 上下文下可见（测试断言两侧）。
- active 高亮按段精确匹配：`/protection-monitor` 下仅「防护监控」高亮、「防护配置」不串台；子路径 `/repositories/libs` 下「仓库管理」仍高亮；根 `/` 仅「仪表盘」高亮（沿用既有 fix-B 测试，保持绿）。
- `pnpm -C frontend test`（含新 AppLayout 用例）绿、`pnpm -C frontend lint` 过、`pnpm -C frontend build` 过。
- 范围克制：未改动 FR-93~99 各页面的页面逻辑（仅导航入口角色门控与 shell/仪表盘密度）。

## 6. 风险 / 待定

- 窄态 active 高亮的视觉对齐：Mantine `NavLink` 无 label 时仍能显示 `data-active`，测试以 `data-active` 断言，不依赖像素。
- 折叠态持久化：本 FR 不做（组件内状态即可），如后续要记忆偏好再加，避免镀金。
