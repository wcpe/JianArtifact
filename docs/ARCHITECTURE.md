# 架构设计：JianArtifact

> 系统当前真貌（HOW）。始终原地更新到现状；结构 / 机制变了就改它。

## 1. 定位与边界

JianArtifact 是一个用 Rust 编写、打包为单一可执行二进制的轻量级多格式制品库管理器。它原生支持多种主流包格式，内置多用户认证、全局角色与每仓库 ACL，支持公开/私有仓库隔离与匿名访客，零外部运行时依赖。

是什么：

- 一个面向制品分发的服务端，对外暴露两类接口——供 Web 控制台与脚本使用的管理 API，以及供包管理器客户端（mvn / npm / docker / curl 等）按各自原生协议访问的格式 API。
- 一个把前端（React + Vite + TypeScript，UI 组件库 Mantine）在编译期嵌入二进制的自包含程序，启动后即可提供 Web 控制台与所有端点，无需独立的静态资源服务器。
- 元数据由嵌入式 SQLite 持有，制品本体（blob）落在本地文件系统。

不是什么：

- 不是依赖外部数据库或中间件的系统：不需要独立的 PostgreSQL/MySQL/Redis、消息队列、独立搜索引擎或容器编排。
- 不是反向代理或 TLS 终结器：TLS 终结由可选的外部反向代理承担，本程序不内建。
- 不是 P2/P3 范围内能力的承载者：S3 后端、OIDC/LDAP、审计日志、指标端点、速率限制、迁移工具、group/virtual 聚合仓库、GC/保留策略、备份恢复，以及七层防护增强（并发/连接控制、慢速攻击防护、异常检测与自动封禁、CC 挑战、WAF 规则引擎、监控告警）、使用分析数据面板、权限增强（用户组/团队、细粒度权限动作）等均不在当前形态内。
- 不是 L3/L4 体积型 DDoS 缓解设施：本程序只做应用层（L7）防护，体积型攻击交由前置反向代理 / CDN / WAF 承担。
- 不是对外遥测的上报方：使用分析数据落本地、不主动外发，不向外部遥测平台 phone-home。

外部边界：

- 上游制品仓库：proxy 类型仓库在缓存未命中时向上游拉取制品。
- 包管理器客户端：通过 Bearer Token 或 Basic Auth 访问格式端点。
- 可选反向代理：置于本程序之前做 TLS 终结。

## 2. 模块与依赖

系统由以下模块构成，各自职责单一，依赖方向单向且无环：

- `api`：axum 路由与中间件（认证、鉴权、请求 ID、统一错误处理）。HTTP 层保持轻薄，不写业务逻辑。
- `auth`：认证。处理本地用户名/密码、Bearer Token、Basic Auth；提供认证 provider 抽象（OIDC/LDAP 为 P2 实现，当前仅留接口边界，不落占位字段）。
- `authz`：授权。负责全局角色 + 每仓库可见性（public/private）+ 每仓库读写 ACL 的综合判定。P2 扩展用户组/团队授权与细粒度权限动作（read/write/delete/admin）。
- `repo`：仓库模型与生命周期（hosted/proxy 配置、可见性）。
- `format`：各格式处理器（maven/npm/docker/raw），经统一 trait 抽象注册。
- `proxy`：上游代理与缓存（拉取、落盘、单飞合并、上游失败回退）。
- `storage`：blob 存储抽象（本地文件系统；S3 为 P2）。
- `meta`：SQLite 元数据访问层（users / repositories / repo_acl / tokens / artifacts 索引）。
- `web`：React + Mantine 前端 + `rust-embed` 静态资源嵌入与服务。
- `config`：TOML 配置加载 + env 覆盖。
- `migrate`：P2 模块——Nexus OSS 迁移（在线 REST + 离线 blob store 双入口）。当前形态不实现。
- `protect`：P2 模块——七层（L7）防护中间件链：多维限流（IP/Token/用户/仓库）、并发/连接上限、慢速攻击超时、访问异常检测与自动封禁、IP 黑白名单、CC 挑战、可配置 WAF 规则引擎，并产出防护监控/告警。挂在 `api` 中间件链前段；L3/L4 体积型 DDoS 不在其内。当前形态不实现。
- `analytics`：P2 模块——使用分析：异步采集访问/下载事件并聚合落 `meta`（SQLite），供数据面板查询；数据本机内部、不外发。当前形态不实现。
- `vuln`：P2 模块——漏洞库离线镜像（定期下载 OSV 等公开漏洞数据到本机）+ 按制品坐标本地匹配，标记制品是否命中已知漏洞；坐标不逐包外发。Docker 镜像层 OS 扫描更重，留 P3。当前形态不实现。

依赖方向（单向，无环）：

```
api → (auth / authz / repo / format) → (proxy / storage / meta) → config
```

其中 `format` 依赖 `storage` / `meta` / `proxy`。`web` 模块通过 `api` 挂载，向用户提供 React 应用与静态资源。严禁反向依赖与环：上层不被下层反向依赖，`meta` 是元数据的唯一真源。换栈/换框架属于架构决策，须先走新 ADR。

## 3. 数据模型

元数据存于嵌入式 SQLite，是元数据的唯一真源；制品本体（blob）落在文件系统/对象存储，数据库仅存索引与 sha256。

SQLite 表（五张）：

- `users`：`id`, `username`, `password_hash`, `role`, `disabled`, `created_at`
- `tokens`：`id`, `user_id`, `name`, `token_hash`, `created_at`, `last_used_at`, `revoked`
- `repositories`：`id`, `name`, `format`, `type`（`hosted` | `proxy`）, `visibility`（`public` | `private`）, `upstream_url`, `upstream_auth_ref`, `created_at`
- `repo_acl`：`id`, `repo_id`, `user_id`, `permission`（`read` | `write`）
- `artifacts`：`id`, `repo_id`, `path`, `size`, `sha256`, `sha1`, `md5`, `sha512`, `content_type`, `cached`, `created_at`（多摘要并存；blob 寻址仍以 `sha256` 为准，sha1/md5 主要为客户端兼容）

关系：

- `tokens.user_id`、`repo_acl.user_id` 指向 `users.id`。
- `repo_acl.repo_id`、`artifacts.repo_id` 指向 `repositories.id`。
- `repo_acl` 记录某用户对某仓库的读或写授权；`artifacts` 记录某仓库下某路径制品的索引与校验和。

blob 文件系统布局：

- 制品本体保存在数据目录下的 blob 存储区（如 `./data/blobs`），运行期由配置中的数据目录决定其根位置。
- 数据库中的 `artifacts` 行通过 `sha256` 与 `path` 与文件系统中的 blob 关联，数据库本身不保存 blob 二进制内容。

敏感项不入 DB 明文：

- 密码以 argon2 哈希形式存于 `users.password_hash`，不存明文。
- API Token 以哈希形式存于 `tokens.token_hash`，不存明文。
- 上游凭据等敏感项不入 DB 明文：数据库仅在 `repositories.upstream_auth_ref` 中存引用，真值走配置/env（如 `config.toml` 或 `JIANARTIFACT_*` 环境变量）。

P2 规划（当前形态不建表、不在数据模型 / 契约中预留占位字段，仅记录演进方向）：

- 用户组/团队：`groups`、`user_groups`；`repo_acl` 的授权主体从用户扩展为用户或组，`permission` 从 `read` | `write` 扩展为 `read` | `write` | `delete` | `admin`。
- 七层防护：动态封禁状态落库（如 `ban_list`），限流阈值与 WAF 规则走配置（TOML），不与元数据混存。
- 使用分析：访问/下载聚合计数（如 `usage_stats`）与可选明细（如 `access_events`），仅本机内部、不外发。
- 漏洞库：本地镜像的漏洞公告（如 `vuln_advisories`，来源 OSV 等）与制品-漏洞匹配（如 `artifact_vulns`）；坐标级匹配、不外发。

P1 的新增能力不引入新表：制品删除与覆盖/不可变策略作用于既有 `artifacts` 与 blob；登录失败计数保存在进程内存（按账户 / IP），不落 DB；首个管理员写入既有 `users` 表。

## 4. 接口

接口分两层并各自承担一类用途；详细契约见 API.md，此处只给概览与定位。

- 管理 API：挂载于 `/api/v1/*`（涵盖 auth 登录/登出/刷新、当前用户 `me`、users、repositories（含制品浏览与删除）、acl、tokens、health），采用 REST + JSON 风格，统一分页与错误约定（详见 API.md），供 Web 控制台与脚本使用。
- 格式 API：各格式按其原生协议挂载（如 Maven、npm、Docker registry v2、Raw），路径中含仓库名以定位目标仓库；供包管理器客户端按原生协议直接访问。
- Web 控制台：`/` 提供 React 应用，`/assets/*` 提供静态资源；二者均由嵌入二进制的前端产物服务。
- 健康检查：管理 API 下提供健康检查端点。

## 5. 关键机制

- 认证中间件：识别请求携带的 Bearer（先按 JWT 会话校验，失败再按 API Token 哈希校验）、Basic Auth（secret 可为口令 argon2 校验或 API Token 哈希校验）或无凭据，解析出调用方身份（`AuthIdentity`：匿名 / 已认证）注入请求扩展；任何无效凭据回退匿名，禁用用户与已吊销 Token 即时失效。Web 会话以无状态 JWT（HS256）承载，放 `Authorization: Bearer` 头（不走 Cookie，天然规避 CSRF，见 ADR-0011）；JWT 签名密钥真源为数据目录下的 `.jwt_secret` 文件（无则生成 256 位高熵随机密钥，类 Unix 下收紧 0600），绝不入库、不进日志，受 `.gitignore` 的数据目录排除覆盖。API Token 为高熵随机串（`jna_` 前缀），仅签发时返回一次明文，DB 只存其 sha256 哈希，校验走定长比较。
- 鉴权中间件：按目标仓库 + 操作（读/写）综合判定——综合 public/private 可见性、全局角色、每仓库 ACL 三者；私有仓库对未授权（含匿名）一律拒绝（401/404）。
- 代理缓存与单飞：proxy 仓库缓存未命中时，从上游拉取 → 校验 → 落盘 → 写索引；同一制品的并发请求经单飞合并，避免对上游重复拉取；上游不可用时按策略回退。
- 流式 IO 先落盘再写索引：上传/下载走流式处理，大文件不整体载入内存；写入时先落 blob 并校验 sha256 通过，再写元数据索引，以避免产生孤儿索引。
- 配置 env 覆盖：运行期配置由 `figment` 分层加载——内置默认值 → 单个 TOML 文件 → 环境变量（前缀 `JIANARTIFACT_`，节名后首个下划线映射为嵌套分隔，如 `JIANARTIFACT_SERVER_PORT` → `server.port`），后者优先于 TOML 文件中的同名项。
- 七层防护中间件链（P2）：在 `api` 入口前段串接限流（IP/Token/用户/仓库 多维）、并发/连接上限、慢速攻击超时、异常检测与自动封禁、IP 黑白名单、CC 挑战与 WAF 规则匹配；命中即在进入业务前阻断，并产出监控/告警。仅应用层（L7）；L3/L4 体积型攻击交前置设施。
- 使用分析异步采集（P2）：访问/下载事件经异步通道采集与聚合，不阻塞主请求路径，聚合结果落 SQLite 供数据面板查询；数据本机内部、不主动外发。
- 首个管理员引导：首次启动检测到无任何用户时，从环境变量（`JIANARTIFACT_ADMIN_USERNAME` / `JIANARTIFACT_ADMIN_PASSWORD`）创建首个管理员；未提供则生成随机口令、打印到启动日志（仅首次），要求登录后改密。系统不开放公开自助注册，用户由管理员创建。
- 登录防护：对登录失败按账户 / IP 计数（进程内存，不落 DB），超过阈值在时间窗内临时锁定 / 限流，抵御暴力破解（P2 七层防护提供更强的异常检测与自动封禁）。
- 写入语义与覆盖策略：制品写入按格式应用覆盖 / 不可变规则（如 Maven release 不可覆盖、snapshot 可覆盖）；删除对 hosted 删本体与索引、对 proxy 删缓存。
- 列表分页：管理 API 列表端点统一 `offset` / `limit` 分页与过滤，返回 `{ items, total, offset, limit, has_more }` 结构。
- 多校验和：制品写入时同时计算 sha256 / sha1 / md5 / sha512 并入索引；各格式按需提供对应 sidecar（如 Maven 的 `.sha1` / `.md5` / `.sha256`），下载方可据以校验。
- 跨仓库搜索：在 `meta` 的 `artifacts` 索引上做关键字 / 坐标检索，结果按调用方身份过滤——只返回其有读权限的仓库制品，绝不泄露无权私有仓库内容。
- 使用方式片段：`format` 按格式与 `public_base_url` 生成获取与接入片段（Maven 依赖、npm / docker 命令、Raw URL/curl 及仓库接入配置），供详情页展示。
- Web 控制台嵌入与 SPA 回退：`web` 模块经 `rust-embed` 在编译期把 `frontend/dist` 打进二进制；在 `api` 路由链中于 API / 格式 / Docker / 健康检查之后接入——`/assets/{*path}` 提供静态资源（按扩展名推断 Content-Type），其余未匹配 GET 经 `fallback` 回退 `index.html` 交前端客户端路由。前端可深链路由均为单段路径、详情用查询参数，避免与格式 catch-all `/{repo}/{*path}` 冲突。干净检出下 `frontend/dist` 仅含占位、无 `index.html` 时返回 503 提示页，保证后端可独立编译 / 测试（构建顺序：先 `pnpm -C frontend build` 再 `cargo build`）。
- 漏洞离线匹配（P2）：`vuln` 定期下载漏洞库公开数据到本机，按制品坐标本地匹配并标记，不把坐标逐包外发。

## 6. 部署

- 运行形态：单一可执行二进制 + 一个 `config.toml` 配置文件 + 一个数据目录（存放 SQLite 文件与 blob）。前端已在编译期嵌入二进制，无独立静态资源服务器。
- 依赖：无需独立数据库或中间件，零外部运行时依赖。
- TLS：可选地将本程序置于反向代理之后，由反向代理做 TLS 终结。
- 跨平台：支持 Linux/Windows/macOS，覆盖 x86_64 与 arm64。

## 7. 关键裁决与不做项

影响架构的重大取舍，详见对应 ADR：

- ADR-0001 技术栈与打包：后端 Rust + axum + tokio，前端 React + Vite + TypeScript（UI 组件库 Mantine）经 rust-embed 嵌入，单一二进制（strip + LTO + `panic = "abort"`、`forbid(unsafe)`）。
- ADR-0002 元数据存储：嵌入式 SQLite 存元数据，blob 存文件系统。
- ADR-0003 认证机制：本地用户名/密码（argon2）+ Bearer Token + Basic Auth + Web 会话/JWT；预留认证 provider 抽象。
- ADR-0004 授权模型：全局角色（Admin/User）+ 每仓库可见性（public/private）+ 每仓库读写 ACL；匿名仅读 public。
- ADR-0005 仓库类型：每格式支持 hosted + proxy（含缓存）。
- ADR-0006 Nexus 迁移：在线 REST API + 离线 blob store 双入口迁移框架，随已实现格式逐期扩展。
- ADR-0007 权限粒度与用户组：在授权模型上扩展用户组/团队与细粒度权限动作（read/write/delete/admin），扩展（不取代）ADR-0004。P2。
- ADR-0008 七层防护：应用层防护套件（多维限流、并发/连接控制、慢速攻击防护、异常检测与自动封禁、黑白名单、CC 挑战、WAF 规则引擎）+ 监控告警；L3/L4 交前置设施。P2。
- ADR-0009 内部使用分析：访问/下载统计与数据面板，统计落本地、不外发、不 phone-home。P2。
- ADR-0010 首启管理员引导：首次启动从环境变量或随机口令创建首个管理员，不开放公开自助注册。
- ADR-0011 会话与 JWT 生命周期：会话 / JWT 的 TTL、刷新端点与 CSRF 防护策略。
- ADR-0012 漏洞库离线对接：本地镜像 OSV 等公开漏洞数据 + 坐标级本地匹配，不逐包外发；Docker 镜像层扫描留 P3。P2。

当前不做项：

- group/virtual 聚合仓库：P3，当前形态不实现，也不在数据模型/契约中预留占位字段。
- OIDC/LDAP 认证集成：P2，当前仅预留 provider 接口边界，不落占位字段。
- S3 兼容对象存储后端：P2，当前 blob 存储仅本地文件系统。
- 七层防护增强与监控告警：P2，当前不实现；L3/L4 体积型 DDoS 始终交前置反向代理 / CDN / WAF。
- 使用分析数据面板：P2，当前不实现；统计数据本机内部、不外发。
- 权限增强（用户组/团队、细粒度权限动作）：P2，当前授权仅全局角色 + 每仓库读写 ACL，不预留占位字段。
- 漏洞库对接（离线镜像 + 坐标级匹配）：P2，当前不实现、不预留漏洞相关占位字段；Docker 镜像层 OS 漏洞扫描更重，留 P3。
