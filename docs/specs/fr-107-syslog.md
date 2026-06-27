# 功能规格：系统日志页（FR-107）

> 状态：开发中　·　关联 PRD：FR-107　·　分支：feature/fr-107-syslog

## 1. 背景与目标

运维需要在控制台直接查看制品库的**运行时技术日志**（tracing 的 ERROR/WARN/INFO/DEBUG），用于排障与观察服务状态，而无需登录主机翻 stdout。当前 `init_tracing` 只把日志打到 stdout，进程重启后早期日志即丢失、也无法在 Web 端查看。

本期（P2）做三件事：① 应用运行日志在保留 stdout 的同时**落盘到数据目录下的日志文件**；② 新增**仅 Admin** 的读取 API（按级别过滤 + 分页 / tail）；③ 控制台新增「系统日志」页展示。

与**审计日志**（FR-31 / FR-77，业务留痕：写 / 管理 / 授权拒绝类安全事件，落 SQLite）严格区分——本项是**运行时技术日志**，来源是 tracing、载体是文件、不落库、不计安全事件。两者面向不同读者与用途，互不替代。

## 2. 需求（要什么）

- **日志落盘**：`init_tracing` 在保留 stdout 输出的同时，追加一个**文件输出层**，把运行日志写到 `{data_dir}/logs/app.log`。级别过滤沿用既有 `RUST_LOG` / 默认 `info`。
- **简单滚动**：单文件按大小上限滚动——文件超过阈值时滚动一次（`app.log` → `app.log.1`，旧 `.1` 覆盖），不引第三方 appender。够用即可。
- **读取 API**：`GET /api/v1/system-logs`，**仅 Admin**（未认证 401、非管理员 403、Admin 200）：
  - 按级别过滤：`level=ERROR|WARN|INFO|DEBUG`（可选，缺省不过滤；按"该级别及更严重"还是"精确该级别"——取**精确匹配**该级别，简单直观）。
  - tail / 分页：默认返回**最近** N 行（tail 语义，最新在前）；`offset` 从最新行起向更旧偏移、`limit` 容量（默认 200、上限 1000）。
  - 文件不存在 / 为空 → 返回空列表，不报错。
  - 每行解析出 `level` / `timestamp` / `message`（解析失败的行归类为无级别、原文进 message，不丢行）。
- **前端「系统日志」页**：新增 `/system-logs` 路由（仅 Admin，`RequireAuth` + `RequireAdmin`）+ 页面——级别过滤下拉 + 列表（时间 / 级别 / 消息）+ 刷新按钮 + 分页 + 空态。导航入口由并行的 FR-92 添加（本 FR 仅建路由 + 页，不动 AppLayout）。
- 范围内：把"日志行 → 结构化条目"的解析做成**纯函数**，可穷举单测；tail / 级别过滤逻辑可测。
- 不做（范围外）：
  - 多文件按日期 / 时间滚动归档、压缩、保留期清理（简单优先，单文件 + 单次大小滚动）。
  - 实时推送（SSE / WebSocket）、日志检索全文索引、跨进程聚合。
  - 把运行日志写入 SQLite（运行日志载体是文件，不落库；DB 是元数据真源，不混入技术日志）。
  - 结构化 JSON 日志格式切换、日志下载导出。

## 3. 设计（怎么做）

### 3.1 日志落盘（init_tracing 时序调整）

`init_tracing` 在 `main` 第一行调用，但 `data_dir` 要等配置加载 + CLI `--data-dir` 覆盖后（约 20 行后）才确定。为既不丢早期日志、又能在拿到 `data_dir` 后补文件层，采用 **tracing-subscriber 的 `reload` 层**（`std` 默认特性，无需新依赖 / 新特性）：

- `init_tracing()` 用 `registry()` 装配：① EnvFilter（`RUST_LOG` / 默认 `info`）；② stdout fmt 层（始终在）；③ 一个**可重载**的 `Option<fmt 文件层>`，初始为 `None`。`init` 后返回一个 reload **句柄**。
- `main` 拿到 `data_dir` 后：创建 `{data_dir}/logs/` 目录，构造文件 writer，`句柄.reload(Some(文件层))` 把文件层换入——此后日志同时进 stdout 与文件。早期（换入前）的少量配置加载日志仍进 stdout（可接受，不丢；它们晚于 data_dir 确定即入文件）。
- 文件层的 writer：自定义一个轻量 `Write` 实现（`RollingFileWriter`），`make_writer` 时打开 / 追加 `app.log`；写入时若文件超过大小上限则滚动（`app.log` → `app.log.1`）。**不引 `tracing-appender`**——单文件 + 单次大小滚动用 std 文件 API 即可，符合"简单优先 / 优先不引依赖"。

> 决策记录：见 ADR-0029（运行日志增设文件 sink + 读取 API，载体为文件不落库，与审计区分）。本文不重复其决策正文。

### 3.2 日志解析（纯函数，可测）

新增 `logs` 模块（`src/logs.rs` 或 `src/logs/`），承载与 axum / DB 无关的纯逻辑：

- `parse_log_line(line: &str) -> LogEntry`：解析 tracing 默认 fmt 行（形如 `2026-06-27T08:00:00.123456Z  INFO message...`）取出时间戳与级别；识别 `ERROR|WARN|INFO|DEBUG|TRACE`。无法识别级别的行：`level=None`、整行作 `message`，不丢。
- `LogLevel` 枚举（`Error|Warn|Info|Debug|Trace`）+ `parse_level(&str) -> Option<LogLevel>`：解析查询参数级别（大小写不敏感）。
- `tail_filter(lines, level, offset, limit) -> (Vec<LogEntry>, total)`：对全部行解析 → 可选级别精确过滤 → 反转为最新在前 → 按 offset/limit 切片。`total` 为过滤后总数（供分页）。纯函数，输入行集合、输出条目，无 IO。
- 读文件的 IO（打开 `app.log`、按行读）放在 handler 侧的薄封装里，调用纯函数做解析与切片。文件不存在 → 空集合。

### 3.3 读取端点（api 薄 handler）

- `src/api/system_logs.rs`：`list_system_logs(State, Identity, Query)`：`identity.require_admin()?` → 读 `{data_dir}/logs/app.log` 全部行（小文件，受大小滚动上限约束）→ 调 `logs::tail_filter` → 组装统一分页响应 `Paginated { items, total, offset, limit, has_more }`（对齐 API.md §1 与 audit 端点风格）返回。
- `AppState` 已含配置；日志文件路径由 `data_dir` 推出（新增一个由 data_dir 计算 `logs/app.log` 的辅助，集中一处，避免与 init 端魔法字符串重复）。
- 路由：`/api/v1/system-logs` 挂到 `api_v1`（GET）。GET 读取类、不入审计（符合 FR-97）。

### 3.4 前端

- `frontend/src/pages/SystemLogsPage.tsx`：仿 `AuditPage` 风格——`Select` 级别过滤（全部 / ERROR / WARN / INFO / DEBUG）+ 刷新按钮 + `Table`（时间 / 级别 / 消息）+ `Pagination` + loading / 空态。级别用 `Badge` 配色（ERROR 红 / WARN 橙 / INFO 蓝 / DEBUG 灰）。
- `App.tsx`：在管理员路由段加 `path="system-logs"`（`RequireAuth` + `RequireAdmin` 包裹），**不动 AppLayout**。
- `api/types.ts`：`SystemLogEntryDto { timestamp: string | null; level: string | null; message: string }` + `SystemLogListParams { level?; offset?; limit? }`。
- `api/endpoints.ts`：`listSystemLogs(params) -> Paginated<SystemLogEntryDto>`，`GET /system-logs`。

## 4. 任务拆分

- [x] 复制 `_template.md` → 本规格；PRD FR-107 状态 计划→开发中（仅该行）。
- [x] 写 ADR-0029（运行日志增设文件 sink + 读取 API；载体文件不落库；reload 补层时序；与审计区分；为何不引 appender）；ADR README 索引加一行。
- [x] 后端测试先行：①级别解析纯函数；②`parse_log_line` 解析正常 / 异常行；③`tail_filter` tail / 级别过滤 / 分页；④端点鉴权矩阵（匿名 401 / User 403 / Admin 200）；⑤文件缺失返空。
- [x] 实现 `logs` 模块（纯函数 + RollingFileWriter）+ `init_tracing` reload 改造 + `api/system_logs.rs` + 路由。
- [x] 前端测试先行：系统日志页渲染、级别过滤、空态；实现页面 + 路由 + api types/endpoint。
- [x] 文档同步：PRD 状态、ARCHITECTURE（logs 模块 + 文件 sink 机制）、API.md（端点）、CHANGELOG（新增一行）。

## 5. 验收标准

- **日志落盘**：服务启动后 `{data_dir}/logs/app.log` 存在且持续写入运行日志；stdout 输出保留不变。
- **端点鉴权**：`GET /api/v1/system-logs` 匿名 401、普通 User 403、Admin 200。
- **过滤 / 分页**：`level=ERROR` 仅返 ERROR 行；tail 最近 N 行最新在前；`offset`/`limit` 正确切片；`has_more` 正确。
- **健壮**：日志文件不存在 / 为空 → 返回 `{ items: [], total: 0, ... }`，HTTP 200 不报错；无法解析级别的行不丢、归无级别。
- **纯函数单测**：`parse_level` / `parse_log_line` / `tail_filter` 覆盖正常 + 边界（空、异常行、级别大小写、offset 越界）。
- **前端**：系统日志页渲染列表、级别过滤触发按参数请求、空态文案；`pnpm -C frontend build` + `test` + `lint` 全绿。
- `rustup run 1.96.0` fmt + clippy（-D warnings）全清；`cargo test` 相关全绿。
- 守 `#![forbid(unsafe_code)]`；中文分级日志；不外发；不新增依赖（用既有 tracing-subscriber 的 reload）。

## 6. 风险 / 待定

- **早期日志入文件时机**：data_dir 确定前（配置加载阶段）的少量日志只进 stdout、不进文件——属已知取舍（文件层须等 data_dir）。不为此提前硬编码默认路径。
- **滚动策略简单**：单文件 + 单次大小滚动（保留 1 个 `.1`），不做按日期归档 / 压缩 / 保留期；够 P2 用，后续若需更强滚动再走 ADR。
- **读取即读全文件**：端点读 `app.log` 全部行再切片——文件大小受滚动上限约束（如几 MB），可接受；不做流式倒读（简单优先）。
- **并发写**：tracing 的 `MakeWriter` 每次写取一次 writer，文件追加 + 滚动判断需在写路径内自洽（用 `Mutex` 串行化写与滚动，写在锁内但为本地小 IO，符合既有 stdout 写串行化基调）。
