# 功能规格（specs）

规格是功能开发期的详细工作稿：集中记录“要什么、怎么做、任务、验收”。它不替代 PRD、ARCHITECTURE、API 或 ADR，也不把未实现目标伪装成当前代码。

## 1. 文档分层

| 层级     | 文件                                               | 责任                                 |
| -------- | -------------------------------------------------- | ------------------------------------ |
| 需求登记 | [`../PRD.md`](../PRD.md)                           | 一条 FR、优先级、状态和版本归属      |
| 当前架构 | [`../ARCHITECTURE.md`](../ARCHITECTURE.md)         | 当前实现的模块、数据和依赖边界       |
| API 真源 | [`../../api/openapi.yaml`](../../api/openapi.yaml) | 管理 API 的字段、路径和生成输入      |
| 架构决策 | [`../adr/README.md`](../adr/README.md)             | 为什么采用或否决某种长期方案         |
| 功能规格 | 本目录                                             | 某个 FR 的开发目标、设计、任务和验收 |

## 2. 0.8.0 规格（已发布）

`0.8.0` 已发布（`VERSION=0.8.0`）。下表是 0.8.0 窗口的主规格，全部随 v0.8.0 交付或退役；正文保留为验收证据。

| FR     | 规格                                                                                                               | 主题                                     | 状态   |
| ------ | ------------------------------------------------------------------------------------------------------------------ | ---------------------------------------- | ------ |
| FR-105 | [`0.8.0-atomic-asset-replication.md`](0.8.0-atomic-asset-replication.md)                                           | 原子 operation、blob 与级联中继完整性    | 已交付 |
| FR-115 | [`0.8.0-primary-standby-replication.md`](0.8.0-primary-standby-replication.md)                                     | 主备级联树、父子边和受控恢复             | 已退役 |
| FR-119 | [`0.8.0-cluster-observability.md`](0.8.0-cluster-observability.md)、[`primary-sync-view.md`](primary-sync-view.md) | 集群页面、直接邻接同步监控与同步观测增强 | 已退役 |
| FR-121 | [`0.8.0-primary-standby-pull-credentials.md`](0.8.0-primary-standby-pull-credentials.md)                           | 父子边逐跳拉取凭据                       | 已退役 |
| FR-53  | [`0.8.0-operations-dashboard.md`](0.8.0-operations-dashboard.md)                                                   | 业务运营仪表盘                           | 已交付 |
| FR-120 | [`0.8.0-host-monitoring.md`](0.8.0-host-monitoring.md)                                                             | 仅当前实例主机监控                       | 已交付 |
| FR-122 | [`0.8.0-admin-observability-mock-matrix.md`](0.8.0-admin-observability-mock-matrix.md)                             | 管理控制台 DevMock 验收矩阵              | 已交付 |
| FR-132 | [`0.8.0-node-backup-and-relocation.md`](0.8.0-node-backup-and-relocation.md)                                       | 节点备份包、冻结窗口与三通道导入         | 已交付 |
| FR-117 | [`notification-center.md`](notification-center.md)                                                                 | 消息中心（已于 v0.8.0 整体退役）         | 已退役 |

标为「已退役」的规格只保留为历史证据：其描述的角色、凭据、水位、拓扑与集群页面在当前代码中已不存在（见 FR-138 与 [`../adr/0027`](../adr/0027-package-based-node-backup-and-relocation.md)），不得作为部署或接口依据。

其它 `0.8.0-*` 文件分别对应格式、迁移、部署安全、生命周期和控制台子能力；以文件头的 FR、状态和目标版本为准。

## 3. 历史规格

`0.1.0`–`0.7.1` 的规格保留用于验收证据和决策追溯。尤其是 `0.7.0-replication-*` 中的双向对等、全互联和共享令牌是历史模型；其后的级联树模型（FR-115/105/121/119）亦已随 FR-138 整体退役，当前搬迁以一致性备份包为单位，任何旧复制规格都不能照旧部署。

## 4. 编写规则

- 命中新数据模型、外部接口、跨模块、并发/事务/状态机或架构决策时，必须先写规格。
- 规格状态必须诚实：`开发中`、`开发完成，待用户验收`、`开发完成，验收通过，待发版` 或历史交付状态。
- 规格引用 ADR，不复制 ADR 的决策正文；架构改变必须同步 ARCHITECTURE 和相关 ADR。
- API 先改 `api/openapi.yaml`，再生成代码和 devmock；规格不能成为第二份字段真源。
- 验收要区分自动化、集成、真实服务、浏览器/客户端和真机证据；`BUILD SUCCESSFUL` 不等于功能验收。

模板见 [`_template.md`](_template.md)。
