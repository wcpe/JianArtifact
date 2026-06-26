# 功能规格：审计日志全面补齐（全量非读）

> 状态：开发中　·　关联 PRD：FR-97（增强 FR-31）　·　分支：feature/fr-97-audit-full

## 1. 背景与目标

FR-31（ADR-0015）的审计中间件（`src/api/audit.rs`）只记**精选**的写 / 管理 / 授权拒绝事件：用户 / Token / 仓库 / ACL 管理与制品上传 / 删除，其余变更类操作（FR-79 防护配置 PATCH、FR-88 设置 PATCH、FR-91 迁移任务控制、在线更新 apply、用户组与组 ACL 增改删、迁移预览 / 搬运等）经中间件时被 `classify_event` 判 `None`、不入审计，留下可追溯性盲区。

FR-97 把审计覆盖从"精选"扩为"**全量非读**"：所有**变更类**请求（HTTP `POST` / `PUT` / `PATCH` / `DELETE`）都产审计事件；**读取类**（`GET` / `HEAD`，含下载 / 浏览 / 搜索 / 详情）一律不进审计（交使用分析计数，避免刷屏与性能损耗）。属 P2 可观测性主题，扩展 ADR-0015，不新增 ADR、不新增依赖。

## 2. 需求（要什么）

- 范围内：
  - 审计中间件按 **HTTP 方法** 而非精选路径白名单决定是否产事件：**非读方法（POST/PUT/PATCH/DELETE）一律产一条审计**，读方法（GET/HEAD/OPTIONS）一律不产。
  - 已知管理 / 格式 / Docker 路径仍归类为**精确语义 action**（`user.create` / `token.issue` / `repo.update` / `acl.update` / `artifact.upload` 等，保持既有取值不变），并带 `target_repo` / `target`。
  - 新覆盖的变更端点归入**合理 action**：设置 PATCH → `settings.update`；防护配置 PATCH → `protection.config.update`；迁移控制（cancel/pause/resume）→ `migrate.job.control`；迁移预览 / 搬运（preview/migrate）→ `migrate.run`；在线更新 apply → `update.apply`；组增删 → `group.create` / `group.delete`、组成员增删 → `group.member.update`、组 ACL 增删 → `group.acl.update`；登出 / 刷新 → `auth.logout` / `auth.refresh`。
  - 其余**未显式归类**的非读路径有**兜底 action**：按方法记 `change.post` / `change.put` / `change.patch` / `change.delete`，保证"无遗漏"——任何新增的非读端点即便未单列也会留痕。
  - 登录（`POST /api/v1/auth/login`）仍由登录 handler 显式发事件（需记被尝试用户名），中间件继续跳过该路径，避免重复。
  - 保留既有异步 channel 投递 + 批量落库 + 保留期轮转；主路径只做一次非阻塞 enqueue，采集 / 写入失败仅 WARN、不影响业务。
- 不做（范围外）：
  - 不在各 handler 里散落审计调用（登录等需特殊上下文者沿用现状）。
  - 不改 `audit_log` 表结构、`NewAuditEntry` 字段、查询端点、轮转任务、`actor_kind` 归类。
  - 不动使用分析（FR-57）：GET 下载 / 详情仍只走使用分析计数，不进审计。
  - 不新增 ADR、不新增依赖。

## 3. 设计（怎么做）

仅改 `src/api/audit.rs` 的事件归类逻辑（`classify_event` 及其下游分发函数），中间件投递链路、channel、写入任务、查询端点不变。

**覆盖矩阵（method × 路径 → action）**：

| 路径前缀 | 方法 | action | target_repo | target |
|---|---|---|---|---|
| `/api/v1/auth/login` | POST | （跳过，handler 发 `login`） | - | - |
| `/api/v1/auth/logout` | POST | `auth.logout` | - | - |
| `/api/v1/auth/refresh` | POST | `auth.refresh` | - | - |
| `/api/v1/users` | POST | `user.create` | - | - |
| `/api/v1/users/{id}` | PATCH / DELETE | `user.update` / `user.delete` | - | id |
| `/api/v1/tokens` | POST | `token.issue` | - | - |
| `/api/v1/tokens/{id}` | DELETE | `token.revoke` | - | id |
| `/api/v1/repositories` | POST | `repo.create` | - | - |
| `/api/v1/repositories/{id}` | PATCH / DELETE | `repo.update` / `repo.delete` | id | - |
| `/api/v1/repositories/{id}/upload` | POST | `artifact.upload` | id | - |
| `/api/v1/repositories/{id}/artifacts/{path}` | DELETE | `artifact.delete` | id | path |
| `/api/v1/repositories/{id}/acl[/{aid}]` | POST / DELETE | `acl.update` | id | - |
| `/api/v1/repositories/{id}/group-acl[/{aid}]` | POST / DELETE | `group.acl.update` | id | - |
| `/api/v1/groups` | POST | `group.create` | - | - |
| `/api/v1/groups/{id}` | DELETE | `group.delete` | - | id |
| `/api/v1/groups/{id}/members[/{uid}]` | POST / DELETE | `group.member.update` | - | id |
| `/api/v1/settings` | PATCH | `settings.update` | - | - |
| `/api/v1/protection/config` | PATCH | `protection.config.update` | - | - |
| `/api/v1/migrate/nexus/**`（preview / migrate） | POST | `migrate.run` | - | 末段（如 `online/migrate`） |
| `/api/v1/migrate/jobs/{id}/{cancel\|pause\|resume}` | POST | `migrate.job.control` | - | `id/动作` |
| `/api/v1/update/apply` | POST | `update.apply` | - | - |
| 其余 `/api/v1/**` 非读 | POST/PUT/PATCH/DELETE | `change.{method}` 兜底 | - | rest |
| `/v2/**`（Docker）| PUT / DELETE | `artifact.upload` / `artifact.delete` | name | manifests/blobs 引用 |
| `/v2/**`（Docker）| POST / PATCH（blob 分块） | `change.{method}` 兜底 | name | 引用 |
| `/{repo}/{path}`（格式 API） | PUT / POST / DELETE | `artifact.upload`（PUT/POST）/ `artifact.delete` | repo | path |

**读取排除**：`classify_event` 入口先判方法——`GET` / `HEAD` 直接返回 `None`；其余方法进入路径归类。已知精确路径返回语义 action，未知非读路径返回兜底 `change.{method}`。健康检查 `/health`、`/metrics`、`/v2/`、`/v2/token`、SPA 静态资源均为 GET，自然被方法门挡下、不产事件。

**action 命名约定**：沿用 ADR-0015 既有 `<域>.<动作>` 小写点分风格；新增 action 与既有同构（如 `settings.update` / `group.create`），集中为静态字符串常量，不散魔法值。兜底 action `change.{method}` 用小写方法名。

**脱敏**：不变——`actor` 只记用户名 / `anonymous`（由身份解析中间件注入，中间件不读请求体 / 不读 `Authorization` 值本体），`detail` 始终为 `None`，`target` 只取 URL 路径段（路径本身不含凭据；登录 / Token 明文在 body，中间件不触碰 body）。密码 / Token / JWT / 上游凭据 / 代理凭据绝不进审计。

## 4. 任务拆分

- [x] 写 spec：覆盖矩阵 + 读取排除 + action 命名 + 脱敏
- [x] PRD §4 FR-97 行 计划→开发中（仅此一行）
- [x] 测试先行（仿 tests/audit_api.rs）：各变更端点产事件、GET 不产、脱敏、非阻塞
- [x] 实现：`classify_event` 改为"方法门 + 精确归类 + 兜底"
- [x] 文档同步：ARCHITECTURE 审计段（精选→全量非读）、CHANGELOG 未发布段
- [x] 验证门：`rustup run 1.96.0` fmt + clippy 全清、`cargo test --jobs 4` 全绿

## 5. 验收标准

- 单元（audit.rs `#[cfg(test)]`）：`classify_event` 对 GET/HEAD 一律 `None`；对各非读端点返回预期 action（含新覆盖端点与兜底）；既有精确归类用例不回归。
- 集成（tests/audit_api.rs）：经 axum 端到端，设置 PATCH / 防护配置 PATCH / 迁移控制 / 组管理等变更请求最终落审计库且 action 正确；GET 类（列表 / 详情 / 搜索 / 下载）不产审计；审计记录不含任何凭据明文；采集异步不阻塞、写入任务缺失时业务仍成功。
- fmt + clippy 全清、`cargo test --jobs 4` 全绿。

## 6. 风险 / 待定

- 兜底 `change.{method}` 会让审计量较"精选"上升：变更类请求频率远低于读流量，且已有 channel 有界丢弃 + 保留期轮转兜底，体量可控；读流量（占绝大多数）仍被方法门完全挡在审计之外。
- 迁移预览（preview）是 POST 但语义偏"读"（只枚举、不落制品）：按"方法非读即留痕"统一归 `migrate.run`，不为其开特例（简单优先，避免按语义猜测分支）。
