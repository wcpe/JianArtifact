# 功能规格：设置页重设计（左侧二级导航 + 在线更新「应用更新」卡片）

> 状态：开发中　·　关联 PRD：FR-103　·　分支：feature/fr-103-settings-redesign

## 1. 背景与目标

设置页（`frontend/src/pages/SettingsPage.tsx`）上一版（被本次取代的旧方案）为「单页纵向堆叠、去 tab」——四块（网络代理 → 在线更新 → 检查与应用更新 → 关于·版本）从上到下顺序排列。该方案信息一锅端、视觉无主次，用户不满意。

本次**重设计**（推翻单页堆叠、不新增 FR 号）：把设置页改为**左侧二级导航 tab + 右侧内容区**，并把在线更新做成一张主次分明的「应用更新」卡片——通道切换 + 检查更新置于卡片右上角，版本对比 / 徽标 / 预发布提示 / 版本明细居中，立即更新 + 回滚两个动作置底。低频项仍折叠在「高级设置」内。

本 FR 属 P2 UX 重构 epic，**纯前端**：UI 重排 + 卡片化，不改后端、不改 `GET` / `PATCH /api/v1/settings`、`/update/check`、`/update/apply`、`/update/rollback` 契约。

## 2. 需求（要什么）

范围内：
- **左侧二级导航**：设置页改为**左侧子导航 tab（垂直）+ 右侧内容区**。tab 两项：**网络代理**、**在线更新**。在线更新 tab 把原「在线更新设置 + 检查与应用更新 + 关于·版本」**合并到这一个 tab**（不再分块平铺）。切 tab 时右侧内容区**固定布局不抖、底部保存条位置稳定**（保存条沿用 `data-testid="settings-save-bar"`、保持 sticky）。
- **在线更新做成「应用更新」卡片**：一张卡片，含——
  - 右上角「正式版 / 测试版」**通道切换**（segmented，对应 `channel` stable / prerelease）+「检查更新」按钮；
  - 当前版本 / 最新版本对比；
  - 「有可用更新」「预发布」**徽标**（`update_available` / `channel===prerelease`）；
  - 预发布通道时一个**提示框**（如「滚动开发预览，由 main 最新构建，可能不稳定」）；
  - 版本 **明细**与 release 发布说明（`UpdateCheck.notes`，无则优雅留空）；
  - 底部「**立即更新并重启**」+「**回滚到上一版**」两个按钮（复用 FR-85 apply + FR-104 `rollbackUpdate` / `rollback_available`，均带二次确认弹窗）。
- **高级设置折叠**：在线更新的低频项（仓库源 owner/repo / API 基址 / 重启模式 / 访问令牌）仍收进该 tab 内默认收起的「高级设置」`Collapse`。「启用在线更新」开关留在卡片可见处。
- 信息密度沿用 `theme/density.ts`；整体稳定不抖；保存条逻辑沿用现有（一次 PATCH 提交网络代理 + 在线更新）。

不做（范围外）：
- 不改后端、不改任何相关端点契约（沿用 FR-100 代理三字段、token 三态、脱敏口径，及 FR-89 channel、FR-104 回滚）。
- 不改保存 / 检查 / 应用 / 回滚的请求逻辑与错误处理。
- 不新增前端依赖。
- 不引入「关于·版本」独立 tab——当前版本展示并入在线更新卡片（版本对比处）。
- 不臆造后端未提供的「提交短 sha」字段：仅当版本串本身含 sha 才展示，否则只显版本号，不改契约、不新增 DTO 字段。

## 3. 设计（怎么做）

- **二级导航**：用 Mantine `Tabs orientation="vertical"`，`Tabs.List` 居左、`Tabs.Panel` 居右。两个 `value`：`proxy`、`update`，默认 `proxy`。所有表单态都存于 `SettingsPage` 父组件（非面板内部），切 tab 即便隐藏面板也不丢状态、PATCH body 不回归。无架构决策、不写 ADR（纯呈现重排）。
- **应用更新卡片**：单张 `Card`，头部 `Group justify="space-between"`——左标题「应用更新」，右侧 `SegmentedControl`（正式版 / 测试版，绑 `channel`）+「检查更新」`Button`。卡片体：「启用在线更新」`Switch`、版本对比行（当前 `current_version` ↔ 最新 `latest_version` + 徽标）、prerelease `Alert`、release notes、底部动作 `Group`（立即更新并重启 / 回滚到上一版）。
  - 通道切为 `prerelease` 即显「预发布」徽标 + 预发布提示框；切回 `stable` 即隐。
- **高级设置**：沿用现有 `Collapse` + 切换按钮，包裹仓库源 / API 基址 / 重启模式 / 访问令牌四项；表单态与提交逻辑不变。
- **保存条**：沿用现有 sticky 底部条（`data-testid="settings-save-bar"`），位于 `Tabs` 之外、整页底部，切 tab 不动。
- 数据加载 / 保存 / 检查 / 应用 / 回滚函数（`fillForm` / `handleSave` / `handleCheck` / `handleApply` / `handleRollback`）原样复用。

## 4. 任务拆分
- [ ] 更新 Vitest：左侧二级导航（两 tab、切 tab 右侧内容切换且保存条不动）、应用更新卡片各态（有更新 / 预发布 / 最新）、通道 segmented 切换驱动 channel 与徽标 / 提示、检查 / 立即更新 / 回滚按钮与二次确认、高级项折叠默认收起可展开、保存 PATCH body 不回归。
- [ ] 改 `SettingsPage.tsx`：单页堆叠 → 左侧二级导航 + 应用更新卡片。
- [ ] 文档同步：spec（本文，取代旧 merge spec）、CHANGELOG（改写 FR-103 段为重设计取代单页堆叠）。

## 5. 验收标准
- `pnpm -C frontend build`（tsc + vite）通过。
- `pnpm -C frontend test` 全绿，含：
  - 左侧二级导航有「网络代理」「在线更新」两 tab（`role="tab"`）；默认「网络代理」面板可见（代理三字段），切到「在线更新」面板显应用更新卡片。
  - 应用更新卡片：默认显启用开关、通道切换（正式版 / 测试版）、检查更新按钮；检查后显版本对比 + 徽标（有更新 / 已是最新）+ release notes；通道切「测试版」显「预发布」徽标 + 预发布提示框。
  - 立即更新并重启 / 回滚到上一版按钮各走二次确认弹窗并调 apply / rollback，成功进入「已触发升级」态；`rollback_available=false` 时回滚禁用。
  - 高级项（仓库源 / API 基址 / 重启模式 / 访问令牌）默认折叠不可见，点「高级设置」展开后可见。
  - 保存 PATCH body（代理三字段 / token 三态 / channel）与 FR-100/89 一致不回归；保存条 `data-testid="settings-save-bar"` 保持 sticky 且贴底。
- `pnpm -C frontend lint` 通过。

## 6. 风险 / 待定
- Mantine `Tabs` 默认保留非激活面板挂载（仅 `hidden`），故其 input 仍在 DOM 但 `role` 查询会因 `hidden` 取不到——测试「面板未激活」用 `getByText(...).not.toBeVisible()`（基于可见性），「面板切换后」用点 tab 后断言内容**可见**，避免误判。
- `Collapse` 收起时子节点仍挂载，测试「高级项默认不可见」用 `toBeVisible()`（基于可见性）而非 `toBeInTheDocument()`。
