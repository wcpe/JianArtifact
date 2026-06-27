# 功能规格：设置页重构（单页堆叠 + 在线更新高级项折叠 + 检查更新展示 release 信息）

> 状态：开发中　·　关联 PRD：FR-103　·　分支：feature/fr-103-settings-merge

## 1. 背景与目标

设置页（`frontend/src/pages/SettingsPage.tsx`）现为「左侧页内 tab（网络代理 / 在线更新 / 关于·版本）+ 右侧表单 + sticky 底部保存条」（FR-96/88/89/100）。切 tab 时各 tab 内卡片高度不一，导致内容区忽大忽小、底部保存条与滚动条跳位。在线更新区一次性铺开 6 项配置（含低频的仓库源 / API 基址 / 重启模式 / 访问令牌），首屏噪声大。检查更新仅展示版本对比，未利用后端已返回的 release 发布说明。

本 FR 属 P2 UX 重构 epic，**纯前端**：UI 重排 + 折叠 + 既有字段展示，不改后端、不改 `GET` / `PATCH /api/v1/settings` 契约。

## 2. 需求（要什么）

范围内：
- **合并为单页纵向堆叠、去 tab**：三块（网络代理 → 在线更新 → 关于·版本）从左侧页内 tab 改为单页从上到下顺序排列，去掉 tab 切换，消除切 tab 内容忽大忽小、保存按钮跳位。底部保存条保持 sticky 固定（`data-testid="settings-save-bar"` 不变）。
- **在线更新高级项折叠**：在线更新区默认只显「更新通道」+「检查与应用更新」（检查更新 / 升级）。把 **仓库源(owner/repo) / API 基址 / 重启模式 / 访问令牌** 四项收进「高级设置」可折叠区（默认收起，点开才显示编辑）。「启用在线更新」开关保留在默认可见处。
- **检查更新展示 release 信息**：点「检查更新」后，除当前 / 最新版本与有无更新外，展示拉取到的 release 发布说明（`UpdateCheck.notes`，即 release body）；无说明时优雅留空（不渲染空块）。
- 信息密度沿用 `theme/density.ts`；单页布局稳定不抖。

不做（范围外）：
- 不改后端、不改 `GET` / `PATCH /api/v1/settings` 契约（沿用 FR-100 代理三字段、token 三态、脱敏口径）。
- 不改保存 / 检查 / 应用更新逻辑与错误处理。
- 不新增前端依赖。

## 3. 设计（怎么做）

- 去掉 `Tabs` / `Tabs.List` / `Tabs.Panel`，三块改为同一 `Stack` 内的顺序卡片（网络代理 → 在线更新 → 关于·版本）。无架构决策、不写 ADR（纯呈现重排）。
- 在线更新区高级项用 Mantine `Collapse` + 一个「高级设置」切换按钮（`useDisclosure` 控制开合，默认收起）包裹仓库源 / API 基址 / 重启模式 / 访问令牌四项；表单态与提交逻辑不变（`Collapse` 不卸载子节点，留空保存 / token 三态语义保持）。
- 检查更新结果卡片复用既有 `check.notes` 展示（FR-89 已有 `whiteSpace: pre-wrap`），保持「有 notes 才渲染」。

## 4. 任务拆分
- [ ] 更新 / 新增 Vitest（单页无 tab、三块同屏、在线更新默认仅显通道 + 检查更新、高级项默认收起且可展开、检查后显 release notes、保存 PATCH body 不回归）
- [ ] 改 `SettingsPage.tsx`：去 tab → 单页堆叠 + 高级设置折叠
- [ ] 文档同步：PRD 状态、CHANGELOG（末尾追加一行）

## 5. 验收标准
- `pnpm -C frontend build`（tsc + vite）通过。
- `pnpm -C frontend test` 全绿，含：单页渲染三块且无 `tab` 角色；在线更新默认可见「更新通道」「检查更新」、不可见「仓库源」「访问令牌」等高级项；点「高级设置」展开后高级项可见；检查更新后展示 `notes`；保存 PATCH body（代理三字段 / token 三态 / channel）与 FR-100/89 一致不回归。
- `pnpm -C frontend lint` 通过。
- 底部保存条 `data-testid="settings-save-bar"` 保持 sticky。

## 6. 风险 / 待定
- `Collapse` 默认收起时子节点仍挂载（Mantine `Collapse` 不卸载内容），故 `getByLabelText` 在收起态仍能取到高级项 input——测试「默认不可见」需用基于可见性的查询（如检查 `Collapse` 的 `in` 态 / 包裹容器），或断言切换按钮文案 + 展开后内容出现，避免误判。
