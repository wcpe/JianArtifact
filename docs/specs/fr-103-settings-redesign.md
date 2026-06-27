# 功能规格：设置页锚点单页重做（左侧 sticky 锚点导航 + 单个全局保存 + 在线更新卡片带进度条）

> 状态：开发中　·　关联 PRD：FR-103　·　分支：feature/fr-103-settings-anchor

## 1. 背景与目标

设置页（`frontend/src/pages/SettingsPage.tsx`）前两版均不满足：

- 第一版「单页纵向堆叠去 tab」——信息一锅端、无主次。
- 第二版「左侧二级 tab + 右侧内容」——遗留四个硬伤：① **两个保存按钮**（系统配置 tab 自带「保存系统配置」+ 全局「保存」）；② 二级导航**不 sticky**，长内容滚下去回不到导航；③ 三个 tab 被 Mantine `Tabs` 强制同宽高，短 tab 留大片空白；④ 没有锚点、无法快速跳节。

本次**第三次重做**（推翻第二版二级 tab，不新增 FR 号）：改为**锚点单页**——左侧 **sticky** 锚点子导航 + 右侧**单页分节**纵向排列，**只有一个**底部 sticky 全局保存按钮，在线更新做成带**应用进度条**的卡片。FR-106 的 limits/observability/vuln/auth 表单并入对应锚点节。

本 FR 属 P2 UX 重构 epic，**纯前端**：UI 重排 + 锚点滚动 + 进度条呈现，**不改后端**、不改 `GET`/`PATCH /api/v1/settings`、`/settings/dynamic`、`/update/check`、`/update/apply`、`/update/rollback` 契约。

## 2. 需求（要什么）

范围内：
- **左侧 sticky 锚点子导航**：六个锚点项——**网络代理 / 在线更新 / 限制与配额 / 可观测性 / 漏洞库 / 安全·会话**。导航整体 `position: sticky` 固定置顶（随右侧内容滚动时常驻可见）；点击某项**平滑滚动**（`scrollIntoView({ behavior: 'smooth' })`）到对应节，并**高亮当前节**——滚动时按可视区用 `IntersectionObserver` 更新高亮项。
- **右侧单页分节**：所有节从上到下**一页纵向排列**（不是 tab、**不强制等高**，短节就短、不留空白）。每节一个标题 + 表单，节带 `id` 供锚点定位。
- **单个全局保存**：**只有一个**底部 sticky「保存」按钮（沿用 `data-testid="settings-save-bar"`、保持 sticky 贴底）。**去掉系统配置节自带的「保存系统配置」按钮**——点全局保存时**统一**提交两次后端写入：① `PATCH /api/v1/settings`（网络代理 + 在线更新，即时生效）；② 若动态配置已加载则 `PATCH /api/v1/settings/dynamic`（limits/observability/vuln/auth，重启生效）。各节内用小字标注「即时生效」/「保存后重启生效」，不再有第二个按钮。
- **在线更新做成卡片**：一张卡片，含——
  - 右上角「正式版 / 测试版」**通道切换**（`SegmentedControl` 绑 `channel`）+「检查更新」按钮；
  - 「启用在线更新」开关（卡内可见处）；
  - 当前 ↔ 最新**版本对比** + 徽标（有可用更新 / 已是最新 / 预发布）；
  - 预发布通道时一个**提示框**（滚动开发预览、可能不稳定）；
  - release 发布说明（`UpdateCheck.notes`，无则优雅留空）；
  - **应用进度条**：点「立即更新并重启」确认后，在 apply 请求在途期间显示进度条（如「下载中 62% 7.8/12.6 MB」）。后端 apply 为单次阻塞请求、无字节级回传（契约不改），故进度为**客户端模拟推进**——按预估总量随时间平滑爬升、**封顶不到 100%**，待请求 resolve（服务进入重启）才置满并切「已触发升级」态；失败则停进度并显错误。
  - 底部「**立即更新并重启**」+「**回滚到上一版**」两个按钮（复用 FR-85 apply + FR-104 rollback / `rollback_available`，均带二次确认弹窗）。
  - 低频高级项（仓库源 owner/repo / API 基址 / 重启模式 / 访问令牌）仍收进卡内默认收起的「高级设置」`Collapse`。
- 信息密度沿用 `theme/density.ts`；布局稳定不抖。

不做（范围外）：
- 不改后端、不改任何相关端点契约（沿用 FR-100 代理三字段、token 三态、脱敏口径，FR-89 channel，FR-104 回滚，FR-106 动态配置）。
- 不新增**真实**字节级进度端点 / SSE（保持单次 POST 契约）——进度条为客户端模拟。
- 不新增前端依赖。

## 3. 设计（怎么做）

- **布局骨架**：`SettingsPage` 内一个两列 `Flex`——左列 sticky 锚点导航（`<nav>`，`position: sticky; top: 0`），右列 `Stack` 纵向分节。各节 `<Box component="section" id="...">`，标题 `Title order={4}`。
- **锚点导航**：节定义集中在一个常量数组 `SECTIONS = [{ id, label }, ...]`（单一真源，导航与内容共用、不复制散落）。导航项点击调 `scrollToSection(id)`（`document.getElementById(id)?.scrollIntoView({ behavior: 'smooth', block: 'start' })`）。当前高亮 `activeId` 由 `IntersectionObserver`（观察各节、取最靠上的可视节）维护，存于 state。导航项 active 态用 Mantine `NavLink` 高亮。
- **单个保存**：父组件持所有表单态（含动态配置态）。一个 `handleSaveAll()`——顺序调 `updateSettings` + （`dynamic` 非空时）`updateDynamicConfig`，任一失败聚合到 `saveError`、成功显「已保存」（措辞兼顾即时/重启生效）。保存条沿用现有 sticky 底部条结构与 `data-testid`。
- **应用更新卡片 + 进度条**：进度态用 `applyProgress: number | null`（null=未在应用）。`handleApply` 开始前置 `applyProgress=0` 并启 `setInterval` 平滑加（上限如 95%），await `applyUpdate()`；成功清定时器、`restarting=true`（保留现有「已触发升级」态），失败清定时器、`applyProgress=null` + 显错误。进度文案据 `UpdateCheck.asset_name` 估算总量展示（无则只显百分比）。`Progress` + 文案在确认升级后、`restarting` 之前显示。
- **FR-106 表单并入**：原「系统配置」tab 的 limits / audit / usage / metrics / metrics_timeseries / vuln / auth 各 input **整段搬入**对应锚点节：limits → 「限制与配额」；audit/usage/metrics/metrics_timeseries → 「可观测性」；vuln → 「漏洞库」；auth → 「安全·会话」。`patchDynamic` 与字段绑定逻辑原样复用，仅去掉该 tab 自带的保存按钮与独立成功提示（并入全局保存）。
- **数据加载 / 检查 / 应用 / 回滚**（`fillForm` / `handleCheck` / `handleApply` / `handleRollback` / `buildProxyPatch`）原样复用；`handleSave` 扩展为 `handleSaveAll` 合并两次 PATCH。无架构决策、不写 ADR（纯前端呈现重排 + 客户端模拟进度）。

## 4. 任务拆分
- [ ] 更新 Vitest（`SettingsPage.test.tsx`）：锚点导航六项 + 点击调 `scrollIntoView`（平滑滚动）；**只有一个保存按钮**（断言「保存系统配置」按钮不存在）；单页分节各节标题在场且可见（非 tab 隐藏）；在线更新卡片各态（有更新 / 预发布 / 最新）+ 通道切换驱动 channel 与徽标 / 提示 + 高级项默认折叠可展开；点立即更新确认后**显应用进度条**；各节表单经全局保存调对应 PATCH（settings + dynamic）；契约不回归（代理三字段 / token 三态 / channel / 动态配置字段）。
- [ ] 改 `SettingsPage.tsx`：二级 tab → 锚点单页 + 单保存 + 应用进度条；FR-106 表单并入锚点节。
- [ ] 文档同步：spec（本文，取代二级 tab 方案）、CHANGELOG 末尾追加一行（改写 FR-103 段为锚点单页重做取代二级 tab）。

## 5. 验收标准
- `pnpm -C frontend build`（tsc + vite）通过。
- `pnpm -C frontend test` 全绿，含：
  - 左侧锚点导航有六项（网络代理 / 在线更新 / 限制与配额 / 可观测性 / 漏洞库 / 安全·会话）；点某项调 `scrollIntoView`（平滑滚动到对应节）。
  - **只有一个保存按钮**：页面存在 `data-testid="settings-save-bar"` 内的「保存」，且**不存在**「保存系统配置」按钮。
  - 单页分节：各节标题（如「网络代理」「限制与配额」「漏洞库」「安全 / 会话」）默认即可见（非 tab 隐藏），短节不留空白由布局保证（不强制等高）。
  - 在线更新卡片：默认显启用开关、通道切换（正式版 / 测试版）、检查更新按钮；检查后显版本对比 + 徽标（有更新 / 已是最新）+ release notes；通道切「测试版」显「预发布」徽标 + 预发布提示框；高级项（仓库源 / API 基址 / 重启模式 / 访问令牌）默认折叠不可见、点「高级设置」展开后可见。
  - 立即更新并重启：点击走二次确认 → 确认后**显示应用进度条**（进度文案 / `Progress`），apply 成功进入「已触发升级」态；422 等失败显错误且不进入重启态、进度条撤下。回滚走二次确认调 rollback，`rollback_available=false` 时回滚禁用。
  - 全局保存：点「保存」一次性提交 `updateSettings`（代理三字段 / token 三态 / channel 与 FR-100/89 一致不回归）+ `updateDynamicConfig`（auth.session_ttl_secs 等动态字段与 FR-106 一致不回归）。
- `pnpm -C frontend lint` 通过。

## 6. 风险 / 待定
- jsdom 无真实布局，`scrollIntoView` 与 `IntersectionObserver` 需打桩：`scrollIntoView` 已在 `src/test/setup.ts` 全局桩；`IntersectionObserver` 在 setup 内补空实现桩（observe/disconnect 空），高亮逻辑不在单测断言可视区计算、只断言导航存在与点击滚动。
- 应用进度条为**客户端模拟**（无真实字节回传，守单次 POST 契约）：测试只断言「确认升级后进度条出现」「成功后进入已触发升级态」，不断言具体百分比数值（计时器驱动、避免脆弱）。
- 单个保存触发两次 PATCH：若动态配置未加载（`dynamic` 为 null）则只发 `updateSettings`，不报错。
