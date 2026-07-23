# ADR-0010：proxy 回源缓存、single-flight 并发收敛与 group 有序路由

## 状态

已接受

## 背景

0.3.0 在 Raw hosted 之上补齐 proxy（远程仓库缓存）与 group（多仓聚合）两种类型的**读路径**（FR-13 / FR-17 / FR-18）。需要决定三件事：

1. **proxy 未命中如何回源与缓存**：本地无缓存时向上游 `remoteUrl` 拉取，结果如何落盘复用。
2. **并发回源如何收敛**：同一制品被多个请求同时首次拉取时，如何避免重复下载与重复写盘（FR-17）。
3. **group 如何在成员间路由**：多个成员仓库中如何选出返回结果，成员故障如何处理。

约束：复用 ADR-0009 的 blob 内容寻址与 `asset` 元数据表（回源结果即普通缓存条目）；分层恒为 `protocol → domain → repository/blobstore/upstream → persistence`；写路径（Put/Delete）仍仅限 hosted，proxy/group 写返回 409。

## 决策

1. **proxy = 本地缓存优先 + 未命中回源缓存**：读请求先查本地 `asset`，命中即返回；未命中经新增 `internal/upstream.Client.Fetch(ctx, remoteURL, path)` 拉取，**流式**写入 blob（边写边算 sha256，不整体入内存，FR-18 复验）并 upsert `asset` 后从本地读返回。上游 404 视为未命中（→ `ErrNotFound`/404），超时 → `ErrUpstreamTimeout`/504，其余失败 → `ErrUpstream`/502。不做 TTL 失效与负缓存（命中即返回）。
2. **single-flight 收敛并发回源**：以 `repoID\x00path` 为键经 `golang.org/x/sync/singleflight` 收敛同一制品的并发首次回源，临界区内先复查缓存，保证 N 个并发请求对同一未命中路径只回源一次（FR-17）。
3. **group = 成员有序解析、首个命中即返回**：按 `members` 配置顺序逐一递归 `resolve`（成员可为 hosted/proxy/group），首个成功即返回；成员 `ErrNotFound` 或回源失败均跳过继续下一成员，全部未命中 → 404。递归以 `maxResolveDepth` 遏制成员环引用。

## 理由

- 回源结果复用既有 blob + asset，天然去重并获得内容寻址完整性（ETag=sha256），proxy 缓存无需额外表或存储路径。
- single-flight 是 Go 生态收敛「同键并发首次计算」的标准做法，键含 `repoID` 避免跨仓串扰；临界区复查缓存兼顾「首个请求已写盘、后续请求进入临界区」的窗口。
- group 有序、故障跳过让聚合读对单成员故障有韧性（一个 proxy 上游挂掉不阻断其它成员命中），符合「聚合仓库尽力返回」的直觉；深度上限防止 group 互相引用形成无限递归。

## 后果

- proxy 不即时失效缓存：上游更新后本地仍返回旧副本，直至该 asset 被显式删除或后续增量引入 TTL/失效策略；GC 仍不即时（同 ADR-0009）。
- group 成员回源错误被静默跳过（不阻断聚合），因此 group 读路径不会向客户端暴露 502/504；这些状态仅出现在直接读 proxy 仓库时。
- `AssetService` 读路径统一收敛到 `Resolve(ctx, repoName, path)` 按 `repo.Type` 分派；hosted-only 部署可传 `nil` upstream（proxy 读将直接判未命中）。`Get` 保留为 hosted 本地读的窄接口。

## 备选方案

- **proxy 带 TTL/条件请求（If-Modified-Since / ETag 回源校验）**：被否（本增量），先交付「命中即返回」的最小可用缓存，失效策略留待后续增量，避免过早引入时钟与协商复杂度。
- **每请求独立回源（不做 single-flight）**：被否，冷启动或热点制品并发首拉会造成重复下载与写盘竞争，浪费上游带宽与本地 IO。
- **group 并发扇出取最快命中**：被否，成员有序语义更可预测（近端成员优先），且避免并发回源多个上游带来的放大效应；有序遍历实现更简单。
