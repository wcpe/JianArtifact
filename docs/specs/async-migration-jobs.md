# 功能规格：迁移任务异步化与进度可观测（在线拉取）

> 状态：开发中　·　关联 PRD：FR-83　·　关联 ADR：ADR-0019（扩展 ADR-0006）　·　分支：feature/async-migration-jobs

## 1. 背景与目标

FR-82 的在线拉取迁移是同步阻塞的（实测某仓库约 4 分钟无进度反馈，且浏览器一断就失去可见性）。本功能把**在线拉取的执行**改为**进程内异步任务**：触发即返回 `job_id`，后台跑，前端轮询进度、展示导入队列，客户端断开可重连续看。属 P2。详见 ADR-0019。

## 2. 需求（要什么）

- `POST /api/v1/migrate/nexus/online/migrate`（仅 Admin）**改为立即返回 `{ job_id }`（202）**，搬运在后台 tokio 任务执行。
- 新增 `GET /api/v1/migrate/jobs/{id}`（仅 Admin）：返回任务进度快照——阶段、总 asset 数、已完成 / 已迁 / 已跳过、当前仓库与 asset、各仓库结果明细、整仓跳过列表、错误（如失败）。
- 新增 `GET /api/v1/migrate/jobs`（仅 Admin）：列出活动 / 近期任务（id + 阶段 + 简要计数），供**重连**找回。
- 前端在线拉取执行后**轮询** `jobs/{id}` 展示进度队列（总数/已完成/已跳过/当前文件 + 进度条）；浏览器 / 网络断开重开后经任务列表或本地记的 `job_id` 重连续看。
- 进度为**进程内、有界**（保留最近 N 个、超出按时序淘汰），**不落库**；服务器重启任务丢失，靠迁移幂等重跑恢复（保留 ADR-0006「无须持久化迁移任务表」）。
- 范围内：在线拉取（FR-82）的执行异步化 + 进度 + 客户端重连。
- 不做（范围外）：离线搬运（FR-38/39）异步化；持久化任务（服务器重启续跑）；SSE/websocket 推送；任务排队 / 限流；单文件断点续传（已定整文件重试）。

## 3. 设计（怎么做）

对齐 ADR-0019（不落库、进程内有界任务、轮询、仅客户端重连）。

- `migrate` 模块：
  - 新增 `OnlinePullProgress`（阶段枚举 `enumerating`/`downloading`/`done`/`failed` + 计数 + 当前项 + repos/skipped_repos/error），`serde::Serialize`。
  - `migrate_online_repositories` 增 `progress: &Mutex<OnlinePullProgress>` 形参：先**枚举该仓库全部 asset（已知总数）**再逐个下载（边搬边更新进度——current、migrated/skipped、done）；保持既有返回报告（供测试 / 最终态）。沿用 FR-82 的重试 / sha256 校验 / 幂等。
- `api` 层（薄）：
  - `AppState` 持有 `MigrationJobs`：`Arc<RwLock<迁移任务有界注册表>>`，键 `job_id` → `Arc<Mutex<OnlinePullProgress>>` + 创建时序（淘汰用）。
  - `online/migrate`：require_admin → 校验 / discover / 选仓库 → 生成 `job_id`、登记进度、`tokio::spawn` 后台跑 `migrate_online_repositories`（结束置 `done`/`failed`）→ **立即返回 `{ job_id }`（202）**。
  - `GET jobs/{id}`：读注册表返回进度快照（未知 404）；`GET jobs`：列任务摘要。
  - 后台任务克隆所需句柄（meta / artifacts / formats / config / 新建 HttpNexusClient / 凭据 / 选择 / 进度 Arc）入 `'static` 任务。
- 前端：迁移页在线执行改异步——发起得 `job_id`、`setInterval` 轮询进度渲染队列与进度条、完成展示报告；记 `job_id` 于 localStorage，加载时若仍 running 则重连续看；并可经 `GET jobs` 列表重连。
- 依赖方向不变；锁外做 IO（注册表锁只护内存态、迁移 IO 在任务内不持注册表锁）；进度 Mutex 临界区只更新计数 / 字段，不在持锁期间下载。

## 4. 任务拆分

- [ ] `OnlinePullProgress` 类型 + `migrate_online_repositories` 接入进度（枚举全量 → 逐个下载更新）+ 单测（进度计数 / 阶段流转）
- [ ] `MigrationJobs` 有界注册表（登记 / 查询 / 列表 / 淘汰）+ 单测
- [ ] API：`online/migrate` 改异步返回 job_id；`GET jobs/{id}`、`GET jobs`；鉴权集成测试
- [ ] 前端：异步发起 + 轮询进度队列 + 完成报告 + 客户端重连
- [ ] 文档同步：PRD 状态、ARCHITECTURE、API、ADR-0019、CHANGELOG

## 5. 验收标准

- 单元：进度随枚举 / 下载推进（总数 / done / migrated / skipped / 当前项 / 阶段）正确；注册表登记 / 查询 / 越界淘汰正确。
- 集成：`online/migrate` 返回 `job_id`（202）且不阻塞；`GET jobs/{id}` 鉴权（匿名 401 / 非 Admin 403）、未知 id 404；`GET jobs` 列表鉴权。
- **真机（需用户确认通过）**：对真实在线 Nexus 发起在线拉取，前端轮询见进度队列推进至完成；中途刷新 / 断开浏览器后重连仍能看到该任务进度续推；最终制品字节一致（沿用 FR-82 验收）。

## 6. 风险 / 待定

- 枚举全量再下载：超大仓库枚举阶段需遍历所有 components 元数据（无下载，较快）；total 已知便于进度条。
- 注册表淘汰阈值 N 取值（默认如 50）；并发多任务由管理员手动触发约束，不内置排队。
- 服务器重启任务丢失为既定取舍（ADR-0019），靠幂等重跑恢复。
