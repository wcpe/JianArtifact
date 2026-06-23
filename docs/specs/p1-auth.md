# 功能规格：L1 认证与身份层

> 状态：开发中　·　关联 PRD：FR-01 / FR-02 / FR-03 / FR-04 / FR-05 / FR-63 / FR-65　·　分支：feature/p1-auth

## 1. 背景与目标

在 L0 地基（配置、SQLite 元数据、blob 存储、首启引导、健康检查）之上，立起第一期的认证与身份层：用户能登录拿到 Web 会话、自助管理供 CLI 使用的 API Token，CLI / 包管理器能用 Bearer 或 Basic 鉴权，管理员能管理用户与角色，并对登录暴力破解有基本防护。这是后续仓库授权（authz）、格式端点写权限校验赖以判定“调用方是谁”的基础。属阶段 P1。

本规格只覆盖**认证与身份解析**（“是谁”），**不含**对仓库的读写鉴权判定 / ACL 强制（属 Batch 2 的 `authz` 与 `repo`）、任何格式处理器、代理缓存、Web UI——提前实现即越界（违反 scope-discipline）。用户管理端点的“仅管理员”角色门属本批（简单全局角色门），但“对某仓库的读写鉴权”不在本批。

## 2. 需求（要什么）

- 范围内：
  - **FR-01**：本地用户名 + 口令登录（argon2 校验），签发有限有效期 JWT 会话。
  - **FR-02**：API Token 签发 / 列表 / 吊销，仅签发时返回一次明文，DB 仅存 sha256 哈希。
  - **FR-03**：Basic Auth 鉴权，secret 可为口令（argon2）或 API Token（哈希），兼容包管理器 CLI。
  - **FR-04**：全局角色 Admin / User。
  - **FR-05**：管理员管理用户（新增 / 查询 / 改角色与禁用 / 删除）。
  - **FR-63**：会话生命周期——JWT TTL、刷新端点、当前用户 `GET /me`。
  - **FR-65**：登录暴力破解防护——按 (用户名, 来源 IP) 进程内存计数，达阈值锁定 / 限流，成功或过期清零。
  - 身份解析中间件：统一识别 Bearer(JWT / API Token) / Basic / 匿名，注入 `AuthIdentity` 供后续鉴权使用。
- 不做（范围外）：仓库 CRUD、对仓库的授权判定 / ACL 强制、私有仓库 404 语义、格式处理器、proxy 缓存、Web 控制台、列表分页 / 搜索（这些是后续 P1 批次）；OIDC / LDAP（P2）；JWT 服务端 denylist（可选增强，本批不做，无状态登出由客户端丢弃令牌）。

## 3. 设计（怎么做）

### 模块结构（在既有 `auth` / `meta` / `api` 上扩展）

- `auth/jwt.rs`：`JwtSigner`（HS256）。密钥真源为 `data_dir/.jwt_secret`，无则生成 256 位随机密钥写入（类 Unix 下收紧 0600），有则复用；密钥绝不入库、不进日志。`verify` 显式要求 HS256、leeway=0 使过期判定精确。
- `auth/token.rs`：API Token 生成（`jna_` 前缀 + 高熵随机体）、`hash_api_token`（sha256，Token 本身已高熵故无需 argon2 慢哈希）、`verify_api_token`（定长比较，避免计时侧信道）。
- `auth/basic.rs`：解析 `Authorization: Basic base64(user:secret)`（只在首个冒号分割，口令可含冒号）；`strip_scheme_prefix` 大小写不敏感剥离方案前缀。
- `auth/lockout.rs`：`LoginGuard`，`Mutex<HashMap<(用户名, IP), 失败状态>>`，`check` / `record_failure` / `record_success`；锁定到期自动恢复，不同用户名 / IP 互不影响。
- `auth/mod.rs`：新增 `AuthIdentity`（Anonymous / Authenticated{user_id, username, role}）与便捷判定（is_admin 等）；re-export 各子模块。
- `meta`：用户增查改删（`get_user_by_id` / `list_users` / `update_user` / `delete_user`）、Token 增查改（`create_token` / `get_token_identity_by_hash` / `list_tokens_by_user` / `get_token_by_id` / `revoke_token` / `touch_token_last_used`）、`Role::from_db_str`（未知值降级 User）。不新增表（沿用既有 users / tokens）。
- `api`：`AppState` 增 `jwt: JwtSigner` 与 `login_guard: Arc<LoginGuard>`；`ApiError` 扩为 BadRequest/Unauthorized/Forbidden/NotFound/Conflict/TooManyRequests/AccountDisabled/Internal；`Identity` 提取器（从扩展取注入身份，`require_authenticated` / `require_admin`）、`ClientIp` 提取器（从 ConnectInfo 取来源 IP）；`identity.rs` 身份解析中间件（四通道，失败回退匿名，禁用 / 吊销即时失效）；`auth_routes.rs`（login/logout/refresh/me）、`users.rs`（仅管理员 CRUD）、`tokens.rs`（本人 Token 增列删）。
- `main.rs`：构造 JwtSigner（按 data_dir）与 LoginGuard（按 config.auth），并用 `into_make_service_with_connect_info::<SocketAddr>()` 注入连接信息供登录防护按 IP 计数。

### 端点（对齐 docs/API.md）

`POST /api/v1/auth/login`（401 口令错 / 403 禁用 / 429 限流）、`POST /auth/logout`（已认证 200）、`POST /auth/refresh`（401 过期）、`GET /me`（401 未认证）、`GET/POST /users` 与 `GET/PATCH/DELETE /users/{id}`（仅 Admin，非管理员 403）、`POST /tokens`（仅此一次返回明文）、`GET /tokens`（不回显明文 / 哈希）、`DELETE /tokens/{id}`（非本人 403）。

### 错误语义

缺失 / 无效凭据 → 401；已认证但非管理员访问管理端点 → 403；账户禁用登录 → 403；登录限流 → 429。私有仓库 404 语义属 Batch 2，本批不涉及。

### 对齐的 ADR

- ADR-0003（认证机制）：本地口令 argon2 + Bearer Token 哈希 + Basic + Web 会话 JWT，provider 抽象不落占位字段（OIDC/LDAP 属 P2）。
- ADR-0011（会话与 JWT 生命周期）：有限 TTL + 刷新端点 + `/me`；JWT 放 `Authorization: Bearer` 头（不走 Cookie），天然规避 CSRF；会话与 API Token 相互独立（Token 不过期，仅可吊销）。
- ADR-0010：首启引导已在 L0 落地，本批不改。

本批未引入与既有 ADR 冲突的新决策，故不新增 ADR；JWT 密钥文件位置（`data_dir/.jwt_secret`）与 HS256 / RustCrypto provider 为实现选择，记于本规格与 ARCHITECTURE §5。

### 本批新增依赖与理由

| 依赖 | 理由 |
|---|---|
| jsonwebtoken（default-features=false, features=["rust_crypto"]） | Web 会话 JWT（HS256，ADR-0011）。关默认特性避开 aws-lc-rs / ring 原生加密后端，启用 RustCrypto 纯 Rust provider，保单一二进制零外部运行时依赖 |
| base64 | Basic Auth `base64(user:secret)` 解码（ADR-0003） |
| subtle | API Token 哈希定长比较，避免计时侧信道 |

> 说明：三者均为纯 Rust 轻量件，非清单外重型件（无外部 DB / MQ / Redis）。`sha2` 已在 L0 引入，本批复用其算 Token 哈希；登录防护用 std `Mutex` + `HashMap`，不引额外并发库。

## 4. 任务拆分

- [x] meta：用户增查改删 + Token 增查改 + `Role::from_db_str`，配套单测。
- [x] auth/jwt：JwtSigner（密钥文件加载 / 生成、签发 / 校验），配套单测。
- [x] auth/token：生成 / 哈希 / 定长比较，配套单测。
- [x] auth/basic：Basic 解析 + 方案前缀剥离，配套单测。
- [x] auth/lockout：LoginGuard 计数 / 锁定 / 恢复，配套单测。
- [x] auth/mod：AuthIdentity 与 re-export。
- [x] api：AppState / ApiError 扩展、Identity / ClientIp 提取器、身份解析中间件（四通道单测）。
- [x] api/auth_routes、users、tokens：端点 handler（薄，逻辑下沉）。
- [x] main：构造 JwtSigner / LoginGuard，serve 带 ConnectInfo。
- [x] HTTP 集成测试（tests/auth_api.rs）：登录 / 锁定 / me / 刷新 / Token / Basic / 用户 CRUD admin-only。
- [x] 文档同步：本规格、PRD 状态（FR-01/02/03/04/05/63/65 改开发中）、API 核对、ARCHITECTURE（补 JWT 密钥 / provider）、CHANGELOG。

## 5. 验收标准

- `cargo build` 成功。
- `cargo test` 全绿（60 lib + 22 集成 = 82 通过），覆盖：登录成功 / 口令错 401 / 不存在用户 401 / 禁用 403 / 缺参 400；连续失败触发 429 与到期恢复；JWT 过期被拒、刷新换发、登出、/me；Token 签发→Bearer 使用→吊销后拒绝、列表不回显明文 / 哈希、吊销他人 403；Basic（口令 / Token 两通道）、错误口令回退匿名 401；身份四通道（Bearer-JWT / Bearer-Token / Basic / 匿名）；用户 CRUD admin-only（非管理员 403、匿名 401、重名 409、非法角色 400）。
- `cargo clippy --all-targets -- -D warnings` 无警告。
- 实跑：起二进制后 `curl` 跑通 登录→拿 JWT→GET /me 200；并验证口令错 401 / 无凭据 401。
- `#![forbid(unsafe_code)]` 生效；注释 / 日志中文分级；JWT 密钥（`data_dir/.jwt_secret`，受 `.gitignore` 的 `/data/` 排除）、Token 明文 / 哈希、口令均不入库、不进日志 / 错误响应。

## 6. 风险 / 待定

- 登录防护本批按连接 IP 计数；`X-Forwarded-For` 仅在可信前置代理时才可采信，留待 P2 七层防护增强（lockout 模块已注明）。
- 无状态 JWT 下登出由客户端丢弃令牌，服务端不维护 denylist；如需“吊销未过期会话即时失效”属可选增强，后续按需评估（本批仅靠 TTL + 禁用 / 删除用户即时阻断身份解析）。
- 失败计数为进程内存，多实例部署时各自独立——当前为单实例形态，符合架构；横向扩展属后续阶段考量。
