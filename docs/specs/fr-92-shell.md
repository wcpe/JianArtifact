# 功能规格：控制台外壳重做（logo + 分段导航 + 点 logo 切换 + 左下许可 + 全局 max-width）

> 状态：开发中　·　关联 PRD：FR-92　·　分支：feature/fr-92-shell

## 1. 背景与目标

控制台 UX 重构 epic（PRD §7 第四期）的**外壳**，纯前端。早先版本（折叠图标导航条 + 密度基线）已落地，但用户对当前外壳强烈不满。本 FR 按**已确认设计**重做控制台外壳 `AppLayout`：把品牌 / 版本 / 切换控件收拢到**左上 logo 区**，导航**分段**呈现，**点 logo 切换展开/收起**，**左下放开源许可 + 折叠按钮**，并给内容区**固定 max-width** 以稳定全局布局（用户抱怨「出来个东西就变形」）。只动 `AppLayout`（及必要的全局布局常量 / CSS）。

复用既有能力不回归：角色门控（FR-95）、页眉全局搜索（FR-94）、更新徽标与版本号取自 `/health`（FR-101）、active 段精确匹配（fix-B）。

## 2. 需求（要什么）

- 范围内（按已确认设计）：
  - **左上 logo 区**：一张内联 **logo 矢量图（SVG，紫底圆角方块 + 浅紫立方体 / 包裹线稿，制品寓意）** + 文字「JianArtifact」+ 其下小灰字版本号 `v{version}`（取自公开 `GET /health`）。**点击「logo + 文字」整体切换导航展开 / 收起**（带 `aria-label` 与键盘可达）。收起态 logo 区只留 SVG（仍可点击展开）。
  - **分段导航**（每段一个小灰字段头，收起态以细分隔线代替段头）：
    - **浏览**：仪表盘（`/`，`IconLayoutDashboard`）· 仓库（`/repositories`，`IconPackage`）· 搜索（`/search`，`IconSearch`）
    - **管理**：用户与组（`/users`，`IconUsers`）· 访问令牌（`/tokens`，`IconKey`）· 上传（`/upload`，`IconUpload`）· Nexus 迁移（`/migration`，`IconArrowsExchange`）
    - **系统 · 监控**：监控（`/monitor`，`IconChartDots`）· 审计日志（`/audit`，`IconClipboardText`）· **系统日志（`/system-logs`，`IconFileText`，新增入口）** · 防护配置（`/protection`，`IconShieldHalf`）· 设置（`/settings`，`IconSettings`）
  - **角色门控**：管理类 / 系统类入口沿用现有 `isAdmin` 门控；匿名仅见公开浏览入口（仓库 / 搜索，FR-95 不回归）。
  - **删除「使用分析」导航入口**（已并入监控，FR-99）。**新增「系统日志」入口**指向 `/system-logs`（路由 + 页由并行 FR-107 创建；路由不存在期间点击属正常，二者一起 land）。
  - **左下 footer**：开源许可入口（`IconLicense`，点击进 `/licenses`）+ 折叠 / 展开按钮。**展开态**显「许可 + 按钮」；**收起态（窄导航）隐藏许可、只留展开按钮在底**。
  - **收起态**：导航项只显图标（Tooltip + `aria-label` 可达），段间用细分隔线。
  - **全局布局稳定**：内容区给**固定 max-width**（居中 + `maxWidth`），卡片 / 新内容出现不再把布局撑变形。落在外壳内容区。
  - **保留 FR-101 更新徽标**：logo 区旁「更新: cur → latest」徽标（仅 Admin、有更新才显，点击进 `/settings`），不删。
  - **保留页眉全局搜索（FR-94）**：回车 / 防抖跳 `/search?q=`。
- 不做（范围外）：
  - 不动其他页面文件（仅 `AppLayout` + 必要全局布局常量 / CSS）。
  - 不实现 `/system-logs` 路由 / 页（属 FR-107），本 FR 只加导航入口。
  - 不翻 FR-92 状态（保持 `开发中`）。
  - 不新增前端依赖（用现有 Mantine + @tabler/icons-react）。
  - 折叠状态持久化（localStorage）非必需，不镀金。

## 3. 设计（怎么做）

- `frontend/src/components/AppLayout.tsx`：
  - **logo SVG**：内联一个小型 `BrandLogo` 组件（`viewBox` 24×24）——紫底圆角方块 + 浅紫立方体 / 包裹线稿（三条棱 + 顶面菱形，制品 / 打包寓意），用主题紫常量，避免硬编码非主题色魔法值（品牌紫常量集中一处）。
  - **logo 区点击切换**：「logo + 文字 + 版本号」包成一个 `role="button"` + `tabIndex=0` 容器，`onClick` / `Enter`/`Space` 调 `toggleNav`，`aria-label` 据展开态给「展开导航」/「收起导航」。收起态隐藏文字与版本号、只留 SVG。
  - **分段导航**：`NAV_SECTIONS`（段标题 + 段内 `NavItem[]`）单一数据源；段内复用既有 `NavItemLink`（展开 `NavLink` 图标 + label；收起 `Tooltip` + 仅图标 + `aria-label`）与 `isNavActive`（段精确匹配，原样保留）。展开态每段顶部一个小灰字段头（`Text size="xs" c="dimmed"`）；收起态段头换成细分隔线（`Divider`）。`adminOnly` / `publicVisible` 门控逻辑沿用，按段过滤、过滤后空段不渲染段头 / 分隔线。
  - **左下 footer**：底部固定一块——展开态横排「开源许可（icon + 文字，点击进 `/licenses`）」+ 右侧「收起导航」按钮；收起态只留居中的「展开导航」按钮（隐藏许可）。
  - **更新徽标**：沿用 FR-101 逻辑与 `aria-label`（「有可用更新，点击前往设置页升级」），位置移到 logo 区（展开态显），仅 Admin 且有更新才显。
  - **页眉**：保留 Burger（移动端）+ 全局搜索框 + 用户名/登出 或 登录按钮（FR-94/95 原样）。品牌从页眉移到 navbar 顶部 logo 区。
  - **内容区 max-width**：`AppShell.Main` 内包一层居中容器（`maxWidth` + `margin: 0 auto`），宽度取 `density.contentMaxWidth`（新增 token），稳定布局。
- 密度基线 `frontend/src/theme/density.ts`：**新增** `contentMaxWidth`（内容区最大宽度），**不删既有导出**（`navbarWidth` / `mainPadding` / `cardPadding` / `gridSpacing` / `inlineGap` 仍被 Dashboard / Search / Settings 页引用，保持兼容）。
- 无新 ADR：UI 重排、行为不变（导航语义、角色门控、路由、版本 / 更新来源均不变），仅外壳呈现重做。如评估需 ADR 先停下汇报。

## 4. 任务拆分

- [x] 重写本 spec + 确认 PRD FR-92 已为新设计、状态 开发中（不翻状态）
- [x] 测试先行（Vitest，覆盖新结构）：分段渲染（三段段头 + 段内项归属）；点 logo 切换展开 / 收起；收起态隐藏许可、留展开按钮在底；删「使用分析」入口；加「系统日志」入口跳 `/system-logs`；更新徽标不回归；角色门控不回归；active 段精确匹配不回归；内容区有 max-width 容器
- [x] 实现：logo SVG + logo 区点击切换 + 分段导航 + 左下 footer + 内容区 max-width + density 新增 token
- [x] 文档同步：CHANGELOG 未发布段末尾追加一行、本 spec 勾选
- [x] 验证门：`pnpm -C frontend build` / `test` / `lint` 全过，恢复 `frontend/dist/.gitkeep`

## 5. 验收标准

- 左上 logo 区显 SVG + 「JianArtifact」+ 小灰字 `v{version}`；点击「logo + 文字」整体切换展开 / 收起（测试断言两态切换 + `aria-label`）。
- 导航按三段（浏览 / 管理 / 系统 · 监控）分组：展开态各段有小灰字段头、段内项正确归属（测试断言段头存在 + 关键项可达）。
- 「使用分析」导航入口已删除（测试断言不可见）；新增「系统日志」入口可见且点击跳 `/system-logs`（测试断言跳转）。
- 左下 footer：展开态显「开源许可」+「收起导航」按钮，点击许可跳 `/licenses`；收起态隐藏许可、只留「展开导航」按钮（测试断言两态）。
- 收起态导航项仅图标 + 可访问名（`aria-label`），读屏 / 键盘可用（测试断言 `aria-label`）。
- 更新徽标不回归：Admin 且有更新才显「更新: cur → latest」、点击跳 `/settings`；非 Admin / 无更新 / 409 不显（沿用 FR-101 测试，保持绿）。
- 角色门控不回归：匿名仅见公开浏览入口；非 Admin 看不到管理 / 系统入口；Admin 全见（测试断言）。
- active 段精确匹配不回归：`/protection` 不被 `/protection-monitor` 串台；`/repositories/libs` 下「仓库」仍高亮；根 `/` 仅「仪表盘」高亮（沿用 fix-B 测试，保持绿）。
- 内容区有固定 max-width 居中容器（测试断言容器存在 / 具最大宽度样式），新内容出现不撑变形。
- `pnpm -C frontend test`（含新 AppLayout 用例）绿、`pnpm -C frontend lint` 过、`pnpm -C frontend build` 过。

## 6. 风险 / 待定

- `/system-logs` 路由本 FR 不创建，点击在 FR-107 land 前会被 `*` catch-all 重定向到 `/`（不报错）；二者一起 land 后正常。
- logo SVG 取主题紫，深 / 浅色模式下对比度依赖 Mantine 主题色，不硬编码非主题色。
- 收起态段头以分隔线代替：测试以「段头文字在收起态不可见、展开态可见」断言，不依赖像素。
