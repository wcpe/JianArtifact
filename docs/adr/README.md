# 架构决策记录（ADR）

记录本项目的重大架构决策：背景、决策、理由、后果与被否的备选。每条决策一页，便于后来者理解"为什么是这样"。

## 当前阅读口径

- 当前发布版本是 `0.10.1`；已退役能力的决策史（`ADR-0013`、`ADR-0023`、`ADR-0026`）只供追溯。
- `ADR-0020` 仍是当前 v2 operation envelope 的已接受基础决策（envelope 机制随 FR-138 退役被保留复用）。
- `ADR-0023` 与 `ADR-0026` 描述的主备级联树与逐跳凭据已随 FR-138 整体退役，两者**已被 `ADR-0027` 取代**（就节点搬迁用途）；搬迁现以一致性备份包为单位。
- 当前实现与未实现目标的综合边界以 [`../ARCHITECTURE.md`](../ARCHITECTURE.md) 为准，需求状态以 [`../PRD.md`](../PRD.md) 为准。

| 编号                                                              | 决策                                                                        | 状态                                                               |
| ----------------------------------------------------------------- | --------------------------------------------------------------------------- | ------------------------------------------------------------------ |
| [0001](0001-go-backend.md)                                        | 后端采用 Go 重写（替代旧 Rust 实现）                                        | 已接受                                                             |
| [0002](0002-sqlite-filesystem-storage.md)                         | 元数据用纯 Go SQLite、制品内容用文件系统 blob                               | 已接受                                                             |
| [0003](0003-mantine-frontend.md)                                  | 管理端沿用 Mantine 7 + i18next 前端栈                                       | 已接受                                                             |
| [0004](0004-design-first-openapi.md)                              | API 契约设计优先，OpenAPI 为唯一真源                                        | 已接受                                                             |
| [0005](0005-single-binary-embed.md)                               | 前端产物经 Go embed 内嵌，单二进制交付                                      | 已接受                                                             |
| [0006](0006-monorepo-workspace.md)                                | 单 monorepo，pnpm+Turborepo / Go Task，Makefile 顶层入口                    | 已接受                                                             |
| [0007](0007-deployment-orchestration.md)                          | 部署编排以 Docker/Compose 为主，Helm/K8s 与 systemd 为可选                  | 已接受                                                             |
| [0008](0008-auth-session-model.md)                                | 认证会话模型：网页自举 + 无状态 JWT + Token 摘要                            | 已接受；部分被 0032 取代，保留未取代部分                           |
| [0009](0009-protocol-auth-and-blob-layout.md)                     | 协议端点原生客户端鉴权（API Token via Basic/Bearer）与 blob sha256 分片布局 | 认证部分已被 0015 取代；blob 布局继续有效                          |
| [0010](0010-proxy-cache-singleflight-group-routing.md)            | proxy 回源缓存、single-flight 与 group 有序路由                             | 已接受                                                             |
| [0011](0011-npm-registry-layout-and-tarball-rewrite.md)           | npm registry 路径布局与 packument tarball 重写                              | 已接受                                                             |
| [0012](0012-nexus-migration-state-machine.md)                     | Nexus 迁移任务状态机、三来源发现与凭据引用                                  | 已接受                                                             |
| [0013](0013-multi-node-replication.md)                            | 多节点复制架构：双向拉取、变更日志与 LWW                                    | 已被 0023 取代（0023 又由 0027 取代）                              |
| [0014](0014-atomic-asset-operations-and-blob-reclamation.md)      | 原子制品操作、Blob 回收与批次复制可见性                                     | 已被 0019 取代                                                     |
| [0015](0015-protocol-authentication-and-credential-references.md) | 协议鉴权、凭据引用与发布账号安全边界                                        | 已接受；部分被 0022/0025/0031/0032 取代，详见状态区                |
| [0016](0016-oci-registry-model.md)                                | OCI Distribution 路由与发布边界                                             | 已接受                                                             |
| [0017](0017-cargo-registry-model.md)                              | Cargo sparse 路由与发布边界                                                 | 已接受                                                             |
| [0018](0018-pypi-registry-model.md)                               | PyPI Simple 路由与发布边界                                                  | 已接受                                                             |
| [0019](0019-recoverable-blob-quarantine-and-operation-outbox.md)  | 可恢复 Blob 隔离回收与原子操作 Outbox                                       | 已被 0024 取代                                                     |
| [0020](0020-versioned-operation-envelope-replication.md)          | 版本化原子操作复制信封与 v1 兼容                                            | 已接受                                                             |
| [0021](0021-operation-envelope-pagination-and-v1-blocking.md)     | 操作信封分页、水位与 v1 阻断                                                | 已接受                                                             |
| [0022](0022-restricted-runtime-reference-namespaces.md)           | 受限运行时凭据与迁移来源引用命名空间                                        | 已接受；取代 0015 第 4、6 项命名空间约束                           |
| [0023](0023-primary-standby-replication.md)                       | 主备级联树单向复制与受控人工提升                                            | 已被 0027 取代；随 FR-138 退役                                     |
| [0024](0024-atomic-physical-blob-reclamation.md)                  | 原子物理 Blob 回收                                                          | 已接受；提交后清理语义被 0031/0034 部分取代，保全/恢复受 0033 扩展 |
| [0025](0025-direct-online-migration-url.md)                       | 管理员直填在线 Nexus 来源与加密凭据                                         | 已接受                                                             |
| [0026](0026-primary-standby-pull-credentials.md)                  | 级联树逐跳拉取凭据的密封存储与轮换                                          | 已被 0027 取代；随 FR-138 退役                                     |
| [0027](0027-package-based-node-backup-and-relocation.md)          | 以一致性备份包取代实时复制通道作为节点搬迁手段                              | 已接受；恢复前提被 0033 部分取代，其余包机制保留                   |
| [0028](0028-repository-alias-and-rename.md)                       | 仓库别名与重命名的语义：共享命名空间、旧名转别名、历史不回填                | 已接受                                                             |
| [0029](0029-external-identity-providers.md)                       | 外部身份源（OIDC / LDAP）接入与会话签发                                     | 已接受；签发/恢复及旧 JWT 失效边界被 0032 部分取代                 |
| [0030](0030-fts5-search-index.md)                                 | 全局搜索以 FTS5 trigram 虚拟表作派生索引，应用层同步、后台回填、可重建      | 已接受                                                             |
| [0031](0031-trusted-security-events.md)                           | 服务端可信关联和事务型安全事件                                              | 已接受，待分批实现                                                 |
| [0032](0032-credential-epochs-and-strong-authentication.md)       | 凭据代次、受控管理员认领与强认证                                            | 已接受，待分批实现                                                 |
| [0033](0033-incident-restrictions-and-recovery-gate.md)           | 持久事故隔离、证据保全与恢复安全闸                                          | 已接受，待分批实现                                                 |
| [0034](0034-private-candidates-and-publication-admission.md)      | 私有发布候选与事务准入                                                      | 已接受，待分批实现                                                 |

> **安全增量决策已接受、分批实施**：用户已批准 0031–0034 及 [PRD 安全能力](../PRD.md#461-安全审计人工事故处置与-hosted-发布准入) 的正式规格；FR-165 基础模块开发中，其余已审核待实施。旧 ADR 仅在明确范围部分取代，历史正文保留。决策接受不等于代码全部生效、验收通过或上线；当前能力以 [ARCHITECTURE](../ARCHITECTURE.md) 为准，未接入入口及真实环境验证仍须逐批完成。
> 模板：状态 / 背景 / 决策 / 理由 / 后果 / 备选方案。

> **别慌通读**：ADR 有意稀少（只为重大决策写），理解现状看 [`../ARCHITECTURE.md`](../ARCHITECTURE.md)，ADR 只按需查"为什么"；被取代的归档不打扰，当前架构 = 未取代的活跃集。增长过快是滥写信号——日常变更归 PRD 状态列 + CHANGELOG。
