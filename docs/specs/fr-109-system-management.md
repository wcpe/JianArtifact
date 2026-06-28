# 功能规格：系统管理页 + 手动重启 / 关闭

> 状态：开发中　·　关联 PRD：FR-109　·　分支：feature/fr-109-system-management

## 1. 背景与目标

P2。当前在线更新散在「设置」页一节，且无从 Web 手动重启 / 关闭服务的能力。新建「系统」页集中系统级运维操作（按 tab：在线更新、重启、关闭），把在线更新从设置页迁入；新增仅 Admin 的手动重启 / 关闭端点。让「配置」（设置页）与「操作」（系统页）信息架构分明。

「从 Web 触发进程重启 / 关闭」原不在范围内（PRD 无 FR、动进程生命周期、敏感），经用户确认纳入 → 先写 ADR 圈职责与安全边界（见 ADR-0033）。

## 2. 需求（要什么）

- **范围内**：
  - 新建「系统」页（仅 Admin），tab：① 在线更新（FR-85/87 的检查 / 应用 / 回滚 + 其配置，自设置页整体迁入）② 重启 ③ 关闭。
  - 后端新增仅 Admin 端点：`POST /api/v1/system/restart`、`POST /api/v1/system/shutdown`。
  - 重启 / 关闭复用 `RestartHandle` + graceful-shutdown；与自更新 apply / rollback **共用单飞互斥**；操作入**审计**。
  - 重启：按运行时 `restart_mode`（self 原地 exec 拉起 / exit 交进程管理器）——与自更新重启同链路，仅不换二进制。
  - 关闭：强制 `RestartMode::Exit`（优雅排空后退出、不自拉起）。
  - 前端二次确认（重启 / 关闭各一个确认弹窗）。
  - 设置页移除「在线更新」节；导航新增「系统」入口。
- **不做（范围外）**：定时 / 计划重启；关闭后的远程唤醒（关闭即停，靠外部进程管理器再起）；多实例编排；除 restart/shutdown 外的其他系统命令。

## 3. 设计（怎么做）

- **后端**（薄 handler，复用现有重启基建，新文件 `src/api/system.rs`）：
  - `system_restart`：`require_admin` → `try_begin_apply` 抢单飞（抢不到 409「更新进行中」）→ `request_restart({mode: from_config(restart_mode), exe: current_exe, argv})` → `200 {status}`。
  - `system_shutdown`：`require_admin` → `try_begin_apply` → `request_restart({mode: Exit, exe: current_exe, argv})` → `200 {status}`。
  - **不受 `[update] enabled` 约束**（纯本地进程操作，与是否允许联网升级无关，同 rollback）。
  - 路由挂 `/system/restart`、`/system/shutdown`（`src/api/mod.rs`）。
  - 审计分类（`src/api/audit.rs`）：`["system","restart"]→system.restart`、`["system","shutdown"]→system.shutdown`。
  - 关闭语义与运维前提见 ADR-0033（关闭=优雅退出；若配 systemd/docker 自动重启，进程会被其再起——真正停机须经进程管理器；这是预期、文档写明）。
- **前端**：
  - 新 `pages/SystemPage.tsx`（Mantine Tabs）；在线更新整块从 `SettingsPage.tsx` 迁入其「在线更新」tab；「重启」「关闭」tab 各一操作卡 + 二次确认 `Modal`。
  - `lib/api.ts`：`postSystemRestart()`、`postSystemShutdown()`。
  - 导航（`AppLayout.tsx`）新增「系统」项（系统·监控段，仅 Admin）；设置页锚点移除「在线更新」。
  - 路由 `/system`（`App.tsx`，Admin 守卫）。

## 4. 任务拆分

- [ ] ADR-0033：系统启停由 Web 层负责的职责 + 安全边界
- [ ] 后端：`src/api/system.rs`（restart/shutdown handler + 鉴权矩阵 / 单飞 / 审计测试）
- [ ] 后端：路由注册 + audit classify + 其分类测试
- [ ] 前端：SystemPage + 迁入在线更新 + 重启/关闭操作卡与二次确认
- [ ] 前端：API client + 导航 + 路由 + 设置页移除在线更新节
- [ ] 文档同步：PRD 状态、ARCHITECTURE、API、CHANGELOG

## 5. 验收标准

- **鉴权矩阵**（高风险，自动化）：`/system/restart`、`/system/shutdown` 匿名 401、User 403、Admin 放行（200 并置位重启请求）。
- **单飞互斥**（自动化）：与 apply / rollback 共用标志——占用时再触发 restart / shutdown 返 409「更新进行中」且不置位重启请求；释放后可再触发。
- **模式正确**（自动化）：restart 置位的 `RestartRequest.mode` = 运行时 restart_mode 解析值；shutdown 强制 `Exit`。
- **审计**（自动化）：classify_event 对两路径产出 `system.restart` / `system.shutdown`。
- **IA**：设置页不再含在线更新节；系统页含在线更新 / 重启 / 关闭三 tab；导航有「系统」入口、不串台。
- **真机（需用户确认）**：点「重启」服务真重启并恢复服务；点「关闭」服务真停（无自动重启管理器时不再起）。单元 / 构建绿不替代此项。

## 6. 风险 / 待定

- 关闭在配了自动重启进程管理器时会被再起——预期行为、ADR + 文档写明，非缺陷。
- 重启 / 关闭触发真正进程行为无法进程内单测；端点层只验「置位正确的请求 + 鉴权 + 单飞 + 审计」，真重启 / 真关闭列手动真机验收。
