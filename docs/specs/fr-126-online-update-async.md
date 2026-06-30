# 功能规格：在线更新异步化 + 进度页 + 日志

> 状态：开发中　·　关联 PRD：FR-126（增强 FR-85/87）　·　关联 ADR：复用 ADR-0021（自更新）/ ADR-0026（回滚）/ ADR-0032（重启与备份）/ ADR-0019（异步 job 范式，借鉴非照搬：本功能需跨重启留存终态）　·　分支：feature/fr-126-update-async

## 1. 背景与目标

当前在线更新（FR-85/87，已交付）的「检查」与「应用」都是**同步阻塞**端点：

- `GET /update/check` 同步联网查 Release，慢上游会拖住请求。
- `POST /update/apply` 同步「下载 → 校验 → 替换 → 置位重启」一镗到底才返回；前置反向代理（1Panel/OpenResty 等）会在长下载时 **504**；前端只能用「客户端模拟进度条卡 95%」假装进度（见 `SystemPage.tsx` L86-208）；apply 触发后不跳转、连接随重启而断；检查结果不留存，每次进系统页都得重新点「检查更新」。

本功能把检查 / 应用改为**进程内异步 job**（立即返回 `job_id`、前端轮询真实进度），全过程写中文分级系统日志（后台 `tail` 可查），把**检查结果**与**应用终态**留存到数据目录状态文件（重启后可读回续看），触发后前端自动进入进度展示。属 P2。**不破坏** FR-85 既有 apply/rollback/restart 的安全语义（仅 sha256 校验、原子替换、单飞互斥、出站默认关闭门）。

## 2. 需求（要什么）

- **检查异步化 + 结果留存**：
  - `POST /api/v1/update/check`（仅 Admin）：触发异步检查 job，立即返回 `202 { job_id }`；后台联网查 Release、比对版本，把 `UpdateCheck` 结果写进度并**留存到状态文件**。
  - `GET /api/v1/update/check`（仅 Admin）：**改为只读取留存的上次检查结果**（不联网），`{ result?: UpdateCheck, checked_at? }`；无留存返回空。前端进页即可显示上次结果，不必每次重点检查。
- **应用异步化 + 重启后可续看**：
  - `POST /api/v1/update/apply`（仅 Admin）：抢 apply 单飞互斥（抢不到 409「更新进行中」，语义不变）→ 立即返回 `202 { job_id }`；后台执行「下载 → 校验 → 替换」并逐阶段更新进度，替换成功后把**终态留存到状态文件**（含 `new_version`、phase=restarting），再置位重启请求触发优雅停机。
  - 重启后：进程加载时读状态文件，把上次 apply 终态回填进 `UpdateJobs` 注册表（标记为已重启完成），前端经 `GET /update/jobs` 或留存的 `job_id` 重连即可看到「上次更新结果」。
- **进度轮询**：
  - `GET /api/v1/update/jobs/{id}`（仅 Admin）：返回该 job 进度快照（阶段 / 当前/最新版本 / 错误）；未知 id 404。
  - `GET /api/v1/update/jobs`（仅 Admin）：列出活动 / 近期 + 重启后回填的 job 摘要，供重连找回。
- **全过程系统日志**：检查 / 下载 / 校验 / 替换 / 重启各阶段写中文分级 `tracing` 日志（INFO 正常阶段、WARN 校验失败 / 上游异常、ERROR 本地替换失败），落 FR-107 滚动日志文件，后台可 `tail`。
- **前端自动进入进度页**：触发应用 / 检查后，系统页「在线更新」区改为展示真实 job 进度（阶段文案 + 进度态），替换原客户端模拟进度条；记 `job_id` 于 localStorage，进页 / 刷新若有进行中 / 近期 job 则重连续看。
- **回滚（FR-104）**：可一并异步化为同一 job 形态（与 apply 共用单飞），保证「触发后自动进度 + 重启后续看」体验一致。

- 范围内：FR-85 检查 / 应用（+ FR-104 回滚）执行异步化 + 进度轮询 + 检查结果留存 + 应用终态跨重启留存 + 系统日志 + 前端进度页与重连。
- 不做（范围外）：把更新 job 做成完整持久化任务表（只留存「上次检查结果 + 上次 apply 终态」单文件，不存历史多任务）；SSE/websocket 推送（沿用轮询）；字节级下载进度百分比（按**阶段**反馈，下载阶段无字节回传则只显「下载中」不强求百分比）；多实例共享 job（单二进制单实例）；改动 FR-85 的下载 / 校验 / 替换 / 重启核心机制与安全门。

## 3. 设计（怎么做）

借鉴 ADR-0019 的「进程内有界 job 注册表 + 轮询 + 客户端重连」范式（见 `api/migration_jobs.rs` / `migrate/online.rs` / `api/migrate.rs`），但**新增一处关键差异**：apply 会替换二进制并自动重启，进程内注册表随之消失，故 apply **终态 + 检查结果须落数据目录状态文件**，重启后读回——这是 ADR-0019「不落库、重启即丢、靠幂等重跑」之外的必要扩展（更新不可重跑恢复，必须留存结果给用户看）。状态文件而非 DB：守 `update → config`（不依赖 `meta`、不碰 SQLite）的分层不变量，且 `update` 模块本就在 `data_dir` 下做文件 IO（`update-tmp`）。

### 后端

- **`update` 模块**（`src/update/`）：
  - 新增 `UpdatePhase`（枚举 `Checking` / `Downloading` / `Verifying` / `Replacing` / `Restarting` / `Done` / `Failed`，`serde rename_all snake_case`）与 `UpdateProgress`（`kind`: `check`/`apply`/`rollback` + `phase` + `current_version` + `latest_version?` + `check?: UpdateCheck` + `new_version?` + `error?` + `restarted: bool`），`Serialize`、`Clone`、`Default`。
  - 新增 `apply_update_with_progress` / `check_with_progress` 变体（或在现有 `apply_update`/`build_check` 外包一层逐阶段写 `&Mutex<UpdateProgress>`）：复用现有 `fetch_latest_release` / `download_to_file` / `verify_checksum` / `execute_replace`，仅在各阶段边界更新进度 + 写 `tracing` 日志，**不改下载 / 校验 / 替换核心逻辑与失败回滚**。锁外做 IO（进度锁只更新内存态）。
  - 新增**状态文件读写**（纯文件 IO，依赖 config/fs）：`persist_state(data_dir, &UpdateState)` / `load_state(data_dir) -> Option<UpdateState>`，落 `{data_dir}/update-state.json`，`UpdateState { last_check?: (UpdateCheck, checked_at), last_apply?: UpdateProgress 终态 }`。写为原子（temp + rename）。**不含任何凭据**（token 绝不写入）。
- **`api` 层（薄）**：
  - 新增 `api/update_jobs.rs`：`UpdateJobs` 有界注册表（`job_id -> Arc<Mutex<UpdateProgress>>`，容量如 20），登记 / 查询 / 列表 / 越界淘汰（仿 `MigrationJobs`，但无需 `JobControl`——更新阶段不支持取消，避免半截替换）。经 `Extension` 注入。
  - `POST /update/check`：require_admin → 校验 enabled（关则 409，沿用 `build_source`）→ 生成 `job_id`、登记进度、`tokio::spawn` 后台检查（结束写进度 + `persist_state` 留存 last_check）→ 立即 202 `{job_id}`。
  - `GET /update/check`：require_admin → `load_state` 取 last_check → 返回 `{ result?, checked_at? }`（不联网）。
  - `POST /update/apply`：require_admin → 抢 `try_begin_apply`（409 不变）→ 生成 `job_id`、登记进度、`tokio::spawn`（持 guard 入任务，保证全程单飞）后台执行 apply_with_progress：成功则 `persist_state`（last_apply 终态 = restarting + new_version）→ 置位 `request_restart`；失败则进度标 Failed + 日志。立即 202 `{job_id}`。
  - `POST /update/rollback`：同 apply 形态异步化（共用单飞）。
  - `GET /update/jobs/{id}` / `GET /update/jobs`：读注册表返回进度快照 / 摘要列表（仿 migrate）。
  - **重启后回填**：`build_router`（或 main 启动早期）构造 `UpdateJobs` 后，`load_state` 若有 last_apply 终态则以一个合成 `job_id`（或固定占位 id）回填注册表 + 标 `restarted=true`，使 `GET /update/jobs` 重启后即含「上次更新结果」。
  - `AppState` 已持 `config`（data_dir）、`restart`、`settings`，无需新字段；`UpdateJobs` 经 Extension 注入（同 `MigrationJobs`）。
- 依赖方向不变（`api → update → config`）；handler 保持薄；凭据 / token 不入状态文件、不进日志（沿用 FR-85）。

### 前端

- `api/types.ts`：新增 `UpdateJobCreated { job_id }`、`UpdateJob`（progress 快照）、`UpdateCheckCached { result?, checked_at? }`。
- `api/endpoints.ts`：`checkUpdate()` 改为 `triggerCheckUpdate(): Promise<UpdateJobCreated>`（POST）+ `getCachedCheck(): Promise<UpdateCheckCached>`（GET）；`applyUpdate()` / `rollbackUpdate()` 改返 `UpdateJobCreated`；新增 `getUpdateJob(id)` / `listUpdateJobs()`。
- `pages/SystemPage.tsx`：在线更新区改为——进页 `getCachedCheck` 显示上次检查结果 + `listUpdateJobs` 重连；点「检查更新」/「应用」/「回滚」→ 得 `job_id`、存 localStorage、`setInterval` 轮询 `getUpdateJob` 渲染**真实阶段进度**（替换 L86-208 客户端模拟进度条 + applyTimer）；终态（done/failed/restarting）停轮询、展示结果（restarting 显「已触发更新、正在重启，稍后刷新」并保留可重连）。文案进 `i18n/locales/zh-CN/system.ts`（已接入 i18next）。

## 4. 任务拆分

- [ ] `update` 模块：`UpdatePhase` / `UpdateProgress` / `UpdateState` 类型 + `*_with_progress` 变体（逐阶段进度 + 中文分级日志）+ 状态文件原子读写 + 单测（进度阶段流转 / 状态文件往返 / 不含凭据）
- [ ] `api/update_jobs.rs`：`UpdateJobs` 有界注册表 + 单测（登记 / 查询 / 越界淘汰）
- [ ] API：`POST /update/check`（异步）/ `GET /update/check`（读留存）/ `POST /update/apply`（异步 + 留存 + 单飞）/ `POST /update/rollback`（异步）/ `GET /update/jobs/{id}` / `GET /update/jobs`；重启后回填；鉴权 + 单飞 + 留存集成测试
- [ ] 前端：endpoints/types 改造 + SystemPage 进度页（轮询 / 重连 / 留存检查结果）+ i18n 文案 + vitest
- [ ] 文档同步：本规格、PRD 状态（FR-126 行 计划→开发中）、API.md（更新端点改异步契约）、ARCHITECTURE（在线更新机制段补异步 + 状态文件留存）、CHANGELOG 未发布段
- [ ] 真机维度：标「待真机」——反代后触发不 504 / 真下载替换 / 真重启后续看

## 5. 验收标准

- 单元：`UpdateProgress` 随阶段推进正确（checking→…→done/failed）；状态文件写入后读回一致且**不含 token / 凭据**；`UpdateJobs` 登记 / 查询 / 越界淘汰正确。
- 集成：
  - `POST /update/check` 返 202 `{job_id}` 且不阻塞；检查 job 完成后 `GET /update/check` 读到留存结果（不联网）。
  - `POST /update/apply` 抢单飞返 202；已有在途时第二个 409「更新进行中」（沿用 FR-85 不变量）；enabled=false 时 check/apply 仍 409、不联网、不开任务。
  - `GET /update/jobs/{id}` 鉴权（匿名 401 / 非 Admin 403）、未知 id 404；`GET /update/jobs` 鉴权。
  - 重启后回填：预置状态文件 + 构造注册表 → `GET /update/jobs` 含上次 apply 终态（`restarted=true` / `new_version`）。
- 前端 vitest：触发应用 → 轮询进度渲染阶段；进页读 `getCachedCheck` 显示上次结果；有进行中 job 时重连续看。
- **真机（需用户确认通过）**：经前置反代触发应用更新**不再 504**、进度推进到终态；apply 后进程**真重启**、重启后前端能看到「上次更新结果」；后台日志文件可 `tail` 到检查 / 下载 / 校验 / 替换 / 重启各阶段中文日志。**本地不可全验，标「待真机」，不假装真机更新过。**

## 6. 风险 / 待定

- **状态文件 vs 不落库（对 ADR-0019 的有意例外）**：把更新终态留存到 `{data_dir}/update-state.json` 状态文件，是对 ADR-0019「迁移 job 不落库、重启即丢」的**有意例外**——因更新替换二进制后无法靠重跑恢复、必须留存终态供用户查看；用**数据目录单文件**而非 DB，守 `update` 不依赖 `meta` / 不碰 SQLite 的分层不变量。经评审确认**无需单开新 ADR**（属在 ADR-0021/0019 既有决策框架内补实现细节、不推翻任一已接受决策），仅在本规格与 ARCHITECTURE 在线更新段各记一句以可追溯。token / 凭据绝不写入该文件。
- 下载阶段无字节级回传 → 进度按**阶段**反馈（下载中 / 校验中 / 替换中），不强求百分比，避免重蹈「假进度卡 95%」。
- 重启后回填用固定 / 合成 job_id：前端重连以 `GET /update/jobs` 列表为准，localStorage 的旧 job_id 取不到时回退列表。
- apply 不支持取消（避免半截替换坏二进制），故 `UpdateJobs` 不引入 `JobControl`（区别于迁移 job）。
