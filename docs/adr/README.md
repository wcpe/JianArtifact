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
| 0020 | 统一出站网络代理与共享出站客户端：`[network.proxy]`（http/https/no_proxy + env）为出站代理唯一真源，`config` 层抽 `build_outbound_client` 统一注入全部出站 reqwest 客户端（rustls 保持、凭据脱敏），配置给值即真源、不配置保留系统 env（P2） | 部分被 ADR-0022、ADR-0024 取代 |
| 0021 | 在线更新（自更新）机制：管理员手动触发查 GitHub 最新 Release → 按本机 target 下载 → 校验 sha256 → 原子替换二进制 → graceful-shutdown 后自动重启（restart_mode self/exit）；出站默认关闭、只拉公开数据不外发、复用 ADR-0020 helper、仅 sha256 不签名（P2） | 已接受（回滚增强见 ADR-0026、重启增强见 ADR-0032） |
| 0022 | 运行时可编辑设置与出站客户端热替换（取代 ADR-0020）：网络代理与在线更新可调字段经 Admin 在线 PATCH 即时生效、无须重启；`config` 层 `NetworkState`（std `RwLock<Arc<NetworkSnapshot>>` 含代理配置 + reqwest::Client），出站点按需取 client、PATCH 锁外重建后原子换槽；沿用 ADR-0020 真源/helper/rustls/脱敏，凭据只入内存槽不落库不回显，设置页改可编辑（P2） | 已接受 |
| 0023 | 主机/系统监控采集：经 sysinfo 跨平台按请求采样本机 CPU/内存/磁盘/uptime，仅 Admin `GET /api/v1/monitor/host`，本机内部不外发（P2） | 已接受（「不留历史时序」一条被 ADR-0027 取代，其余仍有效） |
| 0024 | SOCKS5 出站代理与网页代理凭据管理（取代 ADR-0020「不支持 SOCKS」条目）：启 reqwest `socks` 特性，`[network.proxy]` 新增 `all` 键经 `reqwest::Proxy::all` 支持 `socks5://` 全 scheme 兜底代理（注入序 http→https→all）；设置页每代理拆 URL/用户名/密码三字段，用户名回显、密码三态不回显，纯函数 `rebuild_proxy_url` 据三字段 + 当前存储值重建含凭据 URL（userinfo RFC3986 编码），凭据只入内存槽不落库 / 不回显（P2） | 已接受 |
| 0025 | 开源许可清单构建期扫描 + 数据嵌入二进制 + 公开页：构建期由 `cargo-about`（Rust，按 `about.toml` accepted 清单）+ `pnpm licenses list`（前端）扫描运行时 + 开发依赖许可，`scripts/gen-licenses.mjs` 合并为 JSON 嵌入二进制（`include_str!` + 占位降级），公开端点 `GET /api/v1/licenses` 与页 `/licenses` 匿名只读；运行时不外联、守数据不外发（P2） | 已接受 |
| 0026 | 自更新回滚（增强 ADR-0021）：升级时把当前二进制持久备份为跨平台一致的 `{exe}.rollback.bak`（单备份、不被启动清理，独立于 ADR-0021 临时 `.bak`/`.old`）→ `POST /api/v1/update/rollback`（仅 Admin）复用 execute_replace 原子换回 + 走既有重启链路 → 无备份返 409，回滚与升级共用 apply 单飞 guard，设置视图增 `rollback_available`（P2） | 已接受 |
| 0027 | 统一指标时序采集与查询（取代 ADR-0023「不留时序」）：通用扁平表 `metric_samples(metric_key,ts,value)` 经 `meta` 落库，后台定时按可配间隔采样主机/存储仓库/防护/使用分析各域 gauge + 可配保留期滚动清理 + 行数兜底，仅 Admin `GET /api/v1/monitor/metrics` 降采样查询；保留 FR-98 实时快照，缓存命中率本期降级不采，本机内部不外发（P2） | 已接受 |
| 0028 | 动态配置持久化（文件默认 + DB 覆盖 + 内存缓存，扩展 ADR-0022）：`app_settings(key,value_json)` KV 表经 `meta` 落库，启动加载文件默认 → 读 DB 覆盖 → 填内存热替换槽，PATCH 写库 + 换槽即时生效；优先级 env 显式 > DB > 文件默认；**凭据与 bootstrap 严格不入库**（代理账密/token/密钥/端口/数据目录仍走文件+env），落库白名单默认拒绝；`config` 不反向依赖 DB（覆盖在装配层），增量纳入高频非密钥节（P2） | 已接受 |
| 0031 | 向前兼容迁移 + 容忍未知已应用迁移（扩展 ADR-0002/0026）：自更新回滚只还原二进制不还原 DB，旧二进制遇 DB 里新版应用的更高迁移时 sqlx 默认报错锁死；故 ① 迁移**只增不改不删**（向前兼容，破坏性变更先走 ADR）② `MetaStore::open` 跑迁移设 `set_ignore_missing(true)` 忽略未知更高迁移、只跑自身待应用项——使回滚跨增量迁移后旧二进制仍能打开 DB（P2） | 已接受 |
| 0030 | 网络代理凭据加密落库持久化（扩展 ADR-0028/0024、细化 ADR-0018）：代理经网页配置后只入内存槽、重启即丢（自更新重启后出站断），故落库 `app_settings`——URL/用户名明文、**密码用 XChaCha20-Poly1305（RustCrypto 纯 Rust）加密落库**（密文非明文、红线不破），加密子密钥经 `JwtSigner::derive_key` 从 `.jwt_secret` 文件真源域分隔派生、绝不入库；启动解密恢复进内存槽，解密失败降级无密码不阻断；专用持久化路径不并入 FR-106 明文白名单（P2） | 已接受 |
| 0032 | 交互式终端下的自更新重启与备份健壮性（扩展 ADR-0021/0026）：`restart_mode=self` 在 Unix 改用 `exec` 原地替换进程映像（同 PID/终端/前台，tmux 前台自更新后进程不脱离、日志连续；端口已在优雅停机释放），Windows 无 exec 保持 spawn+exit；派生 staged/.bak/.old/.rollback.bak 前剥离已叠管理后缀防 compound（`.bak.bak`）+ 守自拷贝 + 启动清 compound 残留，保留单层 `.bak`/`.rollback.bak` 两份；原地替换保留原文件名为设计行为（保路径引用、--version 自报真实版本）（P2） | 已接受 |
| 0033 | Web 触发的系统重启 / 关闭（复用自更新重启基建，扩展 ADR-0021/0032）：新建仅 Admin 的 `POST /api/v1/system/restart`（按运行时 restart_mode 重启、不换二进制）与 `/system/shutdown`（强制 Exit 优雅退出、不自拉起），经现有 `RestartHandle::request_restart` + graceful-shutdown 复用停机链路、不新造；安全边界=仅 Admin + 与自更新 apply/rollback 共用单飞互斥 + 入审计（system.restart/shutdown）+ 前端二次确认；纯本地不出站故不受 `[update] enabled` 约束；关闭在配自动重启管理器时会被再起（文档化运维前提）；真重启/真关闭依赖真机验证（P2） | 已接受 |
| 0029 | 运行时日志文件 sink + 读取 API（扩展 ADR-0015）：`init_tracing` 保留 stdout 之外经 tracing-subscriber `reload` 层（拿到 data_dir 后换入）追加文件 sink 写 `{data_dir}/logs/app.log`，自实现单文件 + 单次大小滚动（`std`，不引 tracing-appender）；仅 Admin `GET /api/v1/system-logs` 读文件 → 纯函数解析（时间/级别/消息）+ 级别精确过滤 + tail/分页 → 统一分页响应，文件缺失返空；运行日志载体文件、**不落库**，与审计（业务留痕落 SQLite）严格区分（P2） | 已接受 |

> 模板：状态 / 背景 / 决策 / 理由 / 后果 / 备选方案。

> **别慌通读**：ADR 有意稀少（只为重大决策写），理解现状看 [`../ARCHITECTURE.md`](../ARCHITECTURE.md)，ADR 只按需查"为什么"；被取代的归档不打扰，当前架构 = 未取代的活跃集。增长过快是滥写信号——日常变更归 PRD 状态列 + CHANGELOG。
