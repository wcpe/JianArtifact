# 架构决策记录（ADR）

记录本项目的重大架构决策：背景、决策、理由、后果与被否的备选。每条决策一页，便于后来者理解"为什么是这样"。

| 编号 | 决策 | 状态 |
|---|---|---|
| 0001 | 技术栈与单一二进制打包：后端 Rust+axum+tokio、前端 React+Vite+TS 经 rust-embed 嵌入，单一二进制（strip+LTO+panic=abort、forbid unsafe、<27MB） | 已接受 |
| 0002 | 嵌入式 SQLite 元数据存储（sqlx bundled）作为元数据唯一真源，blob 存文件系统、DB 仅存索引与 sha256 | 已接受 |
| 0003 | 认证机制：本地用户名/密码（argon2）+ Bearer Token + Basic Auth + Web 会话/JWT，预留认证 provider 抽象边界 | 已接受 |
| 0004 | 授权模型：全局角色（Admin/User）+ 每仓库可见性（public/private）+ 每仓库读写 ACL，匿名仅读 public | 已接受 |
| 0005 | 仓库类型：每格式支持 hosted + proxy（含缓存），group/virtual 聚合顺延第三期 | 已接受 |
| 0006 | 制品库迁移入口：在线 REST API + 离线 blob store 双入口，搬运 proxy 配置/缓存与 hosted 制品 | 已接受 |
| 0007 | 权限粒度与用户组：扩展授权模型，新增细粒度权限动作（read/write/delete/admin）与用户组/团队（P2，扩展 0004） | 已接受 |
| 0008 | 七层（L7）应用层防护：多维限流/并发控制/慢速防护/异常封禁/黑白名单/CC 挑战/WAF 规则 + 监控告警，L3/L4 交前置设施（P2） | 已接受 |
| 0009 | 内部使用分析与数据面板：访问/下载统计落本地、不外发、不 phone-home（P2） | 已接受 |
| 0010 | 首个管理员引导：空库首启从环境变量或随机口令创建首个管理员，不开放公开自助注册 | 已接受 |
| 0011 | 会话与 JWT 生命周期：TTL + 刷新端点 + 按承载方式的 CSRF 防护，与 API Token 相互独立 | 已接受 |
| 0012 | 漏洞库离线对接：本地镜像 OSV 等公开漏洞数据 + 坐标级本地匹配，不逐包外发（P2）；Docker 层扫描留 P3 | 已接受 |
| 0013 | Docker Registry v2 Bearer 令牌认证：`/v2/token` 范围令牌端点 + 401 Bearer 质询，复用会话 JWT 的 HS256 密钥，匿名 public 读保持 tokenless | 已接受 |
| 0014 | S3 兼容对象存储后端：经 `BlobStore` 抽象新增可选 opt-in 的 `S3Store`（Cargo `s3` 特性默认关 + 配置 `data.storage.backend`，客户端 aws-sdk-s3 裁 rustls），扩展 ADR-0002，本地 FS 仍默认（P2） | 已接受 |
| 0015 | 可观测性：审计日志经 `meta` 异步落 SQLite（保留期 + 行数轮转、脱敏）+ Prometheus 指标进程内 `metrics`/exporter 经 `GET /metrics` 被动 pull（默认仅 Admin），默认不外发不 phone-home（P2） | 已接受 |
| 0016 | 认证 provider 抽象 + OIDC（授权码流+PKCE）/LDAP（bind）：落地 ADR-0003 预留边界，只在登录入口接入并收敛为本地会话/JWT，四通道与鉴权矩阵不变，JIT 默认关、默认角色 User，不破 ADR-0010（P2） | 已接受 |
| 0017 | 防护监控与告警：五类 L7 防护计数接入 `/metrics`（低基数）+ 进程内阈值告警（中文分级日志 + 异步落 SQLite、去抖、默认关）+ 管理员只读状态端点，坚持数据不外发、不内置外发型通知，扩展 ADR-0008、复用 ADR-0015（P2） | 已接受 |
| 0018 | 运行时防护配置热替换：防护各维度阈值/开关/难度/IP 名单/WAF 规则经 Admin 在线 PATCH 即时生效（std `RwLock` 原子换快照、锁外重建 ip_matcher/waf_rules 派生态），扩展 ADR-0008（P2） | 已接受 |
| 0019 | 迁移执行异步化为进程内任务：在线拉取迁移立即返回 `job_id`、后台 tokio 任务跑，进度存进程内有界注册表（不落库）+ 轮询查询端点 + 客户端重连；保留 ADR-0006「无须持久化迁移任务表」，扩展 ADR-0006（P2） | 已接受 |
| 0020 | 统一出站网络代理与共享出站客户端：`[network.proxy]`（http/https/no_proxy + env）为出站代理唯一真源，`config` 层抽 `build_outbound_client` 统一注入全部出站 reqwest 客户端（rustls 保持、凭据脱敏），配置给值即真源、不配置保留系统 env（P2） | 已被 ADR-0022 取代 |
| 0021 | 在线更新（自更新）机制：管理员手动触发查 GitHub 最新 Release → 按本机 target 下载 → 校验 sha256 → 原子替换二进制 → graceful-shutdown 后自动重启（restart_mode self/exit）；出站默认关闭、只拉公开数据不外发、复用 ADR-0020 helper、仅 sha256 不签名（P2） | 已接受 |
| 0022 | 运行时可编辑设置与出站客户端热替换（取代 ADR-0020）：网络代理与在线更新可调字段经 Admin 在线 PATCH 即时生效、无须重启；`config` 层 `NetworkState`（std `RwLock<Arc<NetworkSnapshot>>` 含代理配置 + reqwest::Client），出站点按需取 client、PATCH 锁外重建后原子换槽；沿用 ADR-0020 真源/helper/rustls/脱敏，凭据只入内存槽不落库不回显，设置页改可编辑（P2） | 已接受 |

> 模板：状态 / 背景 / 决策 / 理由 / 后果 / 备选方案。

> **别慌通读**：ADR 有意稀少（只为重大决策写），理解现状看 [`../ARCHITECTURE.md`](../ARCHITECTURE.md)，ADR 只按需查"为什么"；被取代的归档不打扰，当前架构 = 未取代的活跃集。增长过快是滥写信号——日常变更归 PRD 状态列 + CHANGELOG。
