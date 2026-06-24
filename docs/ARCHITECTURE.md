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
- 不是 P2/P3 范围内能力的承载者：OIDC/LDAP、审计日志、指标端点、速率限制、迁移工具、group/virtual 聚合仓库、GC/保留策略、备份恢复，以及七层防护增强（并发/连接控制、慢速攻击防护、异常检测与自动封禁、CC 挑战、WAF 规则引擎、监控告警）、使用分析数据面板、权限增强（用户组/团队、细粒度权限动作）等均不在当前形态内。（S3 兼容对象存储后端为已实现的可选 opt-in blob 后端，默认仍走本地文件系统，见 ADR-0014。）
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
- `authz`：授权。负责全局角色 + 每仓库可见性（public/private）+ 每仓库 ACL 的综合判定；ACL 动作细化为四级 `read` / `write` / `delete` / `admin`，高动作蕴含低动作（admin ⊇ delete ⊇ write ⊇ read）。用户对某仓库的有效权限为其直接 ACL ∪ 其所属各组的组 ACL（取并集后按动作蕴含判定，FR-49 / ADR-0007）；既有直接-ACL 判定结论不变。
- `repo`：仓库模型与生命周期（hosted/proxy 配置、可见性）。
- `format`：各格式处理器（maven/npm/docker/go/raw/pypi），经统一 trait 抽象注册。
- `proxy`：上游代理与缓存（拉取、落盘、单飞合并、上游失败回退）。
- `storage`：blob 存储抽象（`BlobStore` trait；默认本地文件系统 `LocalFsStore`，可选 opt-in 的 S3 兼容对象存储 `S3Store`，经 `BlobBackend` 枚举运行期分发，见 ADR-0014）。
- `meta`：SQLite 元数据访问层（users / repositories / repo_acl / tokens / artifacts 索引）。
- `web`：React + Mantine 前端 + `rust-embed` 静态资源嵌入与服务。
- `config`：TOML 配置加载 + env 覆盖。
- `migrate`：P2 模块——Nexus OSS 迁移（在线 REST + 离线 blob store 双入口）。当前已实现**在线 REST API 入口**（FR-36）的发现 / 预览：连接在线 Nexus、经其 `service/rest/v1/repositories` 枚举可迁移仓库列表与基本元数据（名 / 格式 / 类型 / 上游地址）；REST 交互经 `NexusClient` trait 抽象、生产实现复用 reqwest 纯 rustls，访问凭据真源 env（`JIANARTIFACT_MIGRATE_<NAME>_USERNAME/PASSWORD`，DB 仅存引用、不入库不进日志）。同时已实现**离线 blob store 入口**（FR-37）的发现 / 预览：当源 Nexus 已下线、只剩其文件型 blob store 目录时，从给定本地目录解析磁盘布局（`content/` 分片目录 + 每个 blob 一份 `.properties` 元数据），按所属仓库枚举可迁移 blob 及基本元数据（坐标 / sha1 / 大小）；纯文件系统读取、解析逻辑为无副作用纯函数，软删 / 损坏 / 缺字段的元数据容错跳过，仅解析 `.properties`、不读取也不搬运 blob 本体。实际制品搬运（FR-38/39）当前形态不实现。
- `protect`：P2 模块——七层（L7）防护中间件链：多维限流（IP/Token/用户/仓库）、并发/连接上限、慢速攻击超时、访问异常检测与自动封禁、IP 黑白名单、CC 挑战、可配置 WAF 规则引擎，并产出防护监控/告警。挂在 `api` 中间件链前段；L3/L4 体积型 DDoS 不在其内。当前形态不实现。
- `analytics`：P2 模块——使用分析：异步采集访问/下载事件并聚合落 `meta`（SQLite），供数据面板查询；数据本机内部、不外发。当前形态不实现。
- `vuln`：P2 模块——漏洞库离线镜像（定期下载 OSV 等公开漏洞数据到本机并落本地库，FR-70）与按制品坐标本地匹配标记（FR-71）均已实现。下载经 `MirrorSource` trait 抽象（生产 `HttpMirrorSource` 走 reqwest 纯 rustls，流式落盘），解压解析每条 OSV 公告后经 `meta` 幂等落库，并支持周期刷新（默认关闭，由配置开启）。坐标级匹配为无副作用纯函数：`format` 各处理器经 `vuln_coordinate` 从制品路径反解生态坐标 `(ecosystem, package, version)`（无标准坐标的 Raw / Docker 不产出、不参与），`meta` 按 `(ecosystem, package)` 查候选受影响行，`vuln::select_hits` 据 OSV `affected` 范围语义（`introduced` 起含、`fixed` 止不含、`last_affected` 止含；另含显式 `versions` 列表）判定命中——全程只比对本机已镜像数据，坐标不外发。Docker 镜像层 OS 扫描更重，留 P3。依赖 `meta` / `config`，方向单向无环。

依赖方向（单向，无环）：

```
api → (auth / authz / repo / format) → (proxy / storage / meta) → config
```

其中 `format` 依赖 `storage` / `meta` / `proxy`；`vuln`（P2）作为后台编排模块依赖 `meta` / `config`，由二进制入口启动其周期刷新任务，落库经 `meta` 唯一入口、不反向依赖上层。`web` 模块通过 `api` 挂载，向用户提供 React 应用与静态资源。严禁反向依赖与环：上层不被下层反向依赖，`meta` 是元数据的唯一真源。换栈/换框架属于架构决策，须先走新 ADR。

## 3. 数据模型

元数据存于嵌入式 SQLite，是元数据的唯一真源；制品本体（blob）落在文件系统/对象存储，数据库仅存索引与 sha256。

SQLite 核心表（P1，五张）：

- `users`：`id`, `username`, `password_hash`, `role`, `disabled`, `created_at`
- `tokens`：`id`, `user_id`, `name`, `token_hash`, `created_at`, `last_used_at`, `revoked`
- `repositories`：`id`, `name`, `format`, `type`（`hosted` | `proxy`）, `visibility`（`public` | `private`）, `upstream_url`, `upstream_auth_ref`, `created_at`
- `repo_acl`：`id`, `repo_id`, `user_id`, `permission`（`read` | `write` | `delete` | `admin`，四级动作）
- `artifacts`：`id`, `repo_id`, `path`, `size`, `sha256`, `sha1`, `md5`, `sha512`, `content_type`, `cached`, `created_at`（多摘要并存；blob 寻址仍以 `sha256` 为准，sha1/md5 主要为客户端兼容）
- `audit_log`（P2 审计日志，FR-31 / ADR-0015）：`id`（自增）, `ts`, `actor`, `actor_kind`（`session` | `token` | `basic` | `anonymous`）, `request_id`, `source_ip`, `action`（如 `login` / `token.issue` / `repo.create` / `acl.update` / `artifact.upload` 等）, `target_repo`, `target`, `result`（`success` | `denied` | `error`）, `detail`。只记元数据级安全 / 管理事件，不记请求体与制品内容；凭据 / 密钥绝不入此表。

使用分析表（P2，FR-57 引入；数据面板 FR-58 尚未实现）：

- `usage_stats`（访问 / 下载聚合计数）：`repo_name`, `repo_path`（制品仓库内路径，仓库级聚合时为空串）, `action`（`access` | `download`）, `count`, `last_at`；主键 `(repo_name, repo_path, action)`，采集走 UPSERT 累加，并发下计数准确。是长期统计真源。
- `usage_events`（可选明细）：`id`（自增）, `ts`, `repo_name`, `repo_path`, `action`, `actor`（用户名或 `anonymous`，不记凭据）, `source_ip`（可空）。仅在配置 `[observability.usage] detail_enabled = true` 时写入，行数由后台按 `max_detail_rows` 兜底裁剪、删最旧，避免撑爆 SQLite。数据本机内部、默认不外发。
用户组/团队表（P2，FR-49 / ADR-0007 引入）：

- `groups`：`id`, `name`（唯一）, `created_at`（用户组/团队）
- `user_groups`：`group_id`, `user_id`（组成员关系，复合主键；外键指向 `groups.id` / `users.id`，`ON DELETE CASCADE`）
- `repo_group_acl`：`id`, `repo_id`, `group_id`, `permission`（`read` | `write` | `delete` | `admin`，四级动作；对组授予仓库 ACL，结构与 `repo_acl` 对齐但主体为组；外键指向 `repositories.id` / `groups.id`，`ON DELETE CASCADE`）

漏洞库离线镜像表（P2，FR-70 引入；FR-71 坐标级匹配复用同表、不新增制品-漏洞缓存表）：

- `vuln_advisories`：`id`, `source`, `summary`, `details`, `severity`, `modified`, `published`, `created_at`（一条公开漏洞公告一行，来源如 OSV）
- `vuln_advisory_affected`：`id`, `advisory_id`, `ecosystem`, `package`, `ranges`, `versions`（公告受影响坐标逐包展开，`ranges` / `versions` 以原始 JSON 文本保真存储；FR-71 经 `(ecosystem, package)` 索引查候选行、在查询时即时按版本范围匹配，不落制品-漏洞缓存表；外键 `advisory_id` 指向 `vuln_advisories.id`，`ON DELETE CASCADE`）
- `vuln_mirror_state`：`source`, `ecosystem`, `last_refreshed`, `advisory_count`（每来源每生态最近一次成功刷新状态，主键 `(source, ecosystem)`，支持幂等刷新与运维观察）

关系：

- `tokens.user_id`、`repo_acl.user_id`、`user_groups.user_id` 指向 `users.id`。
- `repo_acl.repo_id`、`artifacts.repo_id`、`repo_group_acl.repo_id` 指向 `repositories.id`。
- `user_groups.group_id`、`repo_group_acl.group_id` 指向 `groups.id`。
- `repo_acl` 记录某用户对某仓库的动作授权（`read` / `write` / `delete` / `admin`，高动作蕴含低动作）；`repo_group_acl` 记录某组对某仓库的同类动作授权，组成员经 `user_groups` 继承该授权；`artifacts` 记录某仓库下某路径制品的索引与校验和。

blob 文件系统布局：

- 制品本体保存在数据目录下的 blob 存储区（如 `./data/blobs`），运行期由配置中的数据目录决定其根位置。
- 数据库中的 `artifacts` 行通过 `sha256` 与 `path` 与文件系统中的 blob 关联，数据库本身不保存 blob 二进制内容。
- 制品本体按 `sha256` 内容寻址、前两位分桶（`{sha256[0..2]}/{sha256[2..]}`）。启用可选 S3 后端时，对象 key 沿用同一内容寻址布局（再叠加配置的 `prefix`），SQLite 元数据语义不变、仍为唯一真源（见 ADR-0014）。

敏感项不入 DB 明文：

- 密码以 argon2 哈希形式存于 `users.password_hash`，不存明文。
- API Token 以哈希形式存于 `tokens.token_hash`，不存明文。
- 上游凭据等敏感项不入 DB 明文：数据库仅在 `repositories.upstream_auth_ref` 中存引用，真值走配置/env（如 `config.toml` 或 `JIANARTIFACT_*` 环境变量）。

P2 规划（当前形态不建表、不在数据模型 / 契约中预留占位字段，仅记录演进方向）：

- 七层防护：动态封禁状态落库（如 `ban_list`），限流阈值与 WAF 规则走配置（TOML），不与元数据混存。
- 使用分析：访问/下载聚合计数（`usage_stats`）与可选明细（`usage_events`，FR-57 已落表，见上）已落地；数据面板展示（FR-58）尚未实现。仅本机内部、不外发。
- 漏洞库：离线镜像的漏洞公告（`vuln_advisories` / `vuln_advisory_affected` / `vuln_mirror_state`，FR-70 已落表，见上）已落地；制品-漏洞匹配（FR-71）在制品详情查询时即时据坐标比对受影响坐标表得出，不落 `artifact_vulns` 等缓存表，坐标级匹配、不外发。

P1 的新增能力不引入新表：制品删除与覆盖/不可变策略作用于既有 `artifacts` 与 blob；登录失败计数保存在进程内存（按账户 / IP），不落 DB；首个管理员写入既有 `users` 表。

## 4. 接口

接口分两层并各自承担一类用途；详细契约见 API.md，此处只给概览与定位。

- 管理 API：挂载于 `/api/v1/*`（涵盖 auth 登录/登出/刷新、当前用户 `me`、users、repositories（含制品浏览与删除）、acl、tokens、search、audit（仅 Admin，P2）、health），采用 REST + JSON 风格，统一分页与错误约定（详见 API.md），供 Web 控制台与脚本使用。
- 格式 API：各格式按其原生协议挂载（如 Maven、npm、Docker registry v2、Go GOPROXY、Raw、PyPI Simple Repository API），路径中含仓库名以定位目标仓库；供包管理器客户端按原生协议直接访问。
- Web 控制台：`/` 提供 React 应用，`/assets/*` 提供静态资源；二者均由嵌入二进制的前端产物服务。
- 健康检查：管理 API 下提供健康检查端点。

## 5. 关键机制

- 认证中间件：识别请求携带的 Bearer（先按 JWT 会话校验，失败再按 API Token 哈希校验）、Basic Auth（secret 可为口令 argon2 校验或 API Token 哈希校验）、无 scheme 前缀的裸 Token（Cargo registry 客户端约定，按 API Token 哈希校验）或无凭据，解析出调用方身份（`AuthIdentity`：匿名 / 已认证）注入请求扩展；任何无效凭据回退匿名，禁用用户与已吊销 Token 即时失效。Web 会话以无状态 JWT（HS256）承载，放 `Authorization: Bearer` 头（不走 Cookie，天然规避 CSRF，见 ADR-0011）；JWT 签名密钥真源为数据目录下的 `.jwt_secret` 文件（无则生成 256 位高熵随机密钥，类 Unix 下收紧 0600），绝不入库、不进日志，受 `.gitignore` 的数据目录排除覆盖。API Token 为高熵随机串（`jna_` 前缀），仅签发时返回一次明文，DB 只存其 sha256 哈希，校验走定长比较。
- 鉴权中间件：按目标仓库 + 操作（读/写）综合判定——综合 public/private 可见性、全局角色、每仓库 ACL 三者；私有仓库对未授权（含匿名）一律拒绝（401/404）。
- 代理缓存与单飞：proxy 仓库缓存未命中时，从上游拉取 → 校验 → 落盘 → 写索引；同一制品的并发请求经单飞合并，避免对上游重复拉取；上游不可用时按策略回退。
- 流式 IO 先落盘再写索引：上传/下载走流式处理，大文件不整体载入内存；写入时先落 blob 并校验 sha256 通过，再写元数据索引，以避免产生孤儿索引。
- 配置 env 覆盖：运行期配置由 `figment` 分层加载——内置默认值 → 单个 TOML 文件 → 环境变量（前缀 `JIANARTIFACT_`，节名后首个下划线映射为嵌套分隔，如 `JIANARTIFACT_SERVER_PORT` → `server.port`），后者优先于 TOML 文件中的同名项。
- 七层防护中间件链（P2）：在 `api` 入口前段串接限流（IP/Token/用户/仓库 多维）、并发/连接上限、慢速攻击超时、异常检测与自动封禁、IP 黑白名单、CC 挑战与 WAF 规则匹配；命中即在进入业务前阻断，并产出监控/告警。仅应用层（L7）；L3/L4 体积型攻击交前置设施。
- 基础速率限制（P2，FR-33 / ADR-0008）：上述七层防护链中**已落地的基础子集**。`api` 路由链中一个单一职责的限流中间件置于身份解析之内（更靠近 handler，需读已注入的身份），按 **IP 维度** 与 **身份维度（用户 id，含其所有 Token / 会话）** 用进程内**固定时间窗计数**判定，任一维度单窗超阈值即在进入业务前返回 `429 Too Many Requests`（带 `Retry-After`）。热路径只取一次 `Mutex` 做整型自增与窗口比较（临界区内无 IO / 无格式化），窗口表过期键在加锁期间按表大小阈值顺带清理、防无界增长。来源 IP 取连接级 `ConnectInfo`（与登录防护一致），**不采信 `X-Forwarded-For` 等可伪造头**——伪造来源 IP 不绕过；轮换 IP 的同一主体仍受身份维度阈值约束。阈值 / 窗口经 `[protection.rate_limit]` 配置，**默认关闭、阈值保守**（不误杀正常包管理器批量拉取），配置热替换下个请求即按新值判定。本批仅基础 IP / 身份限流；多维（用户 / 仓库）限流与并发/连接上限属 FR-51、慢速 / 封禁 / CC / WAF / 告警属 FR-52~56，均不在本批。
- 审计日志（P2，FR-31 / ADR-0015）：`api` 路由链中一个单一职责的审计中间件置于身份解析之内（更靠近 handler），运行 handler 后按"方法 + 路径 + 响应状态"归类**精选**的写 / 管理 / 授权拒绝事件（用户 / Token / 仓库 / ACL 管理、制品上传 / 删除，及私有越权 `denied`），普通匿名 public 读取不逐条入审计（交指标计数）；登录事件因需记被尝试用户名，由登录 handler 显式发事件。事件经进程内有界 channel 投递给独立写入任务**批量落 `audit_log` 表**（经 `meta`，不绕过它）；主路径只做一次非阻塞 `enqueue`，**采集 / 写入失败只记 WARN、不影响业务**，channel 满则丢弃 + 计数 + WARN（不反压主路径）。后台轮转任务按 `observability.audit.retention_days` 删旧 + `observability.audit.max_rows` 行数兜底。审计仅 Admin 可经 `GET /api/v1/audit` 分页查询；密码 / Token / JWT / 上游凭据一律不入审计（`actor` 只记用户名）。数据本机内部、默认不外发（沿用 ADR-0009 基调）。
- 使用分析异步采集（P2，FR-57 / ADR-0009）：格式 GET 下载（读授权通过的制品 GET）记 `download`、制品详情查看记 `access`；事件经进程内有界 channel 投递给独立写入任务**批量聚合落 `usage_stats`**（经 `meta`，UPSERT 累加，并发下计数准确），主路径只做一次非阻塞 `record`，**采集 / 写入失败只记 WARN、不影响业务**，channel 满则丢弃 + 计数 + WARN（不反压主路径）。明细（`usage_events`）默认关闭，开启后由后台按 `observability.usage.max_detail_rows` 行数兜底裁剪。数据本机内部、**默认不主动外发、不向外部遥测 phone-home**；任何外部导出默认关闭（本批不做导出）。聚合查询入口供数据面板（FR-58）使用，本批不做面板 UI。
- Prometheus 指标端点（P2，FR-32 / ADR-0015）：用 `metrics` facade + `metrics-exporter-prometheus` 进程内 recorder（**pull 模型**，不引外部时序库、不 push / remote-write），启动时若 `observability.metrics.enabled=true` 则安装一次全局 recorder，句柄随 `AppState` 共享。`api` 路由链中一个单一职责的指标中间件置于最内层（最贴近 handler），在请求热路径只做**无锁原子观测**——HTTP 维度（`method` / `status_class` / `format`）计数与延迟直方图、上传 / 下载字节、并发上传 gauge；`proxy` 回源边界（`format` 层）埋点缓存命中 / 未命中、上游回源耗时 / 失败。所有标签取**有界枚举值**，**严禁**以仓库名 / 路径 / 用户名 / 制品坐标作标签（基数纪律，见 ADR-0015）；指标名与标签集中在叶子模块 `metrics_keys`（不依赖业务层，供 `api` 与 `format` 共享，避免魔法字符串散落、防跨层依赖）。渲染仅在 `GET /metrics` 被抓取时发生；端点默认要求认证且仅 Admin，`observability.metrics.allow_anonymous=true` 时免认证抓取（须限内网 / 反代后），`enabled=false` 时返回 404。指标本机内部、不主动外发（沿用 ADR-0009 基调）。
- 使用分析异步采集（P2）：访问/下载事件经异步通道采集与聚合，不阻塞主请求路径，聚合结果落 SQLite 供数据面板查询；数据本机内部、不主动外发。
- 首个管理员引导：首次启动检测到无任何用户时，从环境变量（`JIANARTIFACT_ADMIN_USERNAME` / `JIANARTIFACT_ADMIN_PASSWORD`）创建首个管理员；未提供则生成随机口令、打印到启动日志（仅首次），要求登录后改密。系统不开放公开自助注册，用户由管理员创建。
- 登录防护：对登录失败按账户 / IP 计数（进程内存，不落 DB），超过阈值在时间窗内临时锁定 / 限流，抵御暴力破解（P2 七层防护提供更强的异常检测与自动封禁）。
- 写入语义与覆盖策略：制品写入按格式应用覆盖 / 不可变规则（如 Maven release 不可覆盖、snapshot 可覆盖）；删除对 hosted 删本体与索引、对 proxy 删缓存。
- 列表分页：管理 API 列表端点统一 `offset` / `limit` 分页与过滤，返回 `{ items, total, offset, limit, has_more }` 结构。
- 多校验和：制品写入时同时计算 sha256 / sha1 / md5 / sha512 并入索引；各格式按需提供对应 sidecar（如 Maven 的 `.sha1` / `.md5` / `.sha256`），下载方可据以校验。
- 跨仓库搜索：在 `meta` 的 `artifacts` 索引上做关键字 / 坐标检索，结果按调用方身份过滤——只返回其有读权限的仓库制品，绝不泄露无权私有仓库内容。
- 使用方式片段：`format` 按格式与 `public_base_url` 生成获取与接入片段（Maven 依赖、npm / docker 命令、Raw URL/curl 及仓库接入配置），供详情页展示。
- Web 控制台嵌入与 SPA 回退：`web` 模块经 `rust-embed` 在编译期把 `frontend/dist` 打进二进制；在 `api` 路由链中于 API / 格式 / Docker / 健康检查之后接入——`/assets/{*path}` 提供静态资源（按扩展名推断 Content-Type），其余未匹配 GET 经 `fallback` 回退 `index.html` 交前端客户端路由。前端可深链路由均为单段路径、详情用查询参数，避免与格式 catch-all `/{repo}/{*path}` 冲突。干净检出下 `frontend/dist` 仅含占位、无 `index.html` 时返回 503 提示页，保证后端可独立编译 / 测试（构建顺序：先 `pnpm -C frontend build` 再 `cargo build`）。
- 漏洞库离线镜像（P2，FR-70）：`vuln` 按配置周期把公开漏洞数据集（OSV 等）按生态下载整体镜像（`{base}/{ecosystem}/all.zip`）到本机，流式落盘后解压、逐条解析公告并经 `meta` 幂等落库；下载只携带公开生态名、不把本机制品坐标逐包外发，守隐私红线。默认关闭，由配置 `[vuln]` 显式开启。
- 制品漏洞标记（P2，FR-71）：制品详情查询（`GET /api/v1/repositories/{id}/artifacts/{path}`）时，据仓库格式经 `format::Format::vuln_coordinate` 从制品路径反解生态坐标 `(ecosystem, package, version)`，`meta` 按 `(ecosystem, package)` 查本地候选受影响行，`vuln::select_hits` 用纯函数据 OSV `affected` 范围语义判定命中并去重，详情响应附 `vulnerabilities` 数组（公告 id / 严重度 / 摘要）。无标准坐标的 Raw / Docker 不参与、返回空。全程只比对本机已镜像数据，坐标绝不外发（守 ADR-0012 / 数据不外发）。

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
- ADR-0013 Docker Registry v2 Bearer 令牌认证：`/v2/token` 范围令牌端点 + 401 Bearer 质询，复用会话 JWT 的 HS256 密钥；匿名 public 读保持 tokenless，预先 Basic（curl）照旧可用。
- ADR-0014 S3 兼容对象存储后端：经 `BlobStore` 抽象新增可选 opt-in 的 `S3Store`（Cargo `s3` 特性默认关 + 配置 `data.storage.backend`，客户端 aws-sdk-s3 裁 rustls + ring），扩展 ADR-0002，本地文件系统仍默认。P2。

当前不做项：

- group/virtual 聚合仓库：P3，当前形态不实现，也不在数据模型/契约中预留占位字段。
- OIDC/LDAP 认证集成：P2，当前仅预留 provider 接口边界，不落占位字段。
- S3 兼容对象存储后端：已实现为可选 opt-in 的第二 blob 后端（Cargo `s3` 特性默认关闭 + 配置 `data.storage.backend`，见 ADR-0014）；默认仍为本地文件系统，不启用即零外部运行时依赖。
- 七层防护增强与监控告警：P2，当前不实现；L3/L4 体积型 DDoS 始终交前置反向代理 / CDN / WAF。
- 使用分析数据面板：P2，当前不实现；统计数据本机内部、不外发。
- 权限增强（用户组/团队、细粒度权限动作）：P2，当前授权仅全局角色 + 每仓库读写 ACL，不预留占位字段。
- 漏洞库对接（离线镜像 + 坐标级匹配）：P2，当前不实现、不预留漏洞相关占位字段；Docker 镜像层 OS 漏洞扫描更重，留 P3。
