# 功能规格：L0 地基与首启引导

> 状态：开发中　·　关联 PRD：FR-23 / FR-24 / FR-25 / FR-59　·　分支：feature/p1-foundation

## 1. 背景与目标

把 JianArtifact 的运行地基立起来：单一二进制能加载配置、初始化嵌入式 SQLite 元数据库、初始化文件系统 blob 存储、在空库首启时自举首个管理员，并对外提供健康检查端点。这是第一期（MVP）所有上层能力（认证、仓库、格式处理器、Web UI）赖以运行的底座。属阶段 P1。

本规格只覆盖地基，**不含**登录/Token/Basic Auth 路由、鉴权中间件、任何格式处理器、代理缓存、Web UI——那些是后续批次（提前实现即越界，违反 scope-discipline）。

## 2. 需求（要什么）

- 范围内：
  - **FR-23**：嵌入式 SQLite 元数据存储（五张表：users / tokens / repositories / repo_acl / artifacts）+ 文件系统 blob 存储抽象。
  - **FR-24**：单一二进制 + TOML 配置加载 + 环境变量覆盖（前缀 `JIANARTIFACT_`）。
  - **FR-25**：健康检查端点（`GET /health`，无需认证，返回 200 + 状态 JSON）。
  - **FR-59**：首个管理员引导（空库首启：env 提供则据此建 Admin，否则随机口令打印启动日志，要求改密）。
  - 多校验和计算能力（FR-69 的存储侧基础）：blob 流式写入时同时算 sha256 / sha1 / md5 / sha512。
- 不做（范围外）：认证/鉴权路由与中间件、各格式处理器、proxy 缓存与单飞、Web 控制台、列表分页/搜索、Token 管理、用户管理路由。这些是后续 P1 批次。

## 3. 设计（怎么做）

### 工程结构（单 crate，lib + 薄 bin）

- `src/lib.rs`：库 crate，导出各模块，便于测试复用，避免地基公共 API 误报 dead_code。
- `src/main.rs`：薄入口，`#![forbid(unsafe_code)]`；clap 解析 `--config`（默认 `./config.toml`）与 `--data-dir`（覆盖配置）；初始化 tracing → 加载配置 → 打开 SQLite 跑迁移 → 首启引导 → 构建 router → 监听 serve（含 Ctrl+C 优雅停机）。
- `src/config.rs`：`Config`（server / data / auth / limits 四节），figment 从 TOML + env 加载，env 覆盖 TOML；键名默认对齐 `docs/CONFIG.md`。
- `src/meta/mod.rs`：sqlx `SqlitePool`（WAL + 外键），启动跑 `migrations/`；`count_users` / `create_user` / `get_user_by_username`。元数据唯一真源。
- `migrations/0001_init.sql`：建五表，布尔用 INTEGER、时间 TEXT(ISO8601) 默认 CURRENT_TIMESTAMP，加外键与索引（`repo_acl(repo_id,user_id)`、唯一 `artifacts(repo_id,path)`）。
- `src/storage/mod.rs`：`BlobStore` trait + `LocalFsStore`；流式 `put` 边写边算四摘要、先写临时文件校验再原子落定（按 sha256 分桶寻址）；`get` / `delete`（幂等）/ `exists`。
- `src/auth/mod.rs`：argon2 `hash_password` / `verify_password`；`bootstrap_admin`（仅空库触发：env 双值 → 建 Admin；否则随机高熵口令建默认 `admin`，经 `BootstrapOutcome` 回传给入口打印 WARN）。不开放公开注册。
- `src/api/mod.rs`：`AppState{config, meta, store}`；`ApiError`（`IntoResponse`，返回 `{"error":{"code","message"}}`）；`GET /health`；挂 tower-http trace + request-id 中间件。

### 配置映射

env 仅把“已知节名后的首个下划线”映射为嵌套分隔（`server_port`→`server.port`、`auth_session_ttl_secs`→`auth.session_ttl_secs`），键名内部下划线保留。

### 对齐的 ADR

ADR-0001（技术栈与打包）、ADR-0002（SQLite 元数据）、ADR-0003（argon2 口令）、ADR-0010（首启引导）。本批未引入与 ADR 冲突的决策，故不新增 ADR；figment 作为配置库的实现选择已在 ARCHITECTURE §5 补注。

### 本批新增依赖与理由

| 依赖 | 理由 |
|---|---|
| axum / tokio | HTTP 框架与异步运行时（ADR-0001） |
| tower-http（trace, request-id, util） | 请求追踪与请求 ID 中间件 |
| sqlx（runtime-tokio, sqlite, migrate, macros） | 嵌入式 SQLite 访问 + 迁移 + FromRow/derive；sqlite 经 bundled 零外部依赖（ADR-0002） |
| serde / serde_json / toml | 序列化与 JSON / TOML 解析 |
| figment（toml, env） | TOML + 环境变量分层加载，env 覆盖 TOML（FR-24） |
| argon2 | 口令哈希（ADR-0003） |
| rand | 首启随机口令、Argon2 盐 |
| tracing / tracing-subscriber（env-filter） | 中文分级日志 |
| thiserror / anyhow | 库错误类型 / 入口错误上下文 |
| uuid（v4） | 主键与请求 ID |
| clap（derive） | 命令行参数解析 |
| sha2 / sha1 / md-5 / digest | 多校验和（FR-69）：流式写入同时算 sha256/sha1/md5/sha512，RustCrypto 纯 Rust 轻量实现 |

> 说明：`sha2 / sha1 / md-5 / digest` 不在最初任务给出的依赖清单内，但 FR-69 要求 blob 写入时算四种摘要，无哈希库无法实现。四者均为 RustCrypto 生态轻量纯 Rust 实现（与 argon2 同生态），非重型件，故纳入并在此记录理由。

dev-dependencies：tower（util，测试 oneshot）、http-body-util（读响应体）、tempfile（临时目录）。

## 4. 任务拆分

- [x] 工程骨架：Cargo.toml（release profile strip/lto/panic=abort/codegen-units=1）+ lib/bin。
- [x] 配置加载（config.rs）：figment TOML+env，env 覆盖，键名对齐 CONFIG.md。
- [x] 元数据层（meta + migrations）：连接池 WAL、五表迁移、用户建/计数/查重。
- [x] blob 存储（storage）：流式四摘要、临时文件落定、get/delete/exists。
- [x] 认证与首启引导（auth）：argon2 哈希、bootstrap_admin 三路径。
- [x] API 层（api）：AppState、ApiError、/health、trace + request-id 中间件。
- [x] config.example.toml（占位，键对齐 CONFIG.md）。
- [x] 文档同步：PRD 状态（FR-23/24/25/59 改开发中）、ARCHITECTURE（补 figment）、CHANGELOG。

## 5. 验收标准

- `cargo build` 成功。
- `cargo test` 全绿，覆盖：config env 覆盖 TOML、storage put/get/delete 且四摘要对已知向量正确、meta 建用户/计数/查重、bootstrap（env 建 Admin / 第二次不重复 / env 缺失走随机口令）、/health 集成测试。
- `cargo clippy --all-targets -- -D warnings` 无警告。
- 实跑：起二进制后 `curl /health` 返回 200（已验证：返回 `{"status":"ok",...}` + x-request-id 头）。
- `#![forbid(unsafe_code)]` 生效；注释 / 日志中文；无明文密钥入库（随机口令打印启动日志属 ADR-0010 明确设计，仅首启一次）。

## 6. 风险 / 待定

- 首启随机口令打印到日志是 ADR-0010 明确设计的唯一“密钥进日志”例外，仅首启一次；运维须妥善保管启动日志并尽快改密。
- 登录失败计数、会话等更完整的认证能力在后续批次落地，本批仅备口令哈希与首启引导。
