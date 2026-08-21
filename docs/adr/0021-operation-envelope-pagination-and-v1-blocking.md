# ADR-0021：操作信封分页与 v1 阻断

## 状态

已接受

## 背景

ADR-0020 定义了 v2 的普通变更/操作信封联合流，但未冻结分页终止字段，也未定义 v1 请求跨越既有 operation envelope 的保护行为。仅排除信封而继续返回更高 seq，会让旧节点将 watermark 静默推进越过未应用的原子操作。

## 决策

1. `protocol=v2` 响应固定为 `{"records":[...],"latestSeq":N,"hasMore":bool}`。`records` 按全局 seq 升序，普通 change 与完整 operation envelope 均计作一个 record；envelope 不受普通 `limit` 拆分限制，最大为 500 项。`hasMore` 表示在同一读取快照中是否仍存在 seq 大于最后返回 record 的记录；`latestSeq` 仅是远端观察值。
2. v2 客户端逐 record 成功应用后才持久化该 record 的 seq。它以 `hasMore` 决定是否继续拉取，不得用 `latestSeq`、满页或空页直接跳过未应用记录；空 `records` 且 `hasMore=false` 才表示本次快照已追平。
3. 任一 v1 `sync/pull` 的 `since` 范围若包含 completed operation envelope，服务端必须返回 `409 cluster_peer_upgrade_required`，不返回跨越该 envelope 的 `changes` 或 `latestSeq`。该规则同样用于新增、恢复或降级为 v1 的已配置对端检查。
4. ADR-0020 的 v2 协商与“提交前所有活动对端支持 v2”规则保持不变；本 ADR 是其分页和旧节点安全边界的补充。

## 理由

- 显式 `hasMore` 解除“envelope 恰好填满 limit”与并发新增记录的歧义。
- v1 快速失败使旧节点不能通过高水位跳过不可拆分的原子操作，保留可诊断的升级路径。

## 后果

- 协议测试必须覆盖满页 envelope、空页追平、并发新增、断网重试、v1 跨 envelope 拒绝和历史恢复节点接入。
- 集群运行原子资产操作前需要确认所有活动节点已升级到 v2；普通 v1 变更同步仍保持兼容。
