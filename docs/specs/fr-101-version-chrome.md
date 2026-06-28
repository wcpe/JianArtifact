# 功能规格：控制台版本展示（FR-101）

> 状态：开发中　·　关联 PRD：FR-101　·　分支：feature/fr-101-version-chrome

## 1. 背景与目标
UX 重构 epic 收尾项：把版本信息呈现到控制台外壳（`AppLayout`）。运维 / 用户需要一眼看到当前运行版本号与开源许可入口；管理员还需在有可用更新时被显眼提示。属第四期（P2）UX 重构，**纯前端**，复用既有公开 / Admin 端点，不改后端契约。

## 2. 需求（要什么）
- **Logo 旁更新徽标**：页眉 Logo 旁，**仅 Admin 且确有可更新时**显示徽标 `更新: {current} → {latest}`，点击跳 `/settings`（设置页在线更新区）。判定：Admin 登录后调一次 `GET /api/v1/update/check`，仅当 **HTTP 200 且 `update_available===true`** 才显；未启用在线更新（409）/ 无更新 / 非 Admin / 请求失败 → 不显徽标（静默，不报错、不阻塞渲染）。**只在挂载时查一次并缓存**，不每次渲染重查（避免 GitHub 限流）；查询走后台、不阻塞页面渲染。
- **版本号（logo 区下方）+ 开源许可入口（左下 footer）**：随后到的 FR-92 外壳重做，两者按位置拆开呈现——当前版本号 `v{version}`（取自公开 `GET /health` 的 `version`，**所有用户可见含匿名**）以小灰字置于**左上 logo 区下方**；「开源许可」链接（带 icon，点击进 `/licenses`）置于**左下 footer**。两者均**仅展开态显示**，**窄导航（收缩态）时隐藏**（不再用 icon + Tooltip 占位）。
- 可见性：徽标仅 Admin；版本号（logo 区下方）与许可入口（左下 footer）展开态对所有人（含匿名）可见，收缩态对所有人隐藏。
- 范围内：仅 `AppLayout` 外壳呈现 + api 层新增 `getHealth` 封装（包公开 `/health`）。
- 不做（范围外）：不改后端、不新增后端端点；不在徽标里做轮询自动刷新；不改 `/settings`、`/licenses` 页内容。

## 3. 设计（怎么做）
- api 层：新增 `HealthInfo` 类型（`{status, version, port}`）与 `getHealth()`（`GET /health`，注意该端点在 `/api/v1` 之外、为根路径，单独 `fetch` 封装，失败抛错由调用方静默处理）。
- `AppLayout`：
  - 挂载时 `useEffect` 调 `getHealth()` 取 `version`，存入 state；失败则版本号区不渲染（静默）。
  - 仅当 `isAdmin` 时，挂载时调一次 `checkUpdate()`，仅 200 且 `update_available` 为真才置徽标 state；任何错误（含 409）静默吞掉、不置徽标。两次查询都不阻塞首屏渲染。
  - 页眉 Logo 旁条件渲染 `Badge`（点击 `navigate('/settings')`）。
  - 版本号 `v{version}` 小灰字置于**左上 logo 区下方**（**仅展开态渲染**，收缩态不渲染；对所有人含匿名可见）；「开源许可」链接（带 icon，点击 `navigate('/licenses')`）置于**左下 footer**（**仅展开态渲染**，收缩态不渲染）。
- 无架构决策，不写 ADR。

## 4. 任务拆分
- [x] api：`HealthInfo` 类型 + `getHealth()` 封装公开 `/health`
- [x] 测试先行：扩 `AppLayout.test.tsx`（徽标显隐矩阵、底部版本号、许可按钮跳转、折叠态 Tooltip）
- [x] 实现：`AppLayout` 徽标 + 底部版本号 + 许可入口
- [x] 文档同步：PRD 状态行、ARCHITECTURE（AppLayout 结构）、CHANGELOG 末尾追加一行

## 5. 验收标准
- `pnpm -C frontend test` 全绿，新增 / 改 `AppLayout` 测试覆盖：Admin + 有更新显徽标 / 非 Admin / 无更新 / 未启用（409）不显徽标、展开态显小字版本号（logo 区下方）+ 许可入口（左下 footer）（含匿名）、收缩态版本号与许可入口均隐藏、许可点击跳 `/licenses`。
- `pnpm -C frontend build` 通过、`pnpm -C frontend lint` 通过。
- FR-92 既有测试不回归（折叠 / 角色门控 / 段精确高亮 / 全局搜索 / 匿名 shell）。

## 6. 风险 / 待定
- `/health` 在 `/api/v1` 之外，api 层既有 `request` 恒加 `API_BASE` 前缀，需单独 `fetch`。
- 徽标 / 版本号查询失败必须静默，绝不阻塞或报错打断外壳渲染。
