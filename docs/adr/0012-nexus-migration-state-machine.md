# ADR-0012：Nexus 迁移任务状态机、三来源发现与凭据引用

## 状态

已接受（0.4.0 规格期定稿；实现过程中仅允许在不破坏对外契约的前提下细化）

## 背景

0.3.0 已具备 Raw/Maven/npm 的 hosted/proxy/group 与内容寻址 blob。0.4.0 要让管理员把 **Nexus OSS 3.70.x** 的仓库与制品迁入本系统。迁移是长时、可中断、需可报告的异步作业，且凭据不得入库。需要固定：

1. 任务状态机与持久化边界
2. 三来源发现如何统一为「计划预览」
3. 凭据如何注入、冲突策略默认值
4. 发现是否落库、创建与启动是否分离、进程崩溃后如何恢复

## 决策

### 1. 任务模型与状态机

- 持久化表 **`migration_task`**（SQLite，向前迁移脚本新增），一任务一行主状态 + JSON 扩展（计划摘要、断点、统计、错误摘要）。**凡进入系统的迁移任务一律落库**，无纯内存任务。
- 状态机（单向为主，允许失败/取消后的有限回环）：

```
discover 成功 ──落库──► planned ──POST .../start──► running ──► completed
                           │                        │
                           │                        ├→ failed
                           │                        └→ cancelled
                           └→ cancelled（未启动前可取消）

failed / cancelled ──POST .../resume──► running（从断点续；不可改来源/策略后原地复用，改配置须新建任务）
```

- **发现（discover）落库**：`POST /api/v1/migrations/discover` 同步执行发现；成功后 **写入一条 `planned` 任务**（含 `plan_json`、来源配置、凭据引用、冲突策略），响应同时返回 `taskId` + `MigrationPlan`。发现失败不落库（或落 `failed` 仅当已建行——MVP：**失败不落库**，仅返回错误）。
- **显式 start**：创建/发现成功后的任务状态为 **`planned`，不得自动进入 `running`**。仅 `POST /api/v1/migrations/{id}/start` 将 `planned → running` 并启动异步执行。
- **亦可** `POST /api/v1/migrations` 直接创建 `planned` 任务（body 带 plan 或 source；若无 plan 则服务端先 discover 再落 `planned`），同样须显式 start。
- 执行在进程内 **后台 goroutine + 任务级互斥**（单实例模型，ADR-0002）；不引入外部队列。

### 2. 进程崩溃与 `running` 回收（定稿）

- 服务启动时扫描状态为 **`running`** 的任务 → **一律标为 `failed`**，`error_message` 记「进程中断，请 resume」类中文说明；**不自动 resume**（避免启动风暴与半写入竞态）。
- 运维/管理员对失败任务调用 **`POST .../resume`** 从断点继续。

### 3. 三来源统一为 DiscoverySource 接口

| 来源 ID          | 输入                | 发现方式                                |
| ---------------- | ------------------- | --------------------------------------- |
| `online_rest`    | 基址 URL + 凭据引用 | Nexus REST（仓库列表 + 资产/组件分页）  |
| `offline_dir`    | 本地路径            | 扫描 Nexus 原生 blob/store 目录布局     |
| `offline_bundle` | 自有离线包路径      | 扫描约定目录/清单（版本内文档固化格式） |

发现输出统一 **`MigrationPlan`**：待迁仓库列表（名/格式/类型建议）、资产规模估算、不支持项警告。执行阶段按 plan 条目流式写入本系统 hosted 仓库（格式限 0.3 已支持的 raw/maven/npm）。

### 4. 冲突策略（任务级，创建/discover 时选定，之后不可改）

| 策略           | 行为                                                              |
| -------------- | ----------------------------------------------------------------- |
| `skip`（默认） | 目标 `(repo, path)` 已存在则跳过，计入 report.skipped             |
| `overwrite`    | 覆盖写 blob+asset（与 hosted PUT 语义一致）                       |
| `fail`         | 首次冲突即任务失败，保留断点；resume 仍用原策略，改策略须新建任务 |

### 5. 凭据安全

- 任务与计划中 **只存凭据引用名**（如 `NEXUS_BASIC`），运行时从环境变量读取实际用户名/口令或 Token。
- 引用名可出现在 API 请求体与 DB；**明文密钥永不入库、不进日志、不进报告**。
- 与 proxy 上游凭据约定一致：`SECURITY.md` / OPERATIONS 环境变量注入。

### 6. 幂等与断点

- 断点键：`(task_id, source_repo, path)` 或等价游标（REST 分页 token / 目录 walk 位置）。
- 写入复用 `AssetService.Put` / blob 内容寻址：相同内容天然去重；元数据 `Upsert` 保证路径幂等。
- resume 从断点继续，已成功项不重复计失败。

### 7. API 前缀（与 docs/API.md 对齐并补 start）

- `POST /api/v1/migrations/discover` — 发现并 **落库为 planned**
- `POST /api/v1/migrations` — 创建 **planned** 任务（不自动跑）
- `GET /api/v1/migrations` / `GET /api/v1/migrations/{id}`
- `POST /api/v1/migrations/{id}/start` — **planned → running**
- `POST /api/v1/migrations/{id}/resume` | `cancel`
- `GET /api/v1/migrations/{id}/report`
- 仅 **admin** 可调用。

## 理由

- 单实例 + SQLite 下进程内异步足够 MVP，避免过早引入队列/分布式。
- 三来源统一 Plan，执行层只认 Plan，降低格式分支。
- 凭据引用与现有运维模型一致，满足 NFR-06。
- **discover 落库 + planned + 显式 start**：向导可预览再确认，避免误触大迁；任务可审计、可取消未启动项。
- **崩溃标 failed、等人 resume**：行为可预期，不在启动路径上隐式重跑重 IO。

## 后果

- 正面：可续传、可报告、可审计；与 0.3 blob/asset 无缝衔接；启动安全。
- 负面 / 约束：进程重启后须人工 resume；多实例部署不在 0.4 范围。Nexus 版本以 3.70.x 为验收基线，其它版本 best-effort。
- 不做：迁移 Docker/Cargo 等 0.6 格式；双向同步；Nexus 配置（cleanup policy 等）完整镜像；创建即自动 running。

## 备选方案

- **外部队列（Redis/等）**：与零依赖目标冲突，否决于 0.4。
- **仅在线 REST**：无法覆盖气隙场景，否决。
- **同步阻塞 HTTP 直到迁完**：无法承载大仓，否决。
- **discover 不落库、纯预览**：否决——已定 discover 成功即落 `planned`。
- **创建即 running**：否决——已定须显式 start。
- **启动时自动 resume running**：否决——已定标 failed 等人 resume。
