# 功能规格：设置页重新设计（顶部 Tab 分页方案 A）

> 状态：开发中　·　关联 PRD：FR-129（增强 FR-87/96/110）　·　分支：master（单 FR，不开 worktree）

## 1. 背景与目标
设置页（`frontend/src/pages/SettingsPage.tsx`）当前是「左侧 sticky 锚点导航 + 右侧长滚动堆叠卡片」（FR-103 / FR-110 落地的形态），用户实测暴露两个问题：

1. **锚点高亮 / hover 错位**：从下往上点锚点时，scroll-spy（`IntersectionObserver` + `scrollIntoView` + `scrollMarginTop`）会把高亮停在上一节，hover 显示的也对不上。根因是「滚动联动的可视区高亮」本身。
2. **两个保存按钮并存**：防护节（`ProtectionConfigSection`）自带独立 PATCH `/protection/config` 保存（即时生效），与底部全局保存并存，用户看到两个「保存」语义不一的按钮。

属 P2 UX 重构。目标：把 6 节改为**顶部 Tab 分页**（切换不滚动 → 根因消除错位），并**统一为单一保存按钮**。

## 2. 需求（要什么）
- 范围内：
  - 6 节信息架构由「锚点长滚动」重排为 Mantine `<Tabs>` 顶部水平分页：网络代理（`proxy`）/ 限制与配额（`limits`）/ 可观测性（`observability`）/ 漏洞库（`vuln`）/ 安全·会话（`auth`）/ 防护配置（`protection`）。每节一个 `<Tabs.Panel>`，切换不滚动。
  - 移除 scroll-spy 全套：`SECTIONS`、`activeSection`、`scrollToSection`、`IntersectionObserver` useEffect、`SECTION_SCROLL_STYLE`/`scrollMarginTop`、左侧 `NavLink` 导航。
  - 统一单一保存：防护节配置 state + 保存逻辑上提到 SettingsPage，由全局 `handleSaveAll` 一并调用 PATCH `/protection/config`；移除防护节独立保存按钮。
  - 保留 FR-128 代理测试（`testUrl`/`handleProxyTest`/`data-testid="proxy-test-*"`）于网络代理 Tab，data-testid 不变。
- 不做（范围外）：
  - 不改后端 / 任何 API 契约（PATCH `/settings`、`/settings/dynamic`、`/protection/config` 路径、载荷、语义均不变）。
  - 不改各节字段内容、校验、即时 / 重启生效语义。
  - 不引入新第三方依赖（用 Mantine 既有 `Tabs`）。

## 3. 设计（怎么做）
仅前端、单组件层信息架构重排 + 保存合一，无架构决策、无新 ADR。

- **Tabs 重排**：SettingsPage 用 `<Tabs value defaultValue="proxy">` 包裹 `<Tabs.List>`（6 个 `<Tabs.Tab>`，标题取各节既有 `xxx.title`）+ 6 个 `<Tabs.Panel>`。Tab 受控 state `activeTab` 仅驱动分页显示，**不与滚动联动**。
- **防护节状态上提**：把 `ProtectionConfigSection` 的 `config`/`allowText`/`denyText`/`loading`/`error` state 与 GET `/protection/config` 加载 effect 上提到 SettingsPage；`ProtectionConfigSection` 改为受控展示组件（props 传入 config / 文本 / patch / 各态），内部不再渲染独立保存按钮、不再自带 save。SettingsPage 的 `handleSaveAll` 在 settings + dynamic 之后追加 PATCH `/protection/config`（保持 IP 名单文本归并语义）。
- **保存契约**：`handleSaveAll` 顺序提交 settings → dynamic（已加载才发）→ protection（已加载才发），任一失败聚合到 `saveError`、全成功显「已保存」。
- 注意项目记忆坑：Mantine `px`/`py` props 覆盖同元素 inline `style.padding`；Tab 布局不叠 px，间距走 Mantine 间距 props / style 一致。

## 4. 任务拆分
- [ ] 测试先行：改 `SettingsPage.test.tsx` 按 Tab 交互断言（Tab 列表 6 项、切 Tab 显示对应节、单一保存、代理测试在代理 Tab、防护节无独立保存、防护配置随全局保存一并 PATCH）；删除 scroll-spy / 锚点 / 防护独立保存相关旧断言。
- [ ] 实现：SettingsPage 改 Tabs + 防护状态上提 + 保存合一 + 移除 scroll-spy；ProtectionConfigSection 改受控、去独立保存。
- [ ] i18n：复用各节 `xxx.title` 作 Tab 标题；如需「内容加载中」等无新增文案则不动 locale。
- [ ] 文档同步：PRD FR-129 状态（→ 开发中，交付时由发版标）、本 spec、CHANGELOG 未发布段追加一行；E2E 设置页若涉及同步或标待真机。
- [ ] 验证门：`pnpm -C frontend run lint` + `run test` + `run build`（tsc）全绿；build 后 `git checkout -- frontend/dist/.gitkeep`。

## 5. 验收标准
- Tab 列表含 6 项，点击任一 Tab → 仅该节字段可见、其余节隐藏（与旧「各节默认全可见」相反）。
- 整页仅一个保存按钮（保存条内「保存」），不存在防护节「保存并即时生效」按钮。
- 点全局保存：settings / dynamic / protection 三处契约不变地提交（穷举 dynamic 未加载只发 settings+protection、protection 改动随全局保存 PATCH）。
- 代理测试按钮 / 输入框在网络代理 Tab，`data-testid` 不变、行为不回归。
- `pnpm -C frontend run lint` + `run test` + `run build` 全绿（tsc 通过）。
- **待真机**：浏览器 E2E（Playwright）验证 Tab 切换与单一保存的真实交互、从下往上切 Tab 不再高亮 / hover 错位——jsdom 无真实布局 / 动画，标待真机由用户确认。

## 6. 风险 / 待定
- Tab 分页后「各节默认全可见」的旧测试断言必然失效，需整体改写交互断言，不得删 / 跳过。
- 防护节状态上提需保持 IP 名单文本（allow/deny）与 config 的归并语义不回归。
