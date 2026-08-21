# ADR-0020：版本化原子操作复制信封

## 状态

已接受

## 背景

ADR-0019 要求 completed 操作 outbox 分配全局复制序号并以不可分割信封拉取，但 FR-84 已交付的 `sync/pull` 只返回 `repl_change[]`，客户端逐条应用并推进最后序号。没有版本协商时，旧客户端无法识别操作信封，分页也可能越过未完整交付的批次。

## 决策

1. 保持 FR-84 的 v1 `changes: repl_change[]` 响应不变，继续只承载普通变更。新增 `GET /api/v1/cluster/sync/pull?...&protocol=v2`：响应按全局 `seq` 排序的 `records` 联合数组，元素为普通 `change` 或单个不可分割的 `operation` envelope。
2. completed operation outbox 在与业务视图同一事务中分配一个全局 `seq`。其中的逐项变更不单独出现在 `records`；一个 envelope 即使含至多 500 项，也不得被普通 `limit` 分页拆分。v2 客户端仅在整个 envelope 校验、补 blob、原子应用成功后，才将 watermark 推进到该 `seq`。
3. v0.8 节点使用 v2 拉取并通过响应形态确认对端能力。发起原子资产操作前，所有已配置活动对端必须支持 v2；任一对端不支持时返回 `409 cluster_peer_upgrade_required`，不创建本地操作、不分配 outbox seq。v1 客户端只可继续同步不含原子操作的普通变更。
4. v2 响应中普通 change 与 operation envelope 使用同一全局序列排序。断网、分页、重试和重复拉取均以最后完整成功 record 的 seq 作为 cursor；不得以 `latestSeq` 跳过尚未成功应用的 envelope。

## 理由

- 保留 v1 防止已部署节点因升级立即失去普通复制能力。
- 以单个 ledger seq 表示操作信封，避免逐项 seq 被分页切开，同时维持既有 watermark 总序。
- 在提交前拒绝存在旧对端的原子操作，比让旧节点静默跳过或半应用更符合集群一致性与审计要求。

## 后果

- 复制协议、客户端分派和对端能力探测需要扩展；v0.8 原子资产操作需要集群节点完成 v2 升级。
- 测试必须覆盖 v1 兼容、v2 普通/操作记录交错、分页边界、断网重试、能力不足拒绝和 watermark 安全推进。

## 备选方案

- 让 v1 返回操作内的逐项 `repl_change`：被否，会重新引入半批次可见性。
- 直接改变默认 v1 响应：被否，会破坏已部署节点的反序列化与 cursor 语义。
