# 变更日志

本项目所有重要变更记录于此。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## 未发布版本

### 新增
- Go 模块格式（hosted + proxy）经统一 Format trait 注册接入通用机理：按 GOPROXY 协议暴露 `@v/list` / `.info` / `.mod` / `.zip` / `@latest`，模块路径大小写 bang 编码（`!x` ↔ `X`），版本不可变（重复上传同版本 409），多校验和与流式存取；hosted 据已存版本聚合 `@v/list` 与 `@latest`、`.info` 缺失时按 `.mod` 合成，proxy 对 `.mod`/`.zip`/`.info` 走 cache-miss 单飞缓存、对 `@v/list`/`@latest` 回源透传；授权复用既有编排（上传需 write、private 对无权一律 404）
- Cargo 格式（hosted+proxy，FR-26）：按 Cargo 稀疏索引协议接入，支持 `cargo publish` 发布、稀疏索引与 `.crate` 下载、yank/unyank、registry config.json；同版本不可覆盖（409）、索引 cksum 用 sha256；proxy 回源上游索引（不缓存）并缓存 `.crate`（cache-miss→hit）；发布/yank 需写权限、private 对无权一律 404
- PyPI 格式（FR-27，hosted + proxy）：Simple Repository API（PEP503 HTML / PEP691 JSON）项目与文件索引、twine multipart 上传、pip 下载；hosted 已发布文件不可覆盖（409），proxy 回源上游 Simple 并重写文件链接、包文件单飞缓存（cache-miss → hit 不重复回源）
- NuGet 格式（hosted + proxy）经统一 Format trait 接入：NuGet v3 服务索引、扁平容器版本列表与 .nupkg / .nuspec 存取、`nuget push`（multipart 解析 .nupkg 内嵌 .nuspec 取 id/version）、已发布版本不可覆盖（重复 push 同版本 409）、四校验和、id/version 小写规范化；proxy 回源服务索引重写指向本仓库、版本列表回源、.nupkg cache-miss 缓存；支持 `dotnet nuget push` / `dotnet add package`
- S3 兼容对象存储后端（FR-30，可选 opt-in，默认关闭）：新增 Cargo 特性 `s3` 与 `[data.storage]` 配置节（`backend = "fs"`（默认）/`"s3"` + endpoint/region/bucket/prefix/path_style）；启用 `s3` 特性并配置 `backend = "s3"` 后 blob 本体改存对象存储，写入语义与本地等价（本地临时文件算 sha256 → 内容寻址 key 流式 multipart 上传，失败清理不留孤儿对象），下载流式 GET 不整体载入内存；本地文件系统仍为默认后端，默认构建不含任何 S3 代码与依赖、保持单一二进制零外部运行时依赖；客户端 aws-sdk-s3 裁为纯 rustls + ring（不引入 aws-lc-rs）。详见 ADR-0014 与 docs/OPERATIONS.md「启用即引入外部依赖」
- 审计日志（FR-31，ADR-0015）：新增 `audit_log` 表，经审计中间件采集精选的写 / 管理 / 授权拒绝事件（登录、Token 与用户管理、仓库与 ACL 变更、制品上传 / 删除），普通匿名读取不入审计；事件经进程内有界 channel 异步批量落 SQLite，主路径只做非阻塞投递、采集失败不影响业务、队列满则丢弃 + 计数 + WARN；后台任务按保留天数（`observability.audit.retention_days`，默认 90）与行数上限（`observability.audit.max_rows`，默认 100 万）轮转；新增 `GET /api/v1/audit` 仅 Admin 分页查询；密码 / Token / JWT / 上游凭据一律不入审计
- Nexus OSS 迁移在线 REST API 入口（FR-36）：新增 `migrate` 模块与 `POST /api/v1/migrate/nexus/preview` 端点（仅管理员），连接在线 Nexus 并经其 `service/rest/v1/repositories` 枚举可迁移仓库列表与基本元数据（名 / 格式 / 类型 / proxy 上游地址），作为迁移的发现 / 预览步骤；REST 交互经 `NexusClient` trait 抽象、生产实现复用 reqwest 纯 rustls，访问凭据真源环境变量（`JIANARTIFACT_MIGRATE_<NAME>_USERNAME/PASSWORD`，DB 仅存引用、不入库不进日志）；连接 / 鉴权 / 解析失败映射为 502，不泄露源系统内部细节。仅做发现 / 预览，不搬运制品
- Nexus OSS 迁移离线 blob store 入口（FR-37）：新增 `POST /api/v1/migrate/nexus/offline/preview` 端点（仅管理员），当源 Nexus 已下线、只剩其文件型 blob store 目录时，从给定本地目录解析磁盘布局（`content/` 分片目录 + 每个 blob 一份 `.properties` 元数据），按所属仓库枚举可迁移 blob 及基本元数据（坐标 / sha1 / 大小），作为离线迁移的发现 / 预览步骤；软删 / 损坏 / 缺必要字段的元数据容错跳过、不中断整次枚举，路径不存在 / 缺 `content/` 目录映射为 400；阻塞文件 IO 经 `spawn_blocking` 不阻塞异步运行时。仅解析 `.properties`、不读取也不搬运 blob 本体
- Nexus 迁移 proxy 仓库配置 + 缓存制品搬运（FR-38，ADR-0006）：新增 `POST /api/v1/migrate/nexus/proxy/migrate` 端点（仅管理员），把源 Nexus 的 proxy 类型仓库搬到本系统——据在线 REST 枚举的 proxy 仓库配置（映射 Nexus 格式名 → 本系统已实现格式，如 `maven2`→`maven`；同名仓库复用、未实现格式或缺上游地址整体跳过）在本系统建仓，再从离线 blob store 按仓库名取该仓库的缓存制品本体（成对 `.properties` + `.bytes`，缺本体跳过），经既有 `ArtifactService::ingest_cached` 流式写入缓存（blob 先落盘并校验 sha256 再写元数据索引并标记 `cached`，写索引失败回滚不留孤儿，不整体载入内存）；搬运幂等可重入（同坐标同内容跳过），单制品失败（路径非法 / 读本体失败 / 写入失败）记录跳过、不中断整批，无须持久化迁移任务表；迁移不搬运源系统上游凭据（凭据真源 env / 配置）。仅迁移 proxy 仓库，hosted 仓库制品完整搬运（FR-39）尚未实现
- 漏洞库离线镜像（FR-70，ADR-0012）：新增 `vuln` 模块，按配置周期把公开漏洞数据集（OSV，按生态 `all.zip`）整体镜像下载到本机，流式落盘后解压、逐条解析公告并经 `meta` 幂等落库（公告表 + 受影响坐标表 + 刷新状态表）；下载只携带公开生态名、不外发本机制品坐标。新增 `[vuln]` 配置（默认关闭，含数据源、生态列表、刷新周期）。本批仅镜像/落库，制品坐标匹配标记（FR-71）尚未实现
- 访问 / 下载统计采集（FR-57，ADR-0009）：新增 `usage_stats` 聚合计数表与可选 `usage_events` 明细表；制品 GET 下载记 `download`、详情查看记 `access`，事件经进程内有界 channel 异步批量聚合落 SQLite（UPSERT 累加、并发下计数准确），主路径只做非阻塞采集、采集失败不影响业务、队列满则丢弃 + 计数 + WARN；明细默认关闭，开启后按行数上限兜底裁剪。新增 `[observability.usage]` 配置（`detail_enabled` 默认关闭、`max_detail_rows` 默认 100 万）。统计数据本机内部、默认不外发、不向外部遥测 phone-home，不提供外部导出。本批仅采集 / 落库并提供内部聚合查询入口，数据面板展示（FR-58）尚未实现
- Prometheus 指标端点（FR-32，ADR-0015）：用 `metrics` facade + `metrics-exporter-prometheus` 进程内 recorder（pull 模型，不引外部时序库、不 push / remote-write），新增 `GET /metrics` 渲染进程内注册表为 Prometheus 文本；指标中间件在请求热路径无锁采集 HTTP 维度（method / status_class / format 计数与延迟直方图、上传 / 下载字节、并发上传 gauge），`proxy` 回源边界埋点缓存命中 / 未命中与上游耗时 / 失败，标签均为有界枚举（严禁仓库名 / 路径 / 用户名 / 坐标作标签）；端点默认仅 Admin（`401` / `403`），新增 `[observability.metrics]` 配置 `enabled`（默认开，关闭则 404）与 `allow_anonymous`（默认关，开启须限内网 / 反代后）；指标本机内部、仅抓取时渲染、不主动外发
- 用户组/团队与对组授予仓库 ACL（FR-49，ADR-0007）：新增 `groups` / `user_groups` / `repo_group_acl` 三张表与组管理端点（仅 Admin）——建组 / 删组（级联清成员与组 ACL）/ 加移成员、对组授予 / 撤销仓库读 / 写 / 删 / 管理四级 ACL；授权判定中用户对某仓库的有效权限改为「直接 ACL ∪ 其所属各组的组 ACL」取并集后按动作蕴含判定，既有直接-ACL 判定结论与鉴权矩阵保持不变；私有仓库列表过滤与详情 / 浏览同步纳入经组继承的读权限。增强管理 UI/API（FR-50）尚未实现
- 制品漏洞标记（FR-71，ADR-0012）：制品详情 `GET /api/v1/repositories/{id}/artifacts/{path}` 响应新增 `vulnerabilities` 数组，列出该制品命中的已知漏洞公告（id / 严重度 / 摘要）。`format` 各处理器经 `vuln_coordinate` 从制品路径反解生态坐标（Maven `groupId:artifactId`、npm 包名，含版本；Raw / Docker 无坐标不参与），`meta` 按 `(ecosystem, package)` 查本地候选受影响行，`vuln::select_hits` 用纯函数据 OSV `affected` 范围语义（`introduced` 起含、`fixed` 止不含、`last_affected` 止含，另含显式 `versions` 列表）判定命中并去重。即时查本地受影响坐标表匹配、不落制品-漏洞缓存表；全程只比对本机已镜像数据，制品坐标绝不外发到外部漏洞服务（守数据不外发红线）
- 基础速率限制（FR-33，ADR-0008）：新增 `[protection.rate_limit]` 配置与限流中间件，按 **IP 维度**（连接来源地址）与 **身份维度**（已认证用户及其所有 Token / 会话）用进程内固定时间窗计数，任一维度单窗超阈值即在进入业务前返回 `429 Too Many Requests`（错误码 `too_many_requests`，带 `Retry-After`）；中间件置于身份解析之内、业务之前，热路径只取一次锁做整型自增与窗口比较（无锁外 IO / 无格式化），窗口表过期键按表大小阈值顺带清理防无界增长。来源 IP 取连接级 `ConnectInfo`、**不采信 `X-Forwarded-For`**（伪造来源不绕过），轮换 IP 的同一主体仍受身份维度阈值约束。配置 `enabled`（默认关闭）、`window_secs`（默认 60）、`ip_max_requests`（默认 1200）、`identity_max_requests`（默认 2400），**默认阈值保守、不误杀正常包管理器批量拉取**，配置热替换下个请求即按新值判定。仅应用层（L7）基础限流；多维（用户 / 仓库）限流与并发/连接上限属 FR-51、慢速 / 封禁 / CC / WAF / 告警属 FR-52~56，均不在本批，L3/L4 体积型攻击仍由前置设施承担
- 角色与权限管理增强 UI（FR-50，ADR-0007）：Web 控制台接入用户组与四级动作管理（均仅 Admin）。仓库详情「权限」页签拆为「用户授权」与「用户组授权」两块，授权动作下拉从读 / 写扩为读 / 写 / 删除 / 管理四级（对接 FR-48 既有 ACL 端点）；新增「用户组管理」页支持建组 / 删组、经成员弹窗加入 / 移出成员，仓库「用户组授权」面板对组授予 / 撤销四级 ACL（对接 FR-49 既有组管理与组 ACL 端点）。前端 API 客户端补齐组管理、组 ACL 与四级动作的类型与调用；四级动作的下拉选项 / 中文标签 / 徽章配色抽为共享辅助供用户与组两套面板复用。本批为纯前端增强，不新增后端端点

### 变更
- 仓库 ACL 权限动作细化为四级 `read` / `write` / `delete` / `admin`（FR-48 / ADR-0007）：授权判定纯函数按动作蕴含关系（admin ⊇ delete ⊇ write ⊇ read）综合可见性、全局角色与 ACL 给出结论；既有读 / 写授权语义与判定结论保持不变，既有 `read` / `write` 数据原样兼容；ACL 管理端点（`POST /api/v1/repositories/{id}/acl`）接受四级动作取值。本次仅落地动作模型与判定，删除 / 管理动作的具体业务端点未接入

### 修复
- 无

### 移除
- 无

### 安全
- 无

## [0.1.0] - 2026-06-24

首个正式版本，交付第一期（P1）全部 36 项功能需求（FR-01..25、FR-59..69），含四种高频格式（Maven / npm / Docker、OCI / Raw）的 hosted 与 proxy、认证鉴权、Web 控制台与单一二进制打包。

### 新增
- 项目文档与治理脚手架初始化（PRD、架构、ADR、防漂移规则、工程化配置）
- 运行地基：TOML + 环境变量配置加载、嵌入式 SQLite 元数据库与迁移、文件系统 blob 存储（多校验和）、空库首启管理员引导、健康检查端点
- 认证与身份层：本地口令登录与 JWT 会话（TTL / 刷新 / 当前用户 /me）、API Token 签发/列表/吊销（哈希存储）、Basic Auth 鉴权、全局角色与管理员用户管理、统一身份解析中间件（Bearer-JWT / Bearer-Token / Basic / 匿名 四通道）、登录暴力破解防护（失败锁定 / 限流）
- 仓库模型与授权层：仓库创建/配置/删除（格式、hosted/proxy 类型、public/private 可见性）、每仓库读写 ACL 管理、按全局角色×可见性×ACL 综合判定的授权纯函数、仓库列表（按身份过滤）/详情/制品浏览端点；私有仓库对匿名与无权用户一律返回 404 隐藏存在性
- 制品通用机理与统一格式 trait + Raw 参考格式：hosted 制品流式直传/下载、proxy 代理上游并缓存（cache-miss 回源→校验→落盘→写索引、命中不回源、并发单飞合并、上游失败回退不写坏缓存）、blob 先落盘再写索引（失败回滚不留孤儿）、上传大小限制（超限 413）、四校验和计算与暴露、制品删除与按格式覆盖策略、Raw 格式端点（PUT/GET/DELETE 路径直存直取）、制品详情（四校验和 + 使用方式片段）、跨仓库搜索（结果按读权限过滤、不泄露无权私有制品）
- 三种高频格式（hosted+proxy）经统一 Format trait 注册接入通用机理：Maven（仓库布局、maven-metadata.xml、.sha1/.md5/.sha256 sidecar、release 不可覆盖 409 / snapshot 可覆盖）、npm（packument/tarball、publish 解析 _attachments、已发布版本不可覆盖、dist shasum/integrity 摘要、scoped 包）、Docker/OCI（Registry v2：blob 上传状态机与 digest 校验、manifest 存取、同 tag 可覆盖、tags/list 列出镜像 tag）
- Docker Registry v2 Bearer 令牌认证：新增 `/v2/token` 范围令牌端点（Basic 凭据换取短期 docker 令牌、按 scope 逐项判定授予动作），`GET /v2/` 未带凭据时返回 `401 + WWW-Authenticate: Bearer` 发起认证发现、受保护操作未认证时返回带 scope 的 Bearer 质询，docker 操作接受 `Authorization: Bearer` 令牌；复用会话 JWT 的 HS256 密钥；匿名拉取 public（透明换取匿名令牌）与预先 Basic（curl）照旧可用。让真实 OCI 客户端（skopeo / docker）的认证推送可用
- React Web 控制台（登录与基础仪表盘、仓库管理、用户与每仓库 ACL 管理、Token 管理、制品浏览与跨仓库搜索及详情）：React + Vite + TypeScript + Mantine，登录拿 JWT 放 Authorization 头、401 跳登录、统一错误与分页解析、按角色显隐管理界面；经 rust-embed 编译期嵌入前端产物，axum 提供静态资源与 SPA 客户端路由回退（不拦截 API / 格式 / 健康检查端点）

### 变更
- 无

### 修复
- 无

### 移除
- 无

### 安全
- 记录 RUSTSEC-2023-0071（rsa crate Marvin 攻击，计时侧信道，中危，无修复版本）受控忽略：该依赖经 jsonwebtoken 的 rust_crypto 伞形特性传入，本项目 JWT 仅用 HS256（HMAC）、从不执行 RSA 运算，计时侧信道在实际执行路径不可达；理由与复核条件见 `.cargo/audit.toml`

> 发版时把"未发布版本"段切成 `## [X.Y.Z] - YYYY-MM-DD`，再新建空的"未发布版本"段。
