# 功能规格：Nexus 迁移任务控制（取消 + 暂停 / 继续）

> 状态：开发中　·　关联 PRD：FR-91（增强 FR-83）　·　关联 ADR：ADR-0019（扩展 ADR-0006）　·　分支：feature/fr-91-migrate-control

## 1. 背景与目标

FR-83（ADR-0019）已把在线拉取迁移改为**进程内异步任务**：触发即返回 `job_id`、后台 `tokio` 任务跑、前端轮询进度。但任务一旦发起便无法干预——超大仓库选错、源系统压力过大、需要临时让路时，运维只能干等任务跑完或重启进程（重启会丢全部任务）。本功能给在途任务加**生命周期控制**：**取消**（停止后续 asset 搬运、置已取消）、**暂停 / 继续**（暂停后后台循环挂起、继续后恢复）。属 P2，纯增强、不改 FR-83 的「进程内、不落库、靠幂等重跑恢复」取舍。

## 2. 需求（要什么）

- 新增 `POST /api/v1/migrate/jobs/{id}/cancel`（仅 Admin）：请求取消在途任务——后台循环在下一个 asset 边界停止后续搬运，任务置 `cancelled`（**不算失败**，已搬运的制品保留）。
- 新增 `POST /api/v1/migrate/jobs/{id}/pause`（仅 Admin）：请求暂停在途任务——后台循环在下一个 asset 边界挂起、不再推进，进度 `paused` 置真。
- 新增 `POST /api/v1/migrate/jobs/{id}/resume`（仅 Admin）：请求继续已暂停任务——唤醒挂起的后台循环恢复搬运。
- 进度快照新增暴露暂停态（`paused: bool`）；阶段枚举新增 `paused` / `cancelled`。
- 前端迁移页进度面板加 **取消 / 暂停 / 继续** 按钮，按任务态启停（仅进行中可暂停 / 取消；仅已暂停可继续；终态全禁用）。
- 鉴权与错误：均仅 Admin（匿名 401 / 非 Admin 403）；未知 `id`（含已淘汰）404；对**已结束**任务（done/failed/cancelled）的控制为幂等空操作、返 200（不报错、不改终态）。
- 范围内：在线拉取（FR-82/83）异步任务的取消 + 暂停 / 继续。
- 不做（范围外）：离线搬运（FR-38/39，本就同步）控制；持久化控制状态（服务器重启任务即丢失，沿用 ADR-0019）；单 asset 粒度中断（控制只在 asset 边界生效，正在下载的单个 asset 跑完再停）；任务排队 / 限流。

## 3. 设计（怎么做）

无新架构决策（仍是进程内、不落库、轮询、锁外做 IO），不写新 ADR；沿用 ADR-0019。

- `migrate` 模块（`online.rs`）：
  - 新增 `JobControl`：`cancel: AtomicBool` + `paused: AtomicBool` + `notify: tokio::sync::Notify`（暂停挂起、继续 / 取消唤醒）。提供 `request_cancel` / `request_pause` / `request_resume`（置标志 + `notify_waiters`）等纯方法。**用 std `AtomicBool` + 既有 `tokio::sync::Notify`，不新增依赖 / feature。**
  - `OnlinePullPhase` 增 `Paused` / `Cancelled`；`OnlinePullProgress` 增 `paused: bool`。
  - `migrate_online_with_progress` / `pull_repo_assets` 增 `control: &JobControl` 形参：**逐 asset 循环在每个 asset 处理前**检查——`cancel`→标 `phase=Cancelled` 收尾返回（不算失败、不再搬后续）；`paused`→进度标 `paused=true` / `phase=Paused`，`await` `notify.notified()` 直到被继续唤醒（醒来复核 cancel）。控制检查 / 等待在进度锁外。
- `api` 层（薄）：
  - `MigrationJobs` 注册表条目从「仅进度」扩为「进度 + `Arc<JobControl>`」；`register` 接受 control；新增 `control(id)` 取句柄。`get` / `list` 行为不变。
  - `migrate_nexus_online` 起任务时建 `Arc<JobControl>`、登记并注入后台任务。
  - 新增 3 个薄 handler（cancel/pause/resume）：`require_admin` → 取 control（无则 404）→ 调对应 `request_*` → 200。已结束任务的请求由 `request_*` 幂等吞掉。
  - 进度 DTO 与任务列表 DTO 暴露 `paused`。
- 前端：`OnlineJobPanel` 加按钮区，调新增的 `cancelMigrationJob` / `pauseMigrationJob` / `resumeMigrationJob`；按 `phase` / `paused` 决定按钮可用性；轮询照旧反映新态。
- 依赖方向不变；进度 Mutex 临界区只更新字段、控制等待在锁外（锁外做 IO / 不持锁阻塞）。

## 4. 任务拆分

- [x] `JobControl` + `OnlinePullPhase::Paused/Cancelled` + `paused` 字段 + 循环接入控制 + 单测
- [x] `MigrationJobs` 存 `JobControl` + `control(id)` + 单测
- [x] API：`online/migrate` 注入 control；cancel/pause/resume 三端点 + 路由；DTO 暴露 paused；集成测试（鉴权 / 404）
- [x] 前端：进度面板 取消 / 暂停 / 继续 按钮 + types/endpoints
- [x] 文档同步：PRD 状态、ARCHITECTURE、API、CHANGELOG

## 5. 验收标准

- 单元：取消后逐 asset 循环停止且任务标 `cancelled`、不再搬后续 asset（已搬运的保留）；暂停后循环挂起不推进、继续后恢复并搬完（用阻塞 mock / Notify 卡时序断言）；`request_*` 对已结束任务为幂等空操作。
- 集成：cancel/pause/resume 三端点匿名 401 / 非 Admin 403 / 未知 id 404 / 已结束任务 200。
- 前端：`pnpm test` + `pnpm build` 过。
- **真机（需用户确认通过）**：对真实在线 Nexus 发起在线拉取，前端点暂停→进度停推、点继续→恢复推进、点取消→停止并显示已取消；沿用 FR-82/83 真机验收。

## 6. 风险 / 待定

- 控制只在 asset 边界生效：正在下载的单个 asset 会先跑完才响应控制（大文件下可感知延迟，可接受——迁移低频）。
- 暂停态下若服务器重启，任务（含暂停态）丢失，靠幂等重跑恢复（既定取舍 ADR-0019）。
- 取消是协作式（cooperative）：置标志 + 唤醒，后台循环自行退出，不强杀 tokio 任务。
