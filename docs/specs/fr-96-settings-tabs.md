# 功能规格：设置页信息密度 + 页内 tab / 锚点（重排 FR-87/FR-88 设置页）

> 状态：开发中　·　关联 PRD：FR-96　·　分支：feature/fr-96-settings-tabs

## 1. 背景与目标
属第四期（Web 控制台 UX 重构 epic，P2）。现有设置页（FR-87 起、FR-88 后已可编辑：网络代理 + 在线更新 PATCH /settings）把所有卡片自上而下纵向铺开，信息密度偏低、需大量滚动。本 FR 把设置页重排为**左侧页内 tab 导航 + 右侧高密度可编辑表单**，提升信息密度、减少滚动，并复用 FR-92 立的密度基线 token（`theme/density.ts`）。纯前端，不动后端 / 不改 GET/PATCH /settings 契约。

## 2. 需求（要什么）
- 范围内：
  - 设置页改为**左侧页内 tab / 锚点导航**（Mantine `Tabs`，竖排），分三区：
    - **网络代理**：http / https / no_proxy 紧凑表单（可编辑，沿用 FR-88 既有保存逻辑与脱敏）。
    - **在线更新**：enabled / repo / api_base_url / restart_mode / channel（FR-89）+ token 可编辑表单，以及「检查更新 / 升级」入口与二次确认流（沿用 FR-85/87 既有交互）。
    - **关于·版本**：展示当前版本与设置页生效说明（「保存后运行时即时生效、无须重启」FR-88）。
  - 切换 tab 显示对应区、其余区隐藏。
  - 密度提升：引用 `theme/density.ts`（卡片瘦身 `cardPadding`、堆叠间距 `gridSpacing`），卡片改用密度 token，紧凑表单。
  - 「保存」按钮归属在网络代理与在线更新区共用（一次 PATCH 提交全部字段，沿用既有 `handleSave`，不拆分契约）。
  - 保存动作条为 **sticky 底部固定条**（`position: sticky; bottom: 0`）：始终贴在滚动视口底部、不随内容 / 窗口缩放漂移，配顶部描边 + 背景 + 内边距与正文分隔、不遮挡内容；仅定位呈现，保存逻辑与 PATCH 契约不变。
- 不做（范围外）：
  - 不改后端、不改 GET / PATCH /api/v1/settings 契约。
  - 不改 FR-88/89 既有可编辑字段语义、token 三态、代理凭据脱敏。
  - 不碰其它页面 / shell / 导航 / 路由。
  - 不新增前端依赖（仅用现有 Mantine + @tabler/icons-react）。

## 3. 设计（怎么做）
- 仅改 `frontend/src/pages/SettingsPage.tsx`（+ `SettingsPage.test.tsx`）。
- 数据加载（`getSettings`/`fillForm`）、保存（`handleSave`/`updateSettings`）、检查（`handleCheck`）、应用（`handleApply`）逻辑**原样复用**，仅重排呈现：把现有卡片内容搬进 `Tabs.Panel`。
- 用 Mantine `<Tabs orientation="vertical">`：`Tabs.List` 为左侧导航（图标 + 文字），`Tabs.Panel` 为右侧内容区。
- 默认激活「网络代理」tab。
- 密度：卡片 / 容器 padding 与 Stack gap 引 `density.cardPadding` / `density.gridSpacing`，不再散落魔法值。
- 不涉及架构决策，无新 ADR。

## 4. 任务拆分
- [x] 复制模板 → `docs/specs/fr-96-settings-tabs.md`
- [x] PRD §4 FR-96 行状态 计划 → 开发中
- [x] 扩 `SettingsPage.test.tsx`：tab 切换显示对应区；保存仍调 PATCH；token / 凭据不回显；channel 仍可选；FR-88/89 既有用例全绿
- [x] 实现 Tabs 重排 + 引 density 基线
- [x] 文档同步：CHANGELOG 未发布段末尾追加一行
- [x] 中文 commit（feat(web): ...）

## 5. 验收标准
- 设置页渲染出三个页内 tab（网络代理 / 在线更新 / 关于·版本），点击切换显示对应区。
- 网络代理区编辑后点保存仍调 `PATCH /settings`（`updateSettings`），载荷与 FR-88 一致。
- 在线更新区 token 不回显（仅 has_token 提示）、channel（FR-89）可选并随保存提交。
- 「检查更新 / 升级」入口与二次确认流仍可用，enabled=false 时禁用。
- FR-88/89 既有用例**全部保持绿**（无回归）。
- 证据：`pnpm -C frontend test`（含设置页新 / 旧用例全绿）、`pnpm -C frontend lint`、`pnpm -C frontend build` 通过。本 FR 纯前端，无实机维度。

## 6. 风险 / 待定
- Mantine `Tabs` 切换为「挂载 / 卸载 panel」，需确认保存按钮在切换 tab 后不丢失表单态——表单态由页面级 `useState` 持有，与 panel 挂载无关，安全。
- 既有测试通过 `screen.getByText('网络代理')` 作为「加载完成」锚点；改 tab 后「网络代理」文字仍存在（作为 tab 标签），既有断言不破。
