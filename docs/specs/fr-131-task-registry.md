# 功能规格：通用异步任务注册表

> 状态：开发中　·　关联 PRD：FR-131（增强 FR-83）　·　关联 ADR：ADR-0038（修订 ADR-0019）　·　分支：feature/fr-131-task-registry

## 1. 背景与目标

当前进程内有两套**各自独立**的任务注册表：

- `api/migration_jobs.rs` 的 `MigrationJobs`（FR-83，ADR-0019）：只管迁移 job（在线拉取 FR-82/83、离线预览 FR-124、离线搬运 FR-125），进度为 `OnlinePullProgress`。
- `api/update_jobs.rs` 的 `UpdateJobs`（FR-126）：只管在线更新 job（检查 / 应用 / 回滚），进度为 `UpdateProgress`。

漏洞库刷新（FR-70）则是 `main.rs` 里一条后台周期循环，**没有任何 job 概念**、不在任何注册表中。

问题：长耗时任务散落多处、无统一入口，运维无法「一处看全部在途与近期任务」；迁移没有单飞约束（理论上可并发触发多个搬运）；离页 / 重连只能各端点各查。本功能把进程内任务**收口为统一任务模型**（统一 `kind` / 状态 / 起止时间 + 有界历史），让多类长耗时任务统一进入、可列出、可找回。属 P2。

## 2. 需求（要什么）

- **统一任务模型**：进程内统一任务注册表（kind-agnostic），每个长耗时任务登记一条轻量记录：`id` + `kind`（migration / update / vuln）+ 统一状态（running / paused / succeeded / failed / cancelled）+ 可选 label + 起 / 止 / 更新时间 + 可选 error。三类任务（迁移 / 在线更新 / 漏洞库刷新）统一接入同一注册表。
- **迁移单飞**：同时只允许一个迁移**搬运**任务在途（running/paused）；第二个迁移触发被拒，返回 `409`。
- **有界历史**：注册表有界，保留近期已完成 / 失败任务供「找回」（不只活跃任务）；超出按时序淘汰最旧。
- **新端点**（仅 Admin）：
  - `GET /api/v1/tasks`：列出活跃 + 近期所有任务（跨 kind），按时序。
  - `GET /api/v1/tasks/{id}`：单任务统一记录 + 对应 kind 的进度明细（迁移取 `OnlinePullProgress`、更新取 `UpdateProgress`，有则附）；未知 id 404。
- **兼容既有契约**：既有 `GET /migrate/jobs` / `GET /migrate/jobs/{id}` / `POST /migrate/jobs/{id}/{cancel,pause,resume}` 与 `GET /update/jobs` / `GET /update/jobs/{id}` **保持不变**（前端轮询契约不破，不破坏 FR-124/125/126）。

- 范围内：统一任务注册表（kind/state/时间 + 有界 + 单飞判定）+ 迁移 / 更新 / 漏洞库刷新接入 + `GET /tasks` / `GET /tasks/{id}` + 迁移单飞 409。
- 不做（范围外）：跨进程重启续跑 / 持久化任务表（**仍不落库、重启即清**，与 ADR-0019「不落库」一致；FR-126 既有的更新终态状态文件留存是其自身的有意例外、本 FR 不改它）；任务排队 / 限流 / 优先级；前端任务中心 UI（FR-132）；增量幂等续传进度模型（FR-134）；vuln 的手动触发端点（vuln 仍是周期循环，本 FR 只把每轮刷新登记进注册表）。

## 3. 设计（怎么做）

架构决策（统一进程内任务模型 + 迁移单飞 + 有界历史保留供找回；仍不跨进程重启续跑）见 ADR-0038（修订 ADR-0019），此处不重复决策正文。

### 后端

- **新增 `api/task_registry.rs`**：统一任务注册表（kind-agnostic、进程内、有界、不落库）。
  - `TaskKind { Migration, Update, Vuln }`（`serde rename_all snake_case`）。
  - `TaskState { Running, Paused, Succeeded, Failed, Cancelled }`（`serde rename_all snake_case`）。
  - `TaskRecord { id, kind, state, label: Option<String>, started_at: u64, updated_at: u64, finished_at: Option<u64>, error: Option<String> }`（`Serialize` / `Clone`）。时间取 Unix 秒。
  - `TaskRegistry { inner: RwLock<Inner{ map: HashMap<id, TaskRecord>, order: VecDeque<id> }>, capacity }`（仿 `MigrationJobs` 有界淘汰结构）。
    - `register(kind, label) -> String`：以调用方给定 id 或自生成 UUID 登记一条 `Running` 记录，越界淘汰最旧。提供 `register_with_id(id, kind, label)` 供迁移 / 更新复用其既有 `job_id`，**同一 id 在统一表与 kind 专表一致**。
    - `set_state(id, state, error)`：更新状态 / 终态时间 / 错误（锁内只改内存态）。
    - `try_begin_migration(id, label) -> bool`：原子「检查无在途迁移 → 登记」——临界区内若已有 `Migration` 处于 `Running`/`Paused` 则返回 false（拒），否则登记并返回 true。单飞判定与登记同一把写锁内完成，杜绝竞态双开。
    - `get(id) -> Option<TaskRecord>` / `list() -> Vec<TaskRecord>`（按 order，新在后）。
  - 纯内存、`#![forbid(unsafe_code)]` 下用 `std::sync` 锁；中毒容忍（`unwrap_or_else(into_inner)`，与既有注册表一致）。
- **`AppState` 增字段 `tasks: Arc<TaskRegistry>`**：在 `main.rs` 构造（供 vuln 周期循环登记），随 `AppState` 共享、`build_router` / handler 经 `State` 取用。既有 `MigrationJobs` / `UpdateJobs` **保留不动**（kind 专进度仍各自存其表，统一表只存轻量记录 + 单飞判定 + 跨 kind 列表）。
- **迁移端点**（`api/migrate.rs`）：
  - 三个**搬运**触发（`migrate_nexus_online` / `migrate_nexus_proxy` / `migrate_nexus_hosted`）登记前先 `tasks.try_begin_migration(job_id, label)`，false 即返回 `409`「已有迁移任务在途」；true 则照常登记 `MigrationJobs` 进度 + 起后台任务。后台任务收尾时按终态 `tasks.set_state(job_id, Succeeded/Failed/Cancelled, error)`。
  - 离线预览（`preview_nexus_offline`）为单次枚举（非搬运），登记为 `Migration` 任务但**不参与单飞门**（预览可与搬运并行、互不阻塞），收尾置终态。
- **更新端点**（`api/update.rs`）：检查 / 应用 / 回滚登记时 `tasks.register_with_id(job_id, Update, label)`；后台收尾置终态（apply/rollback 成功置 `Succeeded`，失败 `Failed`）。apply 单飞沿用既有 `restart.try_begin_apply`（不变），统一表只记录、不接管其单飞。重启后回填的 `last-apply` 历史任务（FR-126）按需在 `backfill` 时补登一条 `Update` 记录（标终态）。
- **漏洞库刷新**（`vuln::spawn_refresh_loop` + `main.rs`）：为守 `vuln` 不反向依赖 `api` 的分层不变量，在 `vuln` 模块定义**轻量回调 trait `RefreshObserver`**（`on_start() -> id` / `on_finish(id, ok, err)`，无 api 依赖）；`spawn_refresh_loop` 增可选 `Option<Arc<dyn RefreshObserver>>` 参数，每轮刷新前后回调。`main.rs` 注入一个由 `Arc<TaskRegistry>` 支撑的 adapter（api 类型，main 可依赖），把每轮 vuln 刷新登记为 `Vuln` 任务并置终态。未启用 vuln 时不注入、注册表无 vuln 记录。
- **新端点**（`api/tasks.rs`，薄 handler）：
  - `GET /api/v1/tasks`（require_admin）→ `tasks.list()` 映射为 DTO 数组。
  - `GET /api/v1/tasks/{id}`（require_admin）→ `tasks.get(id)`（404 未知）+ 据 `kind` 从 `MigrationJobs` / `UpdateJobs` 取进度明细附上（取不到则只回记录）。
- 依赖方向不变：`api → (…)`；`vuln` 仍只依赖 `meta` / `config`（新 trait 在 `vuln` 内、不引 api）。handler 保持薄；不引新依赖（复用 `uuid` / `serde` / std 锁）。

### 前端

- 本 FR **不改前端轮询契约**（既有 migrate/update 端点不变），故前端不动。`GET /tasks` 的前端消费由 FR-132（任务中心）落地。

## 4. 任务拆分

- [ ] `api/task_registry.rs`：`TaskKind` / `TaskState` / `TaskRecord` / `TaskRegistry`（register / register_with_id / set_state / try_begin_migration / get / list / 有界淘汰）+ 单测
- [ ] `AppState` 增 `tasks` 字段；`build_router` 与测试用状态构造补之
- [ ] 迁移端点接入统一表 + 迁移单飞（三搬运 409、预览不拦）+ 后台收尾置终态
- [ ] 更新端点接入统一表（register_with_id + 收尾置终态 + 回填补登）
- [ ] `vuln::RefreshObserver` trait + `spawn_refresh_loop` 可选 observer 参数；`main.rs` 注入 TaskRegistry adapter
- [ ] `api/tasks.rs`：`GET /tasks` / `GET /tasks/{id}` 薄 handler + 路由 + 鉴权 / 单飞 / 列表 / 进度附带集成测试
- [ ] 文档同步：本规格、ADR 占位（修订 ADR-0019）、PRD 状态（FR-131 计划→开发中）、API.md（新增 /tasks 端点）、ARCHITECTURE（任务模型一句）、CHANGELOG 未发布段

## 5. 验收标准

- 单元：注册表登记 / 查询 / 列表 / 有界淘汰正确；`try_begin_migration` 在已有在途迁移时拒第二个、无在途时放行；终态 `set_state` 写状态 / finished_at / error。
- 集成（鉴权矩阵）：`GET /tasks` / `GET /tasks/{id}` 匿名 401、非 Admin 403、Admin 200；未知 id 404。
- 集成（单飞）：连续两次触发迁移搬运，第二个返回 409；第一个结束后可再触发。
- 集成（三 kind 进同一表）：迁移 / 更新 / vuln 任务均能登记进统一表并经 `GET /tasks` 列出（vuln 经 observer adapter；测试可直接驱动 adapter 或注册表断言三 kind 共存）。
- 集成（找回）：起任务后经 `GET /tasks` 列表（活跃 + 近期）找回；已完成任务在有界窗内仍可见。
- 回归：既有 `GET /migrate/jobs(/{id})` / 控制端点、`GET /update/jobs(/{id})` 契约与状态码不变（FR-124/125/126 既有用例继续绿）。
- 真机维度：无新增真机维度（迁移 / 更新各自真机维度仍由 FR-82/83/91/124/125/126 覆盖）。本 FR 为进程内收口，单元 + 集成可全验。

## 6. 风险 / 待定

- **vuln 接入的分层取舍**：vuln 刷新循环在 `vuln` 模块（低于 `api`），直接持 api 的 `TaskRegistry` 会反向跨层。采用「`vuln` 内定义 `RefreshObserver` trait + main 注入 api-backed adapter」解耦，守分层不变量；代价是 `spawn_refresh_loop` 签名加一个可选参数（向后兼容：`None` 行为同旧）。
- **单飞范围**：只对三个迁移**搬运**触发设单飞，离线预览（枚举）不拦，避免预览与搬运互斥误伤。
- **统一表 vs kind 专表双源**：统一表只存轻量记录（状态 / 时间），进度明细仍归各 kind 专表（单一真源）；`GET /tasks/{id}` 据 id 从专表取进度附带，不复制进度到统一表。
- **不落库不变**：统一表进程内、重启即清，与 ADR-0019 一致；FR-126 更新终态的状态文件留存是其自身既有例外、本 FR 不动。
