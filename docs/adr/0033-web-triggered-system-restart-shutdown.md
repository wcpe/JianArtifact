# ADR-0033：Web 触发的系统重启 / 关闭（复用自更新重启基建）

## 状态

已接受　·　扩展 ADR-0021（自更新重启）、ADR-0032（重启机制）；安全边界沿用 ADR-0004（授权）、ADR-0015（审计）

## 背景

FR-109 要在「系统」页提供仅 Admin 的**手动重启 / 关闭**服务能力。此前「从 Web 触发进程生命周期」不在范围内（PRD 无 FR、敏感）；经确认纳入，需先定职责与安全边界，避免把高危运维操作做成无防护的裸端点。

现有基建：自更新（ADR-0021/0032）已有 `RestartHandle`（关停通知 + 待处理 `RestartRequest` + apply 单飞标志）+ `shutdown_signal`（`select!(ctrl_c, restart.notified())`）+ `handle_restart`（按 `RestartMode` 自拉起 self / 仅退出 exit）。重启 / 关闭无需另造一套停机链路。

## 决策

1. **复用而非新造停机链路**：手动重启 / 关闭经现有 `RestartHandle::request_restart` 置位请求 + 触发 graceful-shutdown，走 `main` 既有 `handle_restart`。新增两个**薄端点**（`src/api/system.rs`），不在 handler 写停机逻辑。
   - `POST /api/v1/system/restart`：`mode = RestartMode::from_config(运行时 restart_mode)`、`exe = current_exe`、`argv = 当前参数`——与自更新重启同链路，仅不换二进制。
   - `POST /api/v1/system/shutdown`：强制 `mode = RestartMode::Exit`（优雅排空后退出、**不自拉起**）。
2. **安全边界**：
   - **仅 Admin**（`require_admin`）；匿名 401 / User 403（鉴权矩阵穷举）。
   - 与自更新 apply / rollback **共用单飞互斥**（`try_begin_apply`）：同一时刻只允许一个「进程级变更」（升级 / 回滚 / 重启 / 关闭）在途，抢不到 409「更新进行中」，杜绝并发停机 / 与换二进制互踩。
   - **入审计**（`system.restart` / `system.shutdown`，ADR-0015）：高危操作留痕（actor / 时间 / 结果）。
   - **前端二次确认**：重启 / 关闭各一确认弹窗，防误触。
   - **不受 `[update] enabled` 约束**：纯本地进程操作、不出站、与是否允许联网升级无关（同 rollback）。
3. **关闭语义与运维前提**：关闭=优雅排空在途请求后进程退出（`Exit`）。**若部署配了自动重启的进程管理器（systemd `Restart=always` / docker `restart: always`），进程会被其再起**——这是预期：真正停机须经该管理器（`systemctl stop` 等）。文档写明此前提，不在二进制内对抗外部管理器。

## 理由

- **最小改动、不破不变量**：`api` 层只加薄端点编排，停机 / 重启机制零改动；复用单飞标志使「换二进制」与「纯重启 / 关闭」天然互斥，无新并发面。
- **shutdown 强制 Exit**：`Exit` 即「优雅退出、不自拉起」，正是关闭语义；restart 用配置 mode 与自更新重启保持一致（self 原地 exec / exit 交管理器）。
- **高危必设防护**：进程生命周期操作是最高危运维动作，Admin + 单飞 + 审计 + 二次确认四重门是底线；不做无防护裸端点。

## 后果

- 正面：运维可从控制台安全地重启 / 关闭实例，与自更新共享一致的停机时序与互斥保证；审计可追溯。
- 约束 / 负面：真重启 / 真关闭的进程行为无法进程内单测——端点层只验「置位正确请求 + 鉴权 + 单飞 + 审计」，真机行为列**手动验收**（沿用 ADR-0021/0032「重启依赖真机」的取舍）。关闭在自动重启管理器下会被再起（已文档化）。
- 不变：授权矩阵、审计脱敏、停机链路、`enabled` 出站门控（仅约束联网类，restart/shutdown 不联网故不受其约束）均不改。

## 备选方案

- **新造独立停机端点 / 信号机制**：与自更新两套停机链路、并发面翻倍，落选（复用 `RestartHandle`）。
- **shutdown 用配置 restart_mode（不强制 Exit）**：self 模式下「关闭」会自拉起 = 变成重启，违背关闭语义，落选（强制 Exit）。
- **不做关闭、只做重启**：用户明确要两者；关闭语义经强制 Exit + 运维前提文档化已清晰，落选「只做重启」。
