# 功能规格：更新回滚（FR-104）

> 状态：开发中　·　关联 PRD：FR-104（增强 FR-85）　·　分支：feature/fr-104-update-rollback

## 1. 背景与目标

在线更新（FR-85，ADR-0021）已支持管理员一键升级：下载 → 校验 sha256 → 原子替换二进制 → 重启。但升级后若新版本有问题，运维只能手工把旧二进制换回去——而现有备份机制（Unix `{exe}.bak`、Windows `{exe}.old`）并非为「持久回滚源」设计：`.old` 会被启动期 `cleanup_stale_old` 清理，`.bak` 也未被约定为稳定回滚契约。

目标（P2，增强 FR-85）：升级时把当前二进制持久备份为**单个**稳定回滚源，并提供 `POST /api/v1/update/rollback`（仅 Admin）一键还原上一版 + 重启；无备份时返明确错误。设置页加「回滚到上一版本」按钮（二次确认），无备份时禁用。

## 2. 需求（要什么）

- **升级时备份（单备份）**：`apply_update` 在原子替换前，把当前运行的二进制持久备份为跨平台一致的回滚源 `{exe}.rollback.bak`。每次升级覆盖该单一备份（只留上一版，不做多版本历史）。该路径**不被启动清理**（区别于 Windows 临时 `.old`）。
- **回滚端点** `POST /api/v1/update/rollback`（仅 Admin）：校验回滚备份存在 → 原子用备份还原当前二进制 → 按 `restart_mode` 重启（复用现有重启链路）。无备份返 `409`「无可回滚的备份版本」。回滚原子、失败回退不留半截。
- **回滚可用性暴露**：设置聚合视图 `UpdateView` 增 `rollback_available: bool`，供前端启用 / 禁用按钮。
- **前端**：「检查与应用更新」区加「回滚到上一版本」按钮（二次确认 Modal，仿升级流），调 `POST /update/rollback`；无备份时禁用。
- 范围内：单备份语义、跨平台持久备份、回滚端点 + 鉴权、前端按钮 + 二次确认、纯函数路径推导 / 回滚规划。
- 不做（范围外）：多版本回滚历史、回滚到任意指定版本、签名校验、自动回滚（健康探测失败自动回退）、备份压缩 / 远端备份。

## 3. 设计（怎么做）

涉及架构决策（自更新回滚增强、单备份语义、跨平台持久备份），另写 **ADR-0026**（增强 ADR-0021），此处不重复决策正文。

模块改动（守分层 `api → update → config`，update 不碰 meta/DB）：

- `src/update/mod.rs`
  - 新增常量回滚备份后缀 `.rollback.bak`，纯函数 `rollback_backup_path(current_exe) -> PathBuf`（仿 `sibling_with_suffix`，可单测）。
  - `apply_update` 替换成功前，把当前 exe 复制为 `rollback_backup_path`（落盘失败即报错回滚、不触碰二进制）。此持久备份独立于 Unix `.bak` / Windows `.old`（后者维持现有「单步回退 / 启动清理」语义不变）。
  - 新增纯函数 `plan_rollback(current_exe) -> RollbackPlan`（推导备份源、暂存 `.new`、Windows `.old`），与 `plan_replace` 同构、跨平台可测。
  - 新增 `rollback(current_exe) -> Result<RollbackOutcome, UpdateError>`：校验备份存在（不存在报新错误 `NoBackup`）→ 把备份 copy 到暂存 `.new` → 复用 `execute_replace` 原子换回 → 返回落地 exe 路径。替换执行走阻塞线程池。
  - 新增错误 `UpdateError::NoBackup`，映射 `409`。
  - `rollback_available(current_exe) -> bool`：备份文件是否存在（GET settings 用）。
  - `cleanup_stale_old` **不动** `.rollback.bak`（持久保留）。
- `src/api/update.rs`
  - `From<UpdateError>`：`NoBackup → 409`「无可回滚的备份版本」。
  - 新增 handler `rollback_update`（仅 Admin）：`require_admin` → 抢 apply 单飞 guard（与 apply 共用，避免与升级互踩）→ 定位 current_exe → `update::rollback` → 置位重启请求 → 返回 `{status, restored: bool}`。无备份 → 409。
- `src/api/mod.rs`：注册 `POST /update/rollback`。
- `src/api/settings.rs`：`UpdateView` 增 `rollback_available`，`current_view` 据 `update::rollback_available(current_exe)` 填充（current_exe 取不到时降级 false）。
- 前端：`types.ts` `UpdateView` 加 `rollback_available`；新增 `RollbackResponse`；`endpoints.ts` 加 `rollbackUpdate()`；`SettingsPage.tsx` 加回滚按钮 + 二次确认 Modal + 处理函数。

## 4. 任务拆分

- [ ] 后端测试先行：`rollback_backup_path` / `plan_rollback` 路径推导；`apply_update` 留持久备份；`rollback` 有备份还原成功、无备份 `NoBackup`；端点 Admin-only（401/403）、无备份 409。
- [ ] 实现 `update` 模块回滚逻辑 + 错误 + 路径纯函数。
- [ ] 实现 `api::update` rollback handler + 路由 + 错误映射 + settings `rollback_available`。
- [ ] 前端 types/endpoints + 设置页按钮 + 二次确认 + 无备份禁用；前端测试。
- [ ] 文档同步：PRD 状态（FR-104 → 开发中）、ADR-0026 + README、API.md rollback 端点、ARCHITECTURE（如涉及）、CHANGELOG 末尾追加一行。

## 5. 验收标准

- `cargo fmt --check` + `clippy -D warnings`（零警告）+ `cargo test` 全绿；新增回滚单测覆盖：备份路径推导、有 / 无备份回滚规划与执行、端点鉴权 Admin-only、无备份返 409。
- 前端 `pnpm build` + `pnpm test`（回滚按钮 + 二次确认 + 无备份禁用 / 错误）+ `lint` 全绿；恢复 `frontend/dist/.gitkeep`。
- **真机维度（需用户确认）**：真实「升级 → 回滚 → 重启 → 版本回到上一版」端到端只能真机跑（尤其 Windows 运行中 `.exe` 改名 + 重启端口序列），本地以临时目录 + fake 覆盖逻辑覆盖路径正确性；端到端真机回滚标「待验」。

## 6. 风险 / 待定

- 自替换运行中二进制 + 自重启的端到端正确性依赖真机（与 ADR-0021 同源约束），单测只覆盖纯逻辑与替换 / 回滚规划。
- 回滚与升级共用 apply 单飞 guard：同一时刻只允许一个二进制变更在途，避免互踩 `.new` / `.old` / 备份中间态。
