# 架构决策记录（ADR）

记录本项目的重大架构决策：背景、决策、理由、后果与被否的备选。每条决策一页，便于后来者理解"为什么是这样"。

## 当前阅读口径

- 已发布版本的架构基线是 `0.7.1`；0.7.0 的双向全互联复制只保留作历史决策。
- `ADR-0020` 仍是当前 v2 operation envelope 的已接受基础决策。
- `ADR-0023` 与 `ADR-0026` 在 `0.8.0` 未发布开发窗口内记录当前已实现的级联树拓扑和逐跳凭据；真实发布验收仍以 PRD §6/§7 门禁为准。
- 当前实现与未实现目标的综合边界以 [`../ARCHITECTURE.md`](../ARCHITECTURE.md) 为准，需求状态以 [`../PRD.md`](../PRD.md) 为准。

| 编号                                                              | 决策                                                                        | 状态                                      |
| ----------------------------------------------------------------- | --------------------------------------------------------------------------- | ----------------------------------------- |
| [0001](0001-go-backend.md)                                        | 后端采用 Go 重写（替代旧 Rust 实现）                                        | 已接受                                    |
| [0002](0002-sqlite-filesystem-storage.md)                         | 元数据用纯 Go SQLite、制品内容用文件系统 blob                               | 已接受                                    |
| [0003](0003-mantine-frontend.md)                                  | 管理端沿用 Mantine 7 + i18next 前端栈                                       | 已接受                                    |
| [0004](0004-design-first-openapi.md)                              | API 契约设计优先，OpenAPI 为唯一真源                                        | 已接受                                    |
| [0005](0005-single-binary-embed.md)                               | 前端产物经 Go embed 内嵌，单二进制交付                                      | 已接受                                    |
| [0006](0006-monorepo-workspace.md)                                | 单 monorepo，pnpm+Turborepo / Go Task，Makefile 顶层入口                    | 已接受                                    |
| [0007](0007-deployment-orchestration.md)                          | 部署编排以 Docker/Compose 为主，Helm/K8s 与 systemd 为可选                  | 已接受                                    |
| [0008](0008-auth-session-model.md)                                | 认证会话模型：网页自举 + 无状态 JWT + Token 摘要                            | 已接受                                    |
| [0009](0009-protocol-auth-and-blob-layout.md)                     | 协议端点原生客户端鉴权（API Token via Basic/Bearer）与 blob sha256 分片布局 | 认证部分已被 0015 取代；blob 布局继续有效 |
| [0010](0010-proxy-cache-singleflight-group-routing.md)            | proxy 回源缓存、single-flight 与 group 有序路由                             | 已接受                                    |
| [0011](0011-npm-registry-layout-and-tarball-rewrite.md)           | npm registry 路径布局与 packument tarball 重写                              | 已接受                                    |
| [0012](0012-nexus-migration-state-machine.md)                     | Nexus 迁移任务状态机、三来源发现与凭据引用                                  | 已接受                                    |
| [0013](0013-multi-node-replication.md)                            | 多节点复制架构：双向拉取、变更日志与 LWW                                    | 已被 0023 取代                            |
| [0014](0014-atomic-asset-operations-and-blob-reclamation.md)      | 原子制品操作、Blob 回收与批次复制可见性                                     | 已被 0019 取代                            |
| [0015](0015-protocol-authentication-and-credential-references.md) | 协议鉴权、凭据引用与发布账号安全边界                                        | 已接受；第 4、6 项命名空间由 0022 取代    |
| [0016](0016-oci-registry-model.md)                                | OCI Distribution 路由与发布边界                                             | 已接受                                    |
| [0017](0017-cargo-registry-model.md)                              | Cargo sparse 路由与发布边界                                                 | 已接受                                    |
| [0018](0018-pypi-registry-model.md)                               | PyPI Simple 路由与发布边界                                                  | 已接受                                    |
| [0019](0019-recoverable-blob-quarantine-and-operation-outbox.md)  | 可恢复 Blob 隔离回收与原子操作 Outbox                                       | 已被 0024 取代                            |
| [0020](0020-versioned-operation-envelope-replication.md)          | 版本化原子操作复制信封与 v1 兼容                                            | 已接受                                    |
| [0021](0021-operation-envelope-pagination-and-v1-blocking.md)     | 操作信封分页、水位与 v1 阻断                                                | 已接受                                    |
| [0022](0022-restricted-runtime-reference-namespaces.md)           | 受限运行时凭据与迁移来源引用命名空间                                        | 已接受；取代 0015 第 4、6 项命名空间约束  |
| [0023](0023-primary-standby-replication.md)                       | 主备级联树单向复制与受控人工提升                                            | 待用户验收；取代 0013                     |
| [0024](0024-atomic-physical-blob-reclamation.md)                  | 原子物理 Blob 回收                                                          | 已接受；取代 0019 第 3 项                 |
| [0025](0025-direct-online-migration-url.md)                       | 管理员直填在线 Nexus 来源与加密凭据                                         | 已接受                                    |
| [0026](0026-primary-standby-pull-credentials.md)                  | 级联树逐跳拉取凭据的密封存储与轮换                                          | 待用户验收，FR-121 待用户验收             |

> 模板：状态 / 背景 / 决策 / 理由 / 后果 / 备选方案。

> **别慌通读**：ADR 有意稀少（只为重大决策写），理解现状看 [`../ARCHITECTURE.md`](../ARCHITECTURE.md)，ADR 只按需查"为什么"；被取代的归档不打扰，当前架构 = 未取代的活跃集。增长过快是滥写信号——日常变更归 PRD 状态列 + CHANGELOG。
