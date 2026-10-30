package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/sync/singleflight"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// maxResolveDepth 限制 group 成员递归解析深度，防止成员相互引用形成环导致的无限递归。
const maxResolveDepth = 16

// CacheOutcome 表示一次 proxy 读取的缓存来源；空串表示无法判定或非 proxy。
type CacheOutcome string

const (
	// CacheOutcomeUnknown 表示无法可靠判定（hosted/group，或解析未命中判定点），不参与命中率统计。
	CacheOutcomeUnknown CacheOutcome = ""
	// CacheOutcomeHit 表示直接命中本地已缓存副本，未访问上游。
	CacheOutcomeHit CacheOutcome = "hit"
	// CacheOutcomeMiss 表示本地未命中、需要回源拉取。
	CacheOutcomeMiss CacheOutcome = "miss"
)

// cacheOutcomeContextKey 是缓存来源记录器的 context 键（未导出，避免跨包误用）。
type cacheOutcomeContextKey struct{}

// cacheOutcomeRecorder 收集单次请求内 proxy 解析产生的缓存来源；并发回源时可能被多个 goroutine 写入，故加锁。
// sealed 用于冻结 group 等不可靠场景：一旦冻结，成员级的 hit/miss 不再写入，整体保持未知。
type cacheOutcomeRecorder struct {
	mu      sync.Mutex
	outcome CacheOutcome
	sealed  bool
}

func (r *cacheOutcomeRecorder) record(outcome CacheOutcome) {
	r.mu.Lock()
	if !r.sealed {
		r.outcome = outcome
	}
	r.mu.Unlock()
}

func (r *cacheOutcomeRecorder) seal() {
	r.mu.Lock()
	r.sealed = true
	r.outcome = CacheOutcomeUnknown
	r.mu.Unlock()
}

// WithCacheOutcome 在 ctx 上挂载缓存来源记录器，协议层借此读回本次 proxy 读取的 hit/miss。
// 未挂载时 recordCacheOutcome 为无操作，因此后台任务与内部调用无需关心。
func WithCacheOutcome(ctx context.Context) context.Context {
	return context.WithValue(ctx, cacheOutcomeContextKey{}, &cacheOutcomeRecorder{})
}

// CacheOutcomeFromContext 读回 WithCacheOutcome 挂载的缓存来源；未挂载或未判定返回 CacheOutcomeUnknown。
func CacheOutcomeFromContext(ctx context.Context) CacheOutcome {
	if r, ok := ctx.Value(cacheOutcomeContextKey{}).(*cacheOutcomeRecorder); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.outcome
	}
	return CacheOutcomeUnknown
}

// ResolveFailureStatus 表示一次解析失败是否可以确认资源不存在。
type ResolveFailureStatus string

const (
	// ResolveFailureUnknown 表示没有可用的失败判定。
	ResolveFailureUnknown ResolveFailureStatus = ""
	// ResolveFailureConfirmed 表示已确认上游或本地资源不存在。
	ResolveFailureConfirmed ResolveFailureStatus = "confirmed"
	// ResolveFailureUncertain 表示失败可能由离线、阻止窗口或上游故障导致。
	ResolveFailureUncertain ResolveFailureStatus = "uncertain"
)

type resolveFailureContextKey struct{}

type resolveFailureRecorder struct {
	mu     sync.Mutex
	status ResolveFailureStatus
}

// WithResolveFailure 为一次解析挂载失败判定记录器，协议层可据此避免重复探测不确定路径。
func WithResolveFailure(ctx context.Context) context.Context {
	return context.WithValue(ctx, resolveFailureContextKey{}, &resolveFailureRecorder{})
}

// ResolveFailureFromContext 读取当前解析失败判定；未记录时返回未知。
func ResolveFailureFromContext(ctx context.Context) ResolveFailureStatus {
	if r, ok := ctx.Value(resolveFailureContextKey{}).(*resolveFailureRecorder); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.status
	}
	return ResolveFailureUnknown
}

func recordResolveFailure(ctx context.Context, status ResolveFailureStatus) {
	if r, ok := ctx.Value(resolveFailureContextKey{}).(*resolveFailureRecorder); ok {
		r.mu.Lock()
		if r.status != ResolveFailureUncertain {
			r.status = status
		}
		r.mu.Unlock()
	}
}

// upstreamStartedContextKey 是「上游已开始」信号的 context 键（未导出，避免跨包误用）。
type upstreamStartedContextKey struct{}

// upstreamStartedRecorder 持有一条解析链路的「上游已开始」信号：链路上有上游返回 2xx 响应头即置位。
// parent 用于嵌套 group 向上传递——内层成员拿到响应头同样证明外层成员持有该制品，
// 否则外层看门狗会在内层传输途中误取消（嵌套 group 是最常见的 maven-public 配置）。
type upstreamStartedRecorder struct {
	once   sync.Once
	ch     chan struct{}
	parent *upstreamStartedRecorder
}

// fire 幂等地置位信号（多次触发不阻塞、不 panic），并向上层记录器传递。
func (r *upstreamStartedRecorder) fire() {
	r.once.Do(func() {
		close(r.ch)
		if r.parent != nil {
			r.parent.fire()
		}
	})
}

// WithUpstreamStarted 为一次解析挂载「上游已开始」信号，返回的通道在该链路收到首个上游响应头时关闭。
//
// group 读借此把成员预算从「总预算」改为「首字节看门狗」：响应头一到即撤销成员预算，
// 剩余落盘预算交给回源整体超时（见 groupGet 与 ）。未挂载时 markUpstreamStarted
// 是无操作，因此代理直连、后台探测、格式元数据回退等调用方无需感知本机制。
func WithUpstreamStarted(ctx context.Context) (context.Context, <-chan struct{}) {
	parent, _ := ctx.Value(upstreamStartedContextKey{}).(*upstreamStartedRecorder)
	r := &upstreamStartedRecorder{ch: make(chan struct{}), parent: parent}
	return context.WithValue(ctx, upstreamStartedContextKey{}, r), r.ch
}

// markUpstreamStarted 报告本条解析链路已收到上游 2xx 响应头（即上游确实持有该制品）。
// 无记录器时静默忽略，语义与 recordResolveFailure 等 ctx 记录器一致。
func markUpstreamStarted(ctx context.Context) {
	if r, ok := ctx.Value(upstreamStartedContextKey{}).(*upstreamStartedRecorder); ok {
		r.fire()
	}
}

// recordCacheOutcome 把一次 proxy 解析的缓存来源写入 ctx 上的记录器；无记录器时静默忽略。
// 判定点就在各 proxy 读路径「先查本地、未命中再回源」的分叉处，因此只对 proxy 仓库产生非空值。
func recordCacheOutcome(ctx context.Context, outcome CacheOutcome) {
	if r, ok := ctx.Value(cacheOutcomeContextKey{}).(*cacheOutcomeRecorder); ok {
		r.record(outcome)
	}
}

// sealCacheOutcome 冻结本次请求的缓存来源为未知：group 聚合读的首个命中成员无法在解析层可靠判定，
// 因此 group 及其成员回源产生的成员级 hit/miss 不得暴露为整体结果（宁可未知也不猜）。
func sealCacheOutcome(ctx context.Context) {
	if r, ok := ctx.Value(cacheOutcomeContextKey{}).(*cacheOutcomeRecorder); ok {
		r.seal()
	}
}

// AssetService 编排制品的发布与拉取：元数据落 asset 表，内容落内容寻址 blob。
//
// 分层（见 internal/doc.go）：domain -> repository, blobstore, upstream。协议层
// （internal/protocol）经此服务读写制品，不直接触碰 SQLite、文件系统或上游 HTTP。
//
// 读路径按仓库 Type 分派（见 Resolve）：hosted 读本地缓存；proxy 命中即返回，
// 未命中经 upstream 回源并缓存（single-flight 收敛并发回源）；group 先本地快查、
// 未命中则并行回源（见 groupGet），并对不可达上游做 auto-block 短窗阻止（见 proxyHealth）。
type AssetService struct {
	repos     *repository.RepoRepo
	assets    *repository.AssetRepo
	blobs     *blobstore.Store
	upstream  *upstream.Client
	mutator   *AssetMutationCoordinator
	writeGate BusinessWriteGate
	sf        singleflight.Group
	recorder  ChangeRecorder
	nodeID    func() string
	health    *proxyHealth
	negCache  *negativeCache // 404 负缓存（proxy/group 读路径短窗缓存明确 404）
	// groupBudget 是 group 逐成员并行解析的**确认阶段**预算（首字节看门狗时长）。
	// 由常量改为实例字段的原因：JIAN_GROUP_MEMBER_TIMEOUT 需在装配期注入、测试也要能缩短。
	// 由 groupBudgetMu 保护读写（读取见 memberConfirmBudget）。
	groupBudgetMu sync.Mutex
	groupBudget   time.Duration
	// failureRecorder 记录解析失败的最终结论（指标维度）；nil 表示不记录。
	failureRecorder ResolveFailureRecorder
	// upstreamHealth 记录上游健康采样；nil 表示不采样。
	// 与 health（内存态「此刻」）并存：两者一个回答现在通不通，一个回答过去怎么通。
	upstreamHealth *UpstreamHealthService
}

// NewAssetService 构造 AssetService。upstream 供 proxy 回源使用（hosted-only 部署可传 nil）。
func NewAssetService(repos *repository.RepoRepo, assets *repository.AssetRepo, blobs *blobstore.Store, up *upstream.Client) *AssetService {
	mutator := NewAssetMutationCoordinatorDeferred(assets.DB(), blobs)
	svc := &AssetService{
		repos: repos, assets: assets, blobs: blobs, upstream: up, mutator: mutator,
		negCache:    newNegativeCache(),
		groupBudget: groupMemberTimeoutDefault,
	}
	svc.health = newProxyHealth(svc.probeUpstream)
	return svc
}

// SetMutationCoordinator 注入与其它写路径共享的节点级协调器。
func (s *AssetService) SetMutationCoordinator(c *AssetMutationCoordinator) { s.mutator = c }

// SetStorageQuotaCheck 注入仓库级存储配额的提交点权威复检。
// nil 表示不做存储配额判定（兼容既有装配与只读部署）。
func (s *AssetService) SetStorageQuotaCheck(check StorageQuotaGuard) {
	if s.mutator != nil {
		s.mutator.SetStorageQuotaCheck(check)
	}
}

// MutationCoordinator 返回当前节点共享的资产生命周期协调器，供同一装配图中的写服务复用。
func (s *AssetService) MutationCoordinator() *AssetMutationCoordinator { return s.mutator }

// RecoverMutationIntents 显式处理启动时遗留的全部资产 intent，仅供可写节点。
func (s *AssetService) RecoverMutationIntents() error {
	if s.mutator == nil {
		return nil
	}
	return s.mutator.Recover()
}

// RecoverReceivedMutationIntents 仅恢复复制接收产生的 intent，供 standby 启动时
// 解除崩溃前 prepared/staged/committing 批次；旧角色本地 intent 保持不动。
func (s *AssetService) RecoverReceivedMutationIntents() error {
	if s.mutator == nil {
		return nil
	}
	return s.mutator.RecoverReceived()
}

// CleanupUnreferencedBlobs 清理由失败写入遗留且没有任何活动资产引用的 blob。
// 该方法仅由 primary 的定时任务调用，服务启动阶段不得调用。
func (s *AssetService) CleanupUnreferencedBlobs() (int, error) {
	if s.mutator == nil {
		return 0, nil
	}
	removed := 0
	err := s.blobs.WalkActive(func(hash string) error {
		if err := s.mutator.RemoveIfUnreferenced(hash); err != nil {
			return err
		}
		if !s.blobs.Exists(hash) {
			removed++
		}
		return nil
	})
	return removed, err
}

// SetBusinessWriteGate 注入本地业务写栅栏。复制接收直接由 ReplicationService 应用，
// 不经 AssetService，因此不会被备用节点只读策略拦截。
func (s *AssetService) SetBusinessWriteGate(gate BusinessWriteGate) { s.writeGate = gate }

func (s *AssetService) requireBusinessWrite() error {
	return requireBusinessWrite(s.writeGate)
}

// SetAutoBlockBase 调整 auto-block 退避起始时长（默认 40s；装配层注入配置、测试与调优用，<=0 忽略）。
func (s *AssetService) SetAutoBlockBase(d time.Duration) { s.health.setBase(d) }

// SetUpstreamBlockRecorder 注入上游断路器阻止事件记录器；nil 表示不记录。
func (s *AssetService) SetUpstreamBlockRecorder(r UpstreamBlockRecorder) {
	s.health.setBlockRecorder(r)
}

// BlockedUpstreamCount 返回当前处于阻止态（AUTO_BLOCKED / HALF_OPEN）的上游仓库数。
// 供装配层把它接进 /metrics 的 jianartifact_upstream_blocked_repositories 仪表；
// 只读内存态，不查库、不访问网络。
func (s *AssetService) BlockedUpstreamCount() int { return s.health.blockedCount() }

// SetUpstreamHealthService 注入上游健康采样服务；nil 表示不采样。
// 与 SetUpstreamBlockRecorder 同一装配范式：领域定义接口、装配层注入、nil 不记录。
// 采样只写「历史序列」，**不参与任何状态判定**——断路器仍只由 settleUpstreamFailure 决定。
func (s *AssetService) SetUpstreamHealthService(h *UpstreamHealthService) { s.upstreamHealth = h }

// ProbeOnly 发起一次上游 HEAD 探测并返回状态码，**不触碰断路器状态机**。
//
// 与 probeUpstream 的区别只有一个，但很关键：probeUpstream 是断路器内部用的探测判据
// （只把 err 交给状态机决定「成功/失败」）；本方法把**原始状态码**交回调用方，
// 让定时探测作业能按  的五分类记下「是超时、还是 5xx、还是 DNS 失败」，
// 同时不调用任何 record 系列方法。走的是同一个客户端与同一条凭据解析路径
// （upstream.Client.ProbeWithCredential），因此不存在「探测与回源两套口径」。
func (s *AssetService) ProbeOnly(ctx context.Context, remoteURL, credentialRef string) (int, error) {
	if s == nil || s.upstream == nil {
		return 0, ErrUpstream
	}
	if remoteURL == "" {
		return 0, ErrUpstream
	}
	return s.upstream.ProbeWithCredential(ctx, remoteURL, credentialRef)
}

// SetNegativeCacheTTL 调整 404 负缓存 TTL（默认 60s）。
//
// 0 = **关闭负缓存**（每个请求都真实回源 / 探测成员），负数忽略（配置层已回落默认，
// 这里是第二道保险）。装配层在 NewAssetService 之后、第一个请求之前注入
// JIAN_NEGATIVE_CACHE_TTL 的解析结果；测试与调优也用它。
func (s *AssetService) SetNegativeCacheTTL(d time.Duration) { s.negCache.setTTL(d) }

// SetGroupMemberTimeout 调整 group 成员**确认阶段**预算（默认 3s；非正数忽略）。
//
// 只影响「该成员是否及时给出上游响应头」这一件事，不约束响应体传输——落盘预算仍由
// 回源整体超时（JIAN_UPSTREAM_TIMEOUT）承担，见 groupGet 的首字节看门狗。
// 装配层在 NewAssetService 之后、第一个请求之前注入 JIAN_GROUP_MEMBER_TIMEOUT 的解析结果。
func (s *AssetService) SetGroupMemberTimeout(d time.Duration) {
	s.groupBudgetMu.Lock()
	defer s.groupBudgetMu.Unlock()
	if d > 0 {
		s.groupBudget = d
	}
}

// memberConfirmBudget 返回当前的成员确认阶段预算，是看门狗**唯一**的取值来源。
//
// 读路径加锁的理由：热改与在途 group 读之间是真实的数据竞态（-race 会抓到），
// 与 negativeCache 保护 ttl 的做法同构。
func (s *AssetService) memberConfirmBudget() time.Duration {
	s.groupBudgetMu.Lock()
	defer s.groupBudgetMu.Unlock()
	if s.groupBudget <= 0 {
		// 兜底：零值构造（未走 NewAssetService）不得让预算变成「立刻超时」，
		// 那会把整条 group 读退化成不可判定失败。
		return groupMemberTimeoutDefault
	}
	return s.groupBudget
}

// Status 返回 proxy 仓库上游连接状态（供状态展示）。
func (s *AssetService) Status(repoID int64) RemoteHealth { return s.health.status(repoID) }

// RecheckConnection 手动重测 proxy 仓库上游连接：立即 HEAD 探测并更新
// auto-block 状态，返回最新状态视图。仅 online 仓库可调用（调用方负责校验）。
func (s *AssetService) RecheckConnection(repoID int64, remoteURL string) RemoteHealth {
	credentialRef := ""
	if repo, err := s.repos.GetByID(repoID); err == nil {
		if cfg, err := repo.DecodeConfig(); err == nil && cfg.RemoteURL != "" {
			remoteURL = cfg.RemoteURL
			credentialRef = cfg.CredentialRef
		}
	}
	return s.health.recheck(repoID, remoteURL, credentialRef)
}

// SetChangeRecorder 注入复制变更日志记录器；nil 表示不记录。
// 记录器为 ReplicationService 时，顺带把本服务的 404 负缓存注入对端共享，
// SetChangeRecorder 注入变更记录器（复制退役后为 no-op 实现，保留注入点）。
// 另需 SetNodeIdentity 提供节点稳定标识：原子 operation 信封的 version 要用它。
func (s *AssetService) SetChangeRecorder(r ChangeRecorder) { s.recorder = r }

// SetNodeIdentity 注入节点稳定标识（信封 version.NodeID 必须跨重启稳定）。
func (s *AssetService) SetNodeIdentity(n *NodeIdentity) { s.nodeID = n.NodeID }

// NodeID 返回节点稳定标识（供格式元数据等信封构造方使用）。
// 未接线时回退 "local"：单元测试直接构造 AssetService 的场景兜底；
// 生产装配必须经 SetNodeIdentity 注入真实标识（wiring_publish_test 守护）。
func (s *AssetService) NodeID() string {
	if s.nodeID != nil {
		return s.nodeID()
	}
	return "local"
}

// Put 向 hosted 仓库发布一件制品：流式写入 blob，再覆盖写 asset 元数据。
// 仓库不存在返回 ErrNotFound；非 hosted 仓库（proxy/group）返回 ErrConflict。
// 格式（raw/maven/npm）语义由协议层按仓库 format 分派后自行处理，此处仅按内容寻址存字节。
func (s *AssetService) Put(repoName, path string, r io.Reader, contentType string) (*repository.Asset, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return nil, ErrConflict
	}
	// 复制退役后所有节点都是普通节点：发布一律走原子 operation 信封路径。
	return s.putWithPrimaryOperation(repoName, repo, path, r, contentType, false)
}

// putWithPrimaryOperation 将主节点的单制品发布写入不可拆分的 v2 operation，
// 避免 standby 仅拉取 v2 时遗漏仍停留在旧变更流中的协议发布。
// quotaExempt 为 true 时该资产在提交点豁免仓库存储配额（仅迁移导入 / 备份恢复）。
func (s *AssetService) putWithPrimaryOperation(repoName string, repo *repository.Repository, path string, r io.Reader, contentType string, quotaExempt bool) (*repository.Asset, error) {
	asset, err := s.StageBlob(r, contentType)
	if err != nil {
		return nil, err
	}
	asset.RepositoryID = repo.ID
	asset.Path = path
	if quotaExempt {
		markQuotaExempt(asset)
	}
	if _, err := s.PublishAssets(repoName, []*repository.Asset{asset}); err != nil {
		return nil, s.cleanupFailedWrite(asset.BlobHash, err)
	}
	stored, err := s.assets.GetByPath(repo.ID, path)
	if err != nil {
		return nil, err
	}
	s.negCache.remove(repo.ID, path)
	return stored, nil
}

// PutWithTimestamps 与 Put 语义相同，但将资产 created_at/updated_at 固定为源端时间，
// 用于在线迁移保留源 Nexus 资产的时间戳。sourceModified 为零值时回退到 Put（使用本地当前时间），
// 离线路径或缺失时间戳时语义等同 Put。时间按 UTC "YYYY-MM-DD HH:MM:SS" 写入，
// 与 asset 表 datetime('now') 默认值格式一致。
//
// 本方法只服务迁移导入（管理员批量操作），因此**显式豁免仓库存储配额**：
// 迁移导入属可预估的批量管理员操作，配额不阻断它，但提交点会记日志。
func (s *AssetService) PutWithTimestamps(repoName, path string, r io.Reader, contentType string, sourceModified time.Time) (*repository.Asset, error) {
	if sourceModified.IsZero() {
		return s.putWithPrimaryOperationExemptQuota(repoName, path, r, contentType)
	}
	return s.putWithCommitHook(repoName, path, r, contentType, func(asset *repository.Asset) repository.MutationCompletionHook {
		// 源端时间写入资产行：created_at 与 updated_at 均与源一致（用户要求迁移后两时间都对齐源）。
		applySourceTimestamp(asset, sourceModified)
		return func(tx *sqlx.Tx) error { return nil }
	}, true)
}

// putWithPrimaryOperationExemptQuota 与 Put 相同，但显式豁免仓库存储配额。
// 仅供迁移导入 / 备份恢复等管理员批量操作使用；协议发布路径必须走 Put。
func (s *AssetService) putWithPrimaryOperationExemptQuota(repoName, path string, r io.Reader, contentType string) (*repository.Asset, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return nil, ErrConflict
	}
	return s.putWithPrimaryOperation(repoName, repo, path, r, contentType, true)
}

// applySourceTimestamp 将资产行 created_at/updated_at 固定为源端时间的 UTC
// "YYYY-MM-DD HH:MM:SS"（与 asset 表 datetime('now') 默认值格式一致）；
// 零值不修改，保持 datetime('now') 本地时间语义。供各格式迁移导入器在
// PublishAssets 批次发布前标注资产行时间。
func applySourceTimestamp(asset *repository.Asset, sourceModified time.Time) {
	if asset == nil || sourceModified.IsZero() {
		return
	}
	ts := sourceModified.UTC().Format("2006-01-02 15:04:05")
	asset.CreatedAt = ts
	asset.UpdatedAt = ts
}

// PutWithCommitHook 将内容写入 blob 后，与调用方的同库元数据一起完成资产事务。
// 附加事务失败时，资产视图与新写入的无引用 blob 都不会对外保留。
func (s *AssetService) PutWithCommitHook(repoName, path string, r io.Reader, contentType string, hook func(*repository.Asset) repository.MutationCompletionHook) (*repository.Asset, error) {
	return s.putWithCommitHook(repoName, path, r, contentType, hook, false)
}

// putWithCommitHook 是 PutWithCommitHook 的共享实现；quotaExempt 为 true 时该资产在
// 提交点豁免仓库存储配额（仅迁移导入 / 备份恢复等管理员批量操作）。
func (s *AssetService) putWithCommitHook(repoName, path string, r io.Reader, contentType string, hook func(*repository.Asset) repository.MutationCompletionHook, quotaExempt bool) (*repository.Asset, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Type != "hosted" || hook == nil || s.mutator == nil {
		return nil, ErrConflict
	}
	hash, sha1sum, md5sum, size, err := s.putStagedBlob(r)
	if err != nil {
		return nil, err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	asset := &repository.Asset{RepositoryID: repo.ID, Path: path, BlobHash: hash, Size: size, ContentType: contentType, Sha1: sha1sum, Md5: md5sum}
	if quotaExempt {
		markQuotaExempt(asset)
	}
	if err := s.mutator.PutWithCommitHook(asset, hook(asset)); err != nil {
		return nil, s.cleanupFailedWrite(hash, err)
	}
	s.releaseStagedBlob(hash)
	stored, err := s.assets.GetByPath(repo.ID, path)
	if err != nil {
		return nil, err
	}
	s.negCache.remove(repo.ID, path)
	s.recordAssetPut(repo.Name, stored)
	return stored, nil
}

// StageBlob 将内容写入内容寻址存储，但不建立可见资产引用。
// 仅调用方的原子资产提交成功后，返回的资产才能对外可见。
func (s *AssetService) StageBlob(r io.Reader, contentType string) (*repository.Asset, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	hash, sha1sum, md5sum, size, err := s.putStagedBlob(r)
	if err != nil {
		return nil, err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return &repository.Asset{BlobHash: hash, Size: size, ContentType: contentType, Sha1: sha1sum, Md5: md5sum}, nil
}

// StageOCIBlob 保留 OCI 上传调用的兼容入口。
func (s *AssetService) StageOCIBlob(r io.Reader, contentType string) (*repository.Asset, error) {
	return s.StageBlob(r, contentType)
}

func (s *AssetService) putStagedBlob(r io.Reader) (string, string, string, int64, error) {
	if s.mutator == nil {
		return s.blobs.Put(r)
	}
	return s.blobs.PutStaged(r)
}

func (s *AssetService) releaseStagedBlob(hash string) {
	if s.mutator != nil {
		s.blobs.ReleaseStaged(hash)
	}
}

// PublishAssets 将一组已落盘 blob 的资产引用一次性公开。
// 当复制服务可用时，同一 SQLite 事务会同步写入一个 v2 operation outbox。
func (s *AssetService) PublishAssets(repoName string, after []*repository.Asset) (string, error) {
	return s.publishAssets(repoName, after, nil)
}

// PublishAssetsWithCommitHook 将一组资产和附加元数据作为同一事务公开。
func (s *AssetService) PublishAssetsWithCommitHook(repoName string, after []*repository.Asset, hook repository.MutationCompletionHook) (string, error) {
	if hook == nil {
		return "", ErrValidation
	}
	return s.publishAssets(repoName, after, hook)
}

func (s *AssetService) publishAssets(repoName string, after []*repository.Asset, hook repository.MutationCompletionHook) (string, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return "", err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return "", mapNotFound(err)
	}
	proxyMetadataBatch := (repo.Format == "pypi" || repo.Format == "nuget") && repo.Type == "proxy" && hook != nil
	if (repo.Type != "hosted" && !proxyMetadataBatch) || len(after) == 0 {
		return "", ErrConflict
	}
	items, err := s.publishMutationItems(repo, after)
	if err != nil {
		return "", err
	}
	operationID := newOperationID()
	if repo.Type == "hosted" {
		nid := s.NodeID()
		envelope, envelopeErr := ociOperationEnvelope(nid, repoName, operationID, items)
		if envelopeErr != nil {
			return "", envelopeErr
		}
		if err := s.mutator.ApplyWithOutboxAndHook(items, envelope, hook); err != nil {
			return "", err
		}
	} else if _, err := s.mutator.ApplyWithOperationIDAndHook(operationID, items, hook); err != nil {
		return "", err
	}
	s.releasePublishedBlobs(after)
	return operationID, nil
}

// PublishAssetsWithEnvelope 将资产、格式元数据等同批逻辑实体和源端审计绑定到一个 v2 operation。
// 调用方必须提供包含每个资产项的完整 operation 清单；不额外记录可拆分的 v1 asset change。
func (s *AssetService) PublishAssetsWithEnvelope(repoName string, after []*repository.Asset, envelope repository.OperationEnvelope, hook repository.MutationCompletionHook) (string, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return envelope.OperationID, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return envelope.OperationID, mapNotFound(err)
	}
	if repo.Type != "hosted" && !((repo.Format == "pypi" || repo.Format == "nuget") && repo.Type == "proxy") {
		return envelope.OperationID, ErrConflict
	}
	if hook == nil || envelope.OperationID == "" {
		return envelope.OperationID, ErrValidation
	}
	items, err := s.publishMutationItems(repo, after)
	if err != nil {
		return envelope.OperationID, err
	}
	if err := s.mutator.ApplyWithOutboxAndHook(items, envelope, hook); err != nil {
		return envelope.OperationID, err
	}
	s.releasePublishedBlobs(after)
	for _, item := range items {
		s.negCache.remove(repo.ID, item.Path)
	}
	return envelope.OperationID, nil
}

func (s *AssetService) releasePublishedBlobs(assets []*repository.Asset) {
	for _, asset := range assets {
		if asset != nil {
			s.releaseStagedBlob(asset.BlobHash)
		}
	}
}

// DiscardStagedAssets 释放未提交批次的暂存内容，并仅在不存在活动引用时删除 blob。
// 调用方在任一预检、暂存或提交失败时调用，避免失败批次留下孤立内容。
func (s *AssetService) DiscardStagedAssets(assets []*repository.Asset) error {
	seen := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		if asset == nil || asset.BlobHash == "" {
			continue
		}
		if _, exists := seen[asset.BlobHash]; exists {
			continue
		}
		seen[asset.BlobHash] = struct{}{}
		s.releaseStagedBlob(asset.BlobHash)
		if s.mutator == nil {
			if err := s.blobs.Remove(asset.BlobHash); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err := s.mutator.RemoveIfUnreferenced(asset.BlobHash); err != nil {
			return err
		}
	}
	return nil
}

func (s *AssetService) publishMutationItems(repo *repository.Repository, after []*repository.Asset) ([]repository.AssetMutationItem, error) {
	if len(after) == 0 {
		return nil, ErrConflict
	}
	items := make([]repository.AssetMutationItem, 0, len(after))
	for _, asset := range after {
		if asset == nil || asset.Path == "" || asset.BlobHash == "" {
			return nil, ErrValidation
		}
		copy := *asset
		copy.RepositoryID = repo.ID
		before, getErr := s.assets.GetByPath(repo.ID, copy.Path)
		if getErr != nil && !errors.Is(getErr, repository.ErrNotFound) {
			return nil, getErr
		}
		if errors.Is(getErr, repository.ErrNotFound) {
			before = nil
		}
		items = append(items, repository.AssetMutationItem{RepositoryID: repo.ID, Path: copy.Path, Before: before, After: &copy})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	if err := validateUniqueAssetPaths(items); err != nil {
		return nil, err
	}
	return items, nil
}

// PublishOCIAssets 保留 OCI 发布调用的兼容入口。
func (s *AssetService) PublishOCIAssets(repoName string, after []*repository.Asset) (string, error) {
	return s.PublishAssets(repoName, after)
}

func validateUniqueAssetPaths(items []repository.AssetMutationItem) error {
	for i := 1; i < len(items); i++ {
		if items[i-1].Path == items[i].Path {
			return ErrValidation
		}
	}
	return nil
}

func ociOperationEnvelope(nodeID, repoName, operationID string, items []repository.AssetMutationItem) (repository.OperationEnvelope, error) {
	version := repository.ReplicationVersion{NodeID: nodeID, TS: time.Now().UTC().Format(time.RFC3339Nano)}
	operationItems := make([]repository.OperationItem, 0, len(items))
	for _, item := range items {
		data, err := json.Marshal(AssetChangeData{Path: item.After.Path, BlobHash: item.After.BlobHash, Size: item.After.Size,
			ContentType: item.After.ContentType, Sha1: item.After.Sha1, Md5: item.After.Md5})
		if err != nil {
			return repository.OperationEnvelope{}, err
		}
		operationItems = append(operationItems, repository.OperationItem{Type: EntityAsset, Key: AssetKey(repoName, item.After.Path),
			Op: OpPut, Data: string(data), BlobHashes: []string{item.After.BlobHash}, DependsOn: []string{RepoKey(repoName)}, Version: version})
	}
	manifest, err := repository.OperationManifestSHA256(operationItems)
	if err != nil {
		return repository.OperationEnvelope{}, err
	}
	return repository.OperationEnvelope{Kind: "operation", OperationID: operationID, SourceNode: version.NodeID,
		Version: version, ItemCount: len(operationItems), ManifestSHA256: manifest, Items: operationItems}, nil
}

// CacheVerifiedPyPIProxy 将已由 PyPI 专用上游链路读取的文件落入 proxy 本地投影。
// 调用方必须在传入前完成 URL 边界校验；该方法仅接受声明哈希完全匹配的内容。
func (s *AssetService) CacheVerifiedPyPIProxy(repoName, path, expectedSHA256 string, r io.Reader, contentType string) (*repository.Asset, error) {
	return s.cacheVerifiedPyPIProxy(repoName, path, expectedSHA256, r, contentType, nil)
}

// CacheVerifiedPyPIProxyWithCommitHook 将 proxy 包与其格式元数据作为同一资产事务提交。
// 元数据写入失败时，包资产不会对外可见。
func (s *AssetService) CacheVerifiedPyPIProxyWithCommitHook(repoName, path, expectedSHA256 string, r io.Reader, contentType string, hook func(*repository.Asset) repository.MutationCompletionHook) (*repository.Asset, error) {
	if hook == nil {
		return nil, ErrValidation
	}
	return s.cacheVerifiedPyPIProxy(repoName, path, expectedSHA256, r, contentType, hook)
}

func (s *AssetService) cacheVerifiedPyPIProxy(repoName, path, expectedSHA256 string, r io.Reader, contentType string, hook func(*repository.Asset) repository.MutationCompletionHook) (*repository.Asset, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Format != "pypi" || repo.Type != "proxy" {
		return nil, ErrConflict
	}
	if hook != nil && s.mutator == nil {
		return nil, ErrConflict
	}
	hash, sha1sum, md5sum, size, err := s.putStagedBlob(r)
	if err != nil {
		return nil, err
	}
	if hash != expectedSHA256 {
		return nil, s.cleanupFailedWrite(hash, ErrValidation)
	}
	asset, err := s.persistAssetWithHook(repo, path, hash, sha1sum, md5sum, size, contentType, hook)
	if err != nil {
		return nil, s.cleanupFailedWrite(hash, err)
	}
	return asset, nil
}

func (s *AssetService) persistAssetWithHook(repo *repository.Repository, path, hash, sha1sum, md5sum string, size int64, contentType string, hook func(*repository.Asset) repository.MutationCompletionHook) (*repository.Asset, error) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	after := &repository.Asset{RepositoryID: repo.ID, Path: path, BlobHash: hash, Size: size, ContentType: contentType, Sha1: sha1sum, Md5: md5sum}
	if s.mutator != nil {
		var err error
		if hook != nil {
			err = s.mutator.PutWithCommitHook(after, hook(after))
		} else {
			err = s.mutator.Put(after)
		}
		if err != nil {
			return nil, err
		}
	} else if hook != nil {
		return nil, ErrConflict
	} else if err := s.assets.Upsert(repo.ID, path, hash, size, contentType, sha1sum, md5sum); err != nil {
		return nil, err
	}
	s.releaseStagedBlob(hash)
	asset, err := s.assets.GetByPath(repo.ID, path)
	if err != nil {
		return nil, err
	}
	// 写路径成功：同路径此前「确认不存在」的负缓存失效，新制品立即可见。
	s.negCache.remove(repo.ID, path)
	s.recordAssetPut(repo.Name, asset)
	return asset, nil
}

func (s *AssetService) recordAssetPut(repoName string, asset *repository.Asset) {
	if asset == nil {
		return
	}
	s.recordChange(EntityAsset, AssetKey(repoName, asset.Path), OpPut, AssetChangeData{
		Path: asset.Path, BlobHash: asset.BlobHash, Size: asset.Size, ContentType: asset.ContentType, Sha1: asset.Sha1, Md5: asset.Md5,
		CreatedAt: asset.CreatedAt, UpdatedAt: asset.UpdatedAt,
	})
}

// recordChange 记录复制变更日志；记录失败不阻断业务写（复制尽力最终一致，对账兜底，见 ADR-0013）。
func (s *AssetService) recordChange(entityType, entityKey, op string, data any) {
	if s.recorder == nil {
		return
	}
	if err := s.recorder.Record(entityType, entityKey, op, data); err != nil {
		log.Printf("复制变更日志记录失败 entity=%s key=%s op=%s：%v", entityType, entityKey, op, err)
	}
}

// BackfillChecksumsResult 是历史资产 sha1/md5 回填的统计。
type BackfillChecksumsResult struct {
	Scanned   int // 本批扫描条数
	Updated   int // 成功写回条数
	Skipped   int // blob 缺失或校验失败跳过
	Remaining int // 库内仍缺校验和的条数
}

// BackfillChecksums 对 sha1/md5 为空的历史资产从 blob 流式补算并写回（不现算读路径）。
// batch 为单批上限（≤0 默认 500）。可重复调用直至 Remaining=0。
func (s *AssetService) BackfillChecksums(batch int) (*BackfillChecksumsResult, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	if batch <= 0 {
		batch = 500
	}
	list, err := s.assets.ListMissingChecksums(batch)
	if err != nil {
		return nil, err
	}
	res := &BackfillChecksumsResult{Scanned: len(list)}
	for i := range list {
		a := &list[i]
		sha1sum, md5sum, err := s.blobs.Checksums(a.BlobHash)
		if err != nil {
			res.Skipped++
			continue
		}
		if err := s.assets.UpdateChecksums(a.ID, sha1sum, md5sum); err != nil {
			return res, err
		}
		res.Updated++
	}
	remain, err := s.assets.CountMissingChecksums()
	if err != nil {
		return res, err
	}
	res.Remaining = remain
	return res, nil
}

// AssetTimeEntry 是待回填时间的资产条目（时间已格式化为 UTC "YYYY-MM-DD HH:MM:SS"）。
type AssetTimeEntry struct {
	RepoName  string
	Path      string
	CreatedAt string
	UpdatedAt string
}

// BackfillTimesResult 是时间回填的统计。
type BackfillTimesResult struct {
	Scanned int // 传入源条目数
	Updated int // 成功更新时间条数
	Skipped int // 本地无此资产 / 仓库不存在 / 更新失败跳过
}

// BackfillTimes 按 仓库名+路径 回填资产 created_at/updated_at，与源 Nexus 时间对齐。
// batch 为单批上限（≤0 默认 1000）；幂等可重复执行。
func (s *AssetService) BackfillTimes(entries []AssetTimeEntry, batch int) (*BackfillTimesResult, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	if batch <= 0 {
		batch = 1000
	}
	res := &BackfillTimesResult{Scanned: len(entries)}
	repoIDs := make(map[string]int64, 8)
	lookup := func(name string) (int64, bool) {
		if id, ok := repoIDs[name]; ok {
			return id, true
		}
		repo, err := s.repos.GetByName(name)
		if err != nil {
			return 0, false
		}
		repoIDs[name] = repo.ID
		return repo.ID, true
	}
	for i := 0; i < len(entries); i += batch {
		end := i + batch
		if end > len(entries) {
			end = len(entries)
		}
		for _, e := range entries[i:end] {
			repoID, ok := lookup(e.RepoName)
			if !ok {
				res.Skipped++
				continue
			}
			n, err := s.assets.UpdateTimes(repoID, e.Path, e.CreatedAt, e.UpdatedAt)
			if err != nil || n == 0 {
				res.Skipped++
				continue
			}
			res.Updated++
		}
	}
	return res, nil
}

// EmitTimeChanges 为全部 hosted 仓库资产重新记录带创建/更新时间的 put 变更，
// 供对端复制应用后同步时间（幂等：对端已有则更新时间为源值）。返回统计。
func (s *AssetService) EmitTimeChanges() (*BackfillTimesResult, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	res := &BackfillTimesResult{}
	offset := 0
	const pageSize = 100
	for {
		repos, err := s.repos.List(pageSize, offset)
		if err != nil {
			return nil, err
		}
		for i := range repos {
			r := &repos[i]
			if r.Type != "hosted" {
				continue
			}
			for ao := 0; ; ao += 1000 {
				assets, err := s.assets.ListByRepo(r.ID, "", 1000, ao)
				if err != nil {
					return nil, err
				}
				for j := range assets {
					a := &assets[j]
					s.recordChange(EntityAsset, AssetKey(r.Name, a.Path), OpPut, AssetChangeData{
						Path: a.Path, BlobHash: a.BlobHash, Size: a.Size,
						ContentType: a.ContentType, Sha1: a.Sha1, Md5: a.Md5,
						CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
					})
					res.Updated++
				}
				if len(assets) < 1000 {
					break
				}
			}
		}
		if len(repos) < pageSize {
			break
		}
		offset += pageSize
	}
	res.Scanned = res.Updated
	return res, nil
}

// Get 从 hosted 仓库拉取一件制品（仅本地缓存读），返回元数据与内容可读流（调用方负责关闭）。
// 仓库或路径不存在均返回 ErrNotFound。proxy/group 的读路径请用 Resolve。
func (s *AssetService) Get(repoName, path string) (*repository.Asset, io.ReadCloser, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	return s.localGet(repo, path)
}

// Resolve 按仓库 Type 解析读请求：hosted 读本地；proxy 命中即返回、未命中回源缓存后返回；
// group 按成员有序解析、首个命中即返回。返回元数据与内容可读流（调用方负责关闭）。
// 全未命中返回 ErrNotFound；回源失败返回 ErrUpstream / ErrUpstreamTimeout。
// 目录形路径（空串或以 / 结尾）不是制品，直接 ErrNotFound：否则 proxy 会把上游
// 返回的 HTML 目录索引页当成制品缓存（文件树出现空白名的 text/html 假文件）。
func (s *AssetService) Resolve(ctx context.Context, repoName, path string) (*repository.Asset, io.ReadCloser, error) {
	if path == "" || strings.HasSuffix(path, "/") {
		// 早期返回不计入解析结果指标：目录形路径不是制品，补写 Confirmed 会连带改变
		// maven 的 notFound memo 行为（它读的正是同一个结论），属本 FR 边界之外。
		return nil, nil, ErrNotFound
	}
	return s.resolveObserved(ctx, repoName, path, func(ctx context.Context) (*repository.Asset, io.ReadCloser, error) {
		return s.resolvePath(ctx, repoName, path)
	})
}

// ResolveWithRepo 使用调用方已解析的仓库快照执行制品解析，避免协议层重复查库。
func (s *AssetService) ResolveWithRepo(ctx context.Context, repo *repository.Repository, path string) (*repository.Asset, io.ReadCloser, error) {
	if path == "" || strings.HasSuffix(path, "/") {
		return nil, nil, ErrNotFound
	}
	if repo == nil {
		return nil, nil, ErrNotFound
	}
	return s.resolveObserved(ctx, repo.Name, path, func(ctx context.Context) (*repository.Asset, io.ReadCloser, error) {
		return s.resolve(ctx, repo, path, 0)
	})
}

// ResolveIndex 解析并缓存 proxy 的目录索引。
// 普通制品路径仍由 Resolve 拒绝尾斜杠，避免把 Raw 目录页误当制品；
// PyPI Simple 等格式索引明确需要该语义，因此单独开放入口。
func (s *AssetService) ResolveIndex(ctx context.Context, repoName, path string) (*repository.Asset, io.ReadCloser, error) {
	if path == "" || !strings.HasSuffix(path, "/") {
		return nil, nil, ErrNotFound
	}
	return s.resolveObserved(ctx, repoName, path, func(ctx context.Context) (*repository.Asset, io.ReadCloser, error) {
		return s.resolvePath(ctx, repoName, path)
	})
}

func (s *AssetService) resolvePath(ctx context.Context, repoName, path string) (*repository.Asset, io.ReadCloser, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	return s.resolve(ctx, repo, path, 0)
}

// resolve 是 Resolve 的内部递归实现，depth 用于遏制 group 成员环引用。
// 入口先查 404 负缓存：此前「确认不存在」且 TTL 未过期的路径直接 404，
// 不再回源/探测（覆盖 Resolve/groupGet/proxyGet 各层入口，见）。
func (s *AssetService) resolve(ctx context.Context, repo *repository.Repository, path string, depth int) (*repository.Asset, io.ReadCloser, error) {
	if depth > maxResolveDepth {
		recordResolveFailure(ctx, ResolveFailureConfirmed)
		return nil, nil, ErrNotFound
	}
	if s.negCache.hit(repo.ID, path) {
		recordResolveFailure(ctx, ResolveFailureConfirmed)
		return nil, nil, ErrNotFound
	}
	switch repo.Type {
	case "proxy":
		return s.proxyGet(ctx, repo, path)
	case "group":
		return s.groupGet(ctx, repo, path, depth)
	default: // hosted 及未知类型均按本地读处理
		asset, rc, err := s.localGet(repo, path)
		if errors.Is(err, ErrNotFound) {
			recordResolveFailure(ctx, ResolveFailureConfirmed)
		}
		return asset, rc, err
	}
}

// localGet 读本地缓存：查 asset 元数据并打开对应 blob。路径不存在返回 ErrNotFound。
func (s *AssetService) localGet(repo *repository.Repository, path string) (*repository.Asset, io.ReadCloser, error) {
	var asset *repository.Asset
	var rc io.ReadCloser
	read := func() error {
		var err error
		asset, err = s.assets.GetByPath(repo.ID, path)
		if err != nil {
			return mapNotFound(err)
		}
		rc, _, err = s.blobs.Open(asset.BlobHash)
		return err
	}
	var err error
	if s.mutator != nil {
		err = s.mutator.Read(read)
	} else {
		err = read()
	}
	if err != nil {
		return nil, nil, err
	}
	return asset, rc, nil
}

// proxyGet 处理 proxy 仓库读：本地缓存命中即返回；未命中经 single-flight 收敛回源，
// 流式落 blob 并 upsert asset（缓存）后再从本地读返回。
// 上游处于 auto-block 阻止窗口时零连接快速失败（不发起上游连接，见 proxyHealth）。
func (s *AssetService) proxyGet(ctx context.Context, repo *repository.Repository, path string) (*repository.Asset, io.ReadCloser, error) {
	if asset, rc, err := s.localGet(repo, path); err == nil {
		recordCacheOutcome(ctx, CacheOutcomeHit)
		return asset, rc, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, nil, err
	}
	// 本地未命中即计 miss：本次请求无法由已缓存副本直接满足，需走回源（含回源失败）。
	recordCacheOutcome(ctx, CacheOutcomeMiss)
	// 备用节点只服务已同步到本地的缓存；未命中不得回源并写入本地。
	if s.requireBusinessWrite() != nil {
		recordResolveFailure(ctx, ResolveFailureUncertain)
		return nil, nil, ErrNotFound
	}
	if s.upstream == nil {
		recordResolveFailure(ctx, ResolveFailureUncertain)
		return nil, nil, ErrNotFound
	}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return nil, nil, err
	}
	if cfg.RemoteURL == "" {
		recordResolveFailure(ctx, ResolveFailureUncertain)
		return nil, nil, ErrNotFound
	}
	// 仓库手动 offline：不回源，快速 404（本地已确认未命中）。
	if !repo.Online {
		recordResolveFailure(ctx, ResolveFailureUncertain)
		return nil, nil, ErrNotFound
	}
	// 阻止窗口内直接快速失败，不发起上游连接（group 读不拖慢的关键）。
	if s.health.shouldBlock(repo.ID) {
		recordResolveFailure(ctx, ResolveFailureUncertain)
		return nil, nil, ErrUpstream
	}

	key := fmt.Sprintf("%d\x00%s", repo.ID, path)
	_, err, _ = s.sf.Do(key, func() (any, error) {
		// 并发回源收敛：进入临界区先复查缓存，避免重复下载。
		if _, e := s.assets.GetByPath(repo.ID, path); e == nil {
			return nil, nil
		}
		// 角色切换与并发请求交错时再次校验，避免已排队的回源落入备用节点。
		if s.requireBusinessWrite() != nil {
			return nil, ErrNotFound
		}
		after, ferr := s.fetchProxyAsset(ctx, repo, path)
		if ferr != nil {
			return nil, ferr
		}
		if s.mutator != nil {
			if err := s.mutator.Put(after); err != nil {
				return nil, s.cleanupFailedWrite(after.BlobHash, err)
			}
		} else if err := s.assets.Upsert(repo.ID, after.Path, after.BlobHash, after.Size, after.ContentType, after.Sha1, after.Md5); err != nil {
			return nil, s.cleanupFailedWrite(after.BlobHash, err)
		}
		s.releaseStagedBlob(after.BlobHash)
		cached, err := s.assets.GetByPath(repo.ID, path)
		if err != nil {
			return nil, err
		}
		s.negCache.remove(repo.ID, path)
		s.recordAssetPut(repo.Name, cached)
		return nil, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return s.localGet(repo, path)
}

// fetchProxyAsset 从上游流式读取一项代理缓存，但不建立可见资产引用。
// 调用方必须把返回资产与所属格式元数据作为同一事务发布，失败时负责清理 blob。
func (s *AssetService) fetchProxyAsset(ctx context.Context, repo *repository.Repository, path string) (*repository.Asset, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	if repo == nil || repo.Type != "proxy" || s.upstream == nil || !repo.Online {
		return nil, ErrNotFound
	}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return nil, err
	}
	if cfg.RemoteURL == "" {
		return nil, ErrNotFound
	}
	// 上游健康采样的计时起点：从「即将发起上游连接」算到「本次回源收尾」，
	// 因此包含传输与落盘耗时。它不包含闸门快速失败与本地缓存命中的时间——
	// 那些路径根本没有产生上游信号，不该进上游延迟统计。
	upstreamStarted := time.Now()
	// 半开闸门。shouldBlock 只做外层非占用式过滤；这里才是真正发起上游连接前的
	// **占用式**判定——判定与占位在同一把锁内完成，保证每轮半开只放行一个试探请求，
	// 其余请求在试探结果返回前等效封锁（快速失败）。
	if !s.health.beginUpstream(repo.ID) {
		return nil, ErrUpstream
	}
	body, header, err := s.upstream.FetchWithCredential(ctx, cfg.RemoteURL, path, cfg.CredentialRef)
	if err != nil {
		// 错误分类统一走 classifyUpstreamFailure（与 ResolveOCIProxy 共用同一实现），
		// 404 / 410 的豁免语义逐字保留，超时在此单列——慢不构成不可达证据。
		verdict := classifyUpstreamFailure(err)
		// 采样**只消费**这个判定结果，不再判一次（见 docs/specs/0.13.0-upstream-failure-classification.md）。唯一的补充是把「请求被取消」
		// 在采样这一层摘出来（见 upstreamHealthCategoryFor 的说明），不改变上面的判定本身。
		s.recordUpstreamHealthRequest(repo.ID, upstreamHealthCategoryFor(err, verdict.category), upstreamStarted)
		switch verdict.kind {
		case upstreamFailureNotFound:
			s.negCache.add(repo.ID, path)
			recordResolveFailure(ctx, ResolveFailureConfirmed)
			// 上游可达但资源缺失：不据此改状态，但必须释放试探名额，否则后续请求被无限封锁。
			s.health.releaseProbe(repo.ID)
		case upstreamFailureGone:
			// 410 表示当前资源永久下架，不代表上游仓库不可达；
			// 不能因此触发仓库级 auto-block，否则 Go proxy 等客户端的
			// 后续请求会被错误改写成 502，无法按标准链路回退。
			recordResolveFailure(ctx, ResolveFailureUncertain)
			s.health.releaseProbe(repo.ID)
		default:
			// 超时 → 只记 upstream_timeout 事件 + 释放名额（不改状态、不推进退避）；
			// 其余（连接 / DNS / TLS 失败、5xx 等）→ recordFailure 维持既有阻断。
			recordResolveFailure(ctx, ResolveFailureUncertain)
			s.settleUpstreamFailure(repo.ID, cfg.RemoteURL, cfg.CredentialRef, verdict)
		}
		return nil, mapUpstreamErr(err)
	}
	defer func() { _ = body.Close() }()
	// 拿到 2xx 响应头即「确认命中」：该成员确实持有该制品。先置位「上游已开始」信号，
	// group 读据此撤销成员的确认预算，此后 body 传输只受回源整体超时约束。
	// 未挂载信号时为无操作，代理直连 / 后台探测 / 格式元数据回退均不受影响。
	markUpstreamStarted(ctx)
	s.health.recordSuccess(repo.ID)
	// 传输阶段（已拿到 2xx 响应头）的失败与「拿头之前」完全不同：只可能是上游慢、
	// 上游中途断连，或本地磁盘写不进去——都不构成「上游不可达」的证据，因此一律不阻断。
	// 进度不能依赖 blobstore 的返回值（它在 io.Copy 失败时把已传字节数丢弃），
	// 因此在其外层包计数 reader；声明长度直接取已持有的响应头，不额外发请求（见 docs/specs/0.13.0-upstream-failure-classification.md）。
	declared := declaredContentLength(header)
	progress := &countingReader{r: body}
	transferStarted := time.Now()
	hash, sha1sum, md5sum, size, err := s.putStagedBlob(progress)
	if err != nil {
		// 落盘阶段的失败发生在「确认命中」之后：上游确实持有该制品，失败只可能来自
		// 传输中断、超时或本地磁盘。必须按类别分流，不能折叠成 ErrNotFound（假的永久 404），
		// 也不能让裸错误落到协议层 default 分支被映射成 500（本地写失败除外，它本就该是 500）。
		// 同时记 Uncertain：该成员不得被判成「确认不存在」，因而不写负缓存、下次仍会重试。
		verdict := classifyTransferFailure(err, progress.n, declared)
		// 同一份判定结果进入采样：slow_rate → slow 列、transfer_aborted → aborted 列、
		// no_response → timeout 列；local_write_failure / canceled 由采样服务丢弃。
		s.recordUpstreamHealthRequest(repo.ID, verdict.category, upstreamStarted)
		if verdict.kind != upstreamFailureCanceled {
			recordResolveFailure(ctx, ResolveFailureUncertain)
			s.logTransferFailure(repo.Name, path, verdict, progress.n, declared, time.Since(transferStarted))
		}
		switch verdict.kind {
		case upstreamFailureCanceled:
			// 客户端已断开 / 同组其它成员已成功：这是请求生命周期的终点，不是故障，
			// 原样上抛取消，既不记上游判定事件也不改写成因（与协议层对取消的处理一致）。
			return nil, err
		case upstreamFailureLocalWrite:
			// **本地磁盘写满 / 权限失败不是上游故障**：对外 500、且不记任何上游判定事件，
			// 否则会把运维引向「查上游」的错误方向（见 docs/specs/0.13.0-upstream-failure-classification.md）。
			return nil, fmt.Errorf("%w: %v", ErrLocalStorage, err)
		default:
			// no_response / slow_rate / transfer_aborted：三类都只记事件 + 释放半开名额，
			// 不推进退避、不改状态；对外统一 504（它们都发生在「已确认命中」之后）。
			s.settleUpstreamFailure(repo.ID, cfg.RemoteURL, cfg.CredentialRef, verdict)
			return nil, fmt.Errorf("%w: %v", ErrUpstreamTimeout, err)
		}
	}
	contentType := header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	// 成功采样：空类别即「上游可达且完整取回」，落 ok 列并计入延迟统计。
	s.recordUpstreamHealthRequest(repo.ID, "", upstreamStarted)
	return &repository.Asset{RepositoryID: repo.ID, Path: path, BlobHash: hash, Size: size, ContentType: contentType, Sha1: sha1sum, Md5: md5sum}, nil
}

// recordUpstreamHealthRequest 是回源路径记录「真实请求」采样的唯一入口。
// 未接线（nil）时不采样；采样只做进程内合并，不碰磁盘，因此放在回源收尾处不阻塞主路径。
func (s *AssetService) recordUpstreamHealthRequest(repoID int64, category string, started time.Time) {
	if s.upstreamHealth == nil {
		return
	}
	s.upstreamHealth.RecordRequest(repoID, category, time.Since(started))
}

// upstreamHealthCategoryFor 是采样点在  分类结果之上的**唯一**补充判定。
//
// 为什么需要它：classifyUpstreamFailure 面向断路器，把「客户端断开 / 优雅停机」这类
// context.Canceled 一路落到 unreachable——对断路器这是可接受的口径（取消不该放行），
// 但作为**上游健康信号**它必须被丢弃，否则每次用户中断大文件下载都会往「不可达」上记一笔，
// 把连通率压低到与上游无关的水平（见 docs/specs/0.13.0-upstream-failure-classification.md）。
//
// 只在采样这一层做，**不改** classifyUpstreamFailure 的判定与词表（断路器行为不受影响）。
func upstreamHealthCategoryFor(err error, category string) string {
	if errors.Is(err, context.Canceled) {
		return upstreamCategoryCanceled
	}
	return category
}

func (s *AssetService) cleanupFailedWrite(hash string, cause error) error {
	s.releaseStagedBlob(hash)
	if s.mutator == nil {
		if err := s.blobs.Remove(hash); err != nil {
			return errors.Join(cause, fmt.Errorf("清理未引用 blob 失败：%w", err))
		}
		return cause
	}
	if err := s.mutator.RemoveIfUnreferenced(hash); err != nil {
		return errors.Join(cause, fmt.Errorf("清理未引用 blob 失败：%w", err))
	}
	return cause
}

// ResolveOCIProxy 按 OCI Bearer 质询读取 proxy 资源。只有通过期望 sha256 摘要
// 校验的上游正文才会写入本地缓存，防止错误 manifest/blob 污染后续拉取。
func (s *AssetService) ResolveOCIProxy(ctx context.Context, repoName, path, pullScope, expectedDigest string) (*repository.Asset, io.ReadCloser, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	if repo.Type != "proxy" {
		return s.Resolve(ctx, repoName, path)
	}
	if asset, rc, err := s.ociLocalGet(repo, path, expectedDigest); err == nil {
		recordCacheOutcome(ctx, CacheOutcomeHit)
		return asset, rc, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, nil, err
	}
	// 本地未命中即计 miss：本次请求无法由已缓存副本直接满足，需按 Bearer 质询回源。
	recordCacheOutcome(ctx, CacheOutcomeMiss)
	if s.requireBusinessWrite() != nil || s.upstream == nil || !repo.Online || s.health.shouldBlock(repo.ID) {
		return nil, nil, ErrNotFound
	}
	cfg, err := repo.DecodeConfig()
	if err != nil || cfg.RemoteURL == "" {
		return nil, nil, ErrNotFound
	}
	key := fmt.Sprintf("oci:%d\x00%s", repo.ID, path)
	_, err, _ = s.sf.Do(key, func() (any, error) {
		if cached, cacheErr := s.assets.GetByPath(repo.ID, path); cacheErr == nil {
			if expectedDigest == "" || cached.BlobHash == strings.TrimPrefix(expectedDigest, "sha256:") {
				return nil, nil
			}
		}
		if s.requireBusinessWrite() != nil {
			return nil, ErrNotFound
		}
		// 半开闸门（占用式）。上面兜底过滤只保证「不落在阻止窗口内」，
		// 这里在真正发起上游连接前抢占名额，保证每轮半开只放行一个试探请求。
		// 与普通 proxy 路径的区别是收敛点在 singleflight 内：不同 path 是不同 key，
		// 因此闸门必须装在这里而非外层。
		// 上游健康采样计时起点（仅覆盖真正发起上游连接之后的耗时）。
		upstreamStarted := time.Now()
		if !s.health.beginUpstream(repo.ID) {
			return nil, ErrUpstream
		}
		session, sessionErr := s.upstream.NewSession(cfg.RemoteURL, cfg.CredentialRef)
		if sessionErr != nil {
			// 构造会话失败（地址不合法等）视同不可达：采样与断路器判定同源，不另判一次。
			s.recordUpstreamHealthRequest(repo.ID, classifyUpstreamFailure(sessionErr).category, upstreamStarted)
			s.health.recordFailure(repo.ID, cfg.RemoteURL, cfg.CredentialRef, UpstreamBlockReasonUpstreamError)
			return nil, mapUpstreamErr(sessionErr)
		}
		body, header, fetchErr := session.ReadOCIPath(ctx, path, pullScope)
		if fetchErr != nil {
			// 与 fetchProxyAsset 共用同一分类判定（classifyUpstreamFailure）：
			// 超时只记事件 + 释放名额，不阻断；其余非 404 维持 recordFailure。
			verdict := classifyUpstreamFailure(fetchErr)
			s.recordUpstreamHealthRequest(repo.ID, verdict.category, upstreamStarted)
			switch verdict.kind {
			case upstreamFailureNotFound:
				// 上游可达但资源缺失：不改状态，但必须释放试探名额。
				s.health.releaseProbe(repo.ID)
			case upstreamFailureGone:
				// OCI 路径只豁免 404、不豁免 410：与 raw 路径的这一差异属既有行为，
				// 不在本 FR 范围内统一（见 0.13.0-upstream-slow-not-unreachable §6.2）。
				// 这里只把**超时**从阻断分支摘出，410 仍按原口径计入不可达。
				s.health.recordFailure(repo.ID, cfg.RemoteURL, cfg.CredentialRef, UpstreamBlockReasonUpstreamError)
			default:
				s.settleUpstreamFailure(repo.ID, cfg.RemoteURL, cfg.CredentialRef, verdict)
			}
			return nil, mapUpstreamErr(fetchErr)
		}
		defer func() { _ = body.Close() }()
		s.health.recordSuccess(repo.ID)
		// 传输阶段的判定与 fetchProxyAsset 完全同构，因此复用同一函数：
		// ReadOCIPath 返回 nil 错误即代表**已拿到 2xx 响应头**（readResponse 只在 2xx 时
		// 返回 body，非 2xx / 404 / 410 一律走错误分支），此刻之后的失败只可能发生在正文
		// 传输阶段，与「是否已拿到响应头」这一判据的时点一一对齐（见 docs/specs/0.13.0-upstream-failure-classification.md）。
		// 注：OCI 的 manifest 与 blob 是**两次独立调用**，各自有各自的拿头时点，
		// 因此不存在「两段混在一起、拿头时点不可判定」的问题。
		declared := declaredContentLength(header)
		progress := &countingReader{r: body}
		transferStarted := time.Now()
		hash, sha1sum, md5sum, size, putErr := s.putStagedBlob(progress)
		if putErr != nil {
			verdict := classifyTransferFailure(putErr, progress.n, declared)
			s.recordUpstreamHealthRequest(repo.ID, verdict.category, upstreamStarted)
			if verdict.kind != upstreamFailureCanceled {
				recordResolveFailure(ctx, ResolveFailureUncertain)
				s.logTransferFailure(repo.Name, path, verdict, progress.n, declared, time.Since(transferStarted))
			}
			switch verdict.kind {
			case upstreamFailureCanceled:
				return nil, putErr
			case upstreamFailureLocalWrite:
				return nil, fmt.Errorf("%w: %v", ErrLocalStorage, putErr)
			default:
				s.settleUpstreamFailure(repo.ID, cfg.RemoteURL, cfg.CredentialRef, verdict)
				return nil, fmt.Errorf("%w: %v", ErrUpstreamTimeout, putErr)
			}
		}
		if !ociDigestMatches(hash, expectedDigest, header.Get("Docker-Content-Digest")) {
			s.removeUnreferencedBlob(hash)
			return nil, ErrUpstream
		}
		contentType := header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		after := &repository.Asset{RepositoryID: repo.ID, Path: path, BlobHash: hash, Size: size, ContentType: contentType, Sha1: sha1sum, Md5: md5sum}
		if s.mutator != nil {
			if putErr := s.mutator.Put(after); putErr != nil {
				s.removeUnreferencedBlob(hash)
				return nil, putErr
			}
		} else if putErr := s.assets.Upsert(repo.ID, path, hash, size, contentType, sha1sum, md5sum); putErr != nil {
			s.removeUnreferencedBlob(hash)
			return nil, putErr
		}
		s.releaseStagedBlob(hash)
		// 成功采样必须在「摘要校验通过 + 资产已登记」之后：摘要不匹配的响应不是一次
		// 可用的成功回源，把它记成 ok 会让连通率偏高。
		s.recordUpstreamHealthRequest(repo.ID, "", upstreamStarted)
		cached, getErr := s.assets.GetByPath(repo.ID, path)
		if getErr != nil {
			return nil, getErr
		}
		s.negCache.remove(repo.ID, path)
		s.recordAssetPut(repo.Name, cached)
		return nil, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return s.ociLocalGet(repo, path, expectedDigest)
}

func (s *AssetService) ociLocalGet(repo *repository.Repository, path, expectedDigest string) (*repository.Asset, io.ReadCloser, error) {
	asset, rc, err := s.localGet(repo, path)
	if err != nil || expectedDigest == "" {
		return asset, rc, err
	}
	if strings.TrimPrefix(expectedDigest, "sha256:") == asset.BlobHash {
		return asset, rc, nil
	}
	_ = rc.Close()
	return nil, nil, ErrNotFound
}

func (s *AssetService) removeUnreferencedBlob(hash string) {
	s.releaseStagedBlob(hash)
	if s.mutator != nil {
		_ = s.mutator.RemoveIfUnreferenced(hash)
		return
	}
	_ = s.blobs.Remove(hash)
}

func ociDigestMatches(hash, expectedDigest, contentDigest string) bool {
	expected := strings.TrimPrefix(expectedDigest, "sha256:")
	if expected != "" && hash != expected {
		return false
	}
	if contentDigest == "" {
		return true
	}
	if !strings.HasPrefix(contentDigest, "sha256:") || len(contentDigest) != len("sha256:")+64 {
		return false
	}
	return hash == strings.TrimPrefix(contentDigest, "sha256:")
}

// groupMemberTimeoutDefault 是 group 仓库逐成员解析时**确认阶段**预算的兜底默认值：
// 只约束「该成员是否及时给出上游响应头」这一件事，不约束响应体传输（落盘预算由回源整体
// 超时 JIAN_UPSTREAM_TIMEOUT 承担，见 groupGet 的首字节看门狗）。
// 防止单个不可达 proxy 成员阻塞后续成员的探测；配合并行探测与 auto-block 阻止，
// 值取小（3s）让不可达上游快速失败、整体响应控制在秒级。
//
// 生产装配经 SetGroupMemberTimeout 注入 JIAN_GROUP_MEMBER_TIMEOUT 的解析结果，本常量只在
// 未注入时兜底（测试构造与未接线的路径），因此必须与 config 的 defaultGroupMemberTimeout
// 保持同值——改一处必须同步改另一处。
// 注意：看门狗的计时器只读 AssetService.memberConfirmBudget()，不得另硬编码一份时长。
const groupMemberTimeoutDefault = 3 * time.Second

// groupGet 处理 group 仓库读，目标是"本地命中优先、缺失快速失败"：
//  1. 先串行快查全部成员的本地缓存（hosted / 已缓存 proxy 命中即返回，不触发网络）——
//     保持成员顺序语义（先成员命中优先）。
//  2. 本地全未命中 → 对未被 auto-block 阻止的 proxy 成员并行回源（取最快成功；首个成功后取消其余）。
//     不可达成员经 proxyHealth 阻止窗口跳过（零连接）。
//
// 成员预算的两阶段模型：
//   - 确认阶段：成员确认预算（JIAN_GROUP_MEMBER_TIMEOUT，默认 3s）只约束「该成员是否及时
//     给出上游 2xx 响应头，或给出结论」；
//   - 落盘阶段：一旦确认命中，该成员的预算立即撤销，剩余传输只受回源整体超时
//     （JIAN_UPSTREAM_TIMEOUT，默认 30s）约束——即首字节看门狗，见下方并行回源的实现。
//
// 为什么必须解耦：把两者塞进同一个 3s 会让「大制品」必挂——15MB 制品需约 5MB/s 才能
// 在 3s 内下完，而 3s 到期会让 resp.Body.Read 失败，整个制品随后被判成不可判定甚至缺失。
//
// 全未命中时的返回码按成员失败类别分流（不可判定绝不折叠成 404）：
//   - 所有成员都确认不存在 → ErrNotFound（对外 404）；
//   - 存在不可判定成员且其中有超时 → ErrUpstreamTimeout（对外 504）；
//   - 存在不可判定成员是本地存储写入失败（且无超时）→ ErrLocalStorage（对外 500）：
//     这是服务端自身故障，报成 502 会让人去查上游；
//   - 其余不可判定成员不是超时（上游 5xx、熔断阻止、离线、成员查找失败）→ ErrUpstream（对外 502）。
//
// 为什么必须分流：上游只是「慢」或「暂时不可用」并不证明制品不存在。折叠成 404 的后果是
// 客户端认为该制品永久缺失（Gradle/Maven 直接回退下一仓库或构建失败），而这次超时又不写
// 负缓存，于是每次重试都重新超时——表现为一个永不消失、也无法自愈的假 404。
//
// 全未命中是否写负缓存（M-1）：
// 只有「确认不存在」才写——即本次探测中没有任何成员被跳过（offline/auto-block 阻止）
// 或回源失败（上游 5xx/超时）。任一成员不可判定（可能只是暂时不可用），则不写负缓存，
// 避免把上游故障误缓存成永久不存在（上游恢复后同路径立即重试成功）。
func (s *AssetService) groupGet(ctx context.Context, repo *repository.Repository, path string, depth int) (*repository.Asset, io.ReadCloser, error) {
	// group 的缓存来源不可靠：首个命中成员可能来自任一成员，冻结为未知，避免成员级 hit/miss 外泄。
	sealCacheOutcome(ctx)
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return nil, nil, err
	}
	// 环路保护：深度超限是确定的结构性结论（不是「上游可能只是慢」），因此仍按确认不存在处理。
	if depth > maxResolveDepth {
		recordResolveFailure(ctx, ResolveFailureConfirmed)
		return nil, nil, ErrNotFound
	}

	// 成员快照只解析一次，并始终按配置原始顺序保存，供两个阶段共同复用。
	members := make([]*repository.Repository, len(cfg.Members))
	// 成员级失败按两类累计，把「不可判定」细分成可区分的返回码：
	//   timedOut —— 至少一个成员以超时告终（成员预算耗尽或回源超时）→ 504；
	//   failed   —— 至少一个成员因其它原因不可判定（上游 5xx、熔断阻止、离线、成员查找失败）→ 502。
	// 两者的并集就是「本轮存在不可判定成员」，它同时是「不写 404 负缓存」的判据。
	// 已确认不存在的成员不进入任一累计——那才是真正的 404 证据。
	var timedOut, failed atomic.Bool
	// localFailed 单独累计「本地存储写入失败」：它既不能证明制品不存在，也不是上游不可达，
	// 与 failed 混在一起会让整体对外变成 502，把运维引向查上游。
	var localFailed atomic.Bool
	// undecided 报告本轮是否存在不可判定成员。
	undecided := func() bool { return timedOut.Load() || failed.Load() }
	for i, name := range cfg.Members {
		member, getErr := s.repos.GetByName(name)
		if getErr != nil {
			failed.Store(true)
			recordResolveFailureMember(ctx, name, resolveFailureKindMemberUnknown)
			continue
		}
		members[i] = member
	}

	// 阶段一：串行快查全部成员的本地缓存，保持成员优先级。
	for _, member := range members {
		if member == nil {
			continue
		}
		if !member.Online {
			failed.Store(true)
			continue
		}
		if asset, rc, localErr := s.localGet(member, path); localErr == nil {
			return asset, rc, nil
		}
	}
	if s.requireBusinessWrite() != nil {
		recordResolveFailure(ctx, ResolveFailureUncertain)
		return nil, nil, ErrNotFound
	}

	type job struct {
		idx    int
		member *repository.Repository
	}
	jobs := make([]job, 0, len(members))
	for i, member := range members {
		if member == nil {
			continue
		}
		if !member.Online {
			failed.Store(true)
			recordResolveFailureMember(ctx, member.Name, resolveFailureKindOffline)
			continue
		}
		if member.Type != "proxy" {
			recordResolveFailureMember(ctx, member.Name, resolveFailureKindNotProxy)
			continue
		}
		if s.health.shouldBlock(member.ID) {
			failed.Store(true)
			recordResolveFailureMember(ctx, member.Name, resolveFailureKindBlocked)
			continue
		}
		jobs = append(jobs, job{idx: i, member: member})
	}
	if len(jobs) == 0 {
		// 没有任何成员可回源：本分支不会发生超时，不可判定一律归到上游故障侧。
		if undecided() {
			recordResolveFailure(ctx, ResolveFailureUncertain)
			return nil, nil, ErrUpstream
		}
		s.negCache.add(repo.ID, path)
		recordResolveFailure(ctx, ResolveFailureConfirmed)
		return nil, nil, ErrNotFound
	}

	results := make([]bool, len(members))
	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// 确认预算只读一次：同一请求内所有成员用同一个上界，避免热改让同一轮探测里不同成员
	// 拿到不同预算（那会让「本轮到底给了多少预算」在排查时无法复现）。
	// 这里是看门狗时长的**唯一**取值点，setter 注入的值经 memberConfirmBudget 到达。
	confirmBudget := s.memberConfirmBudget()
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			// 成员 ctx 只带取消、**不带 deadline**：groupMemberTimeout 是「确认预算」而非
			// 「成员总预算」。不能改用更长的 WithTimeout——子 ctx 的 deadline 只能收紧、
			// 无法在收到响应头后撤回，那正是本次要修的故障。
			memberCtx, mCancel := context.WithCancel(groupCtx)
			defer mCancel()
			memberCtx, started := WithUpstreamStarted(memberCtx)
			memberCtx = WithResolveFailure(memberCtx)

			// resolve 放进内层 goroutine，外层只做「等结论 / 等首字节」的竞速。
			// resolveErr / memberRC 由内层写入，必须等 done 关闭后才能读取（否则 -race 报竞态）。
			done := make(chan struct{})
			var resolveErr error
			var memberRC io.ReadCloser
			go func() {
				defer close(done)
				_, memberRC, resolveErr = s.resolve(memberCtx, j.member, path, depth+1)
			}()

			// 同时就绪时偏向宽容侧：先做一次非阻塞的确认检查，再进 select。
			// 误取消的代价是把本可下载完的制品判成缺失（正是本 FR 要修的故障），
			// 多等的代价只是一次回源整体超时，两者不可对称。
			alreadyStarted := false
			select {
			case <-started:
				alreadyStarted = true
			default:
			}
			watchdogFired := false
			if !alreadyStarted {
				// 计时器独立于 ctx：只有它能在不设 deadline 的前提下提供确认阶段上界。
				timer := time.NewTimer(confirmBudget)
				defer timer.Stop()
				select {
				case <-started:
					// 已确认该成员持有该制品 → 撤销确认预算，余下交给回源整体超时。
				case <-done:
					// 成员已给出结论（含快速 404）。
				case <-timer.C:
					// 计时器到点的瞬间成员可能刚给出结论、或刚收到响应头，仍按宽容侧复查一次，
					// 避免在边界上误取消一个其实可用的成员。
					select {
					case <-started:
					case <-done:
					default:
						// 确认预算内既无响应头也无结论 → 判为不可判定；取消该成员让
						// resp.Body.Read 立即失败，释放上游连接与内层 goroutine。
						mCancel()
						watchdogFired = true
					}
				}
			}
			// 任何收尾路径都必须等内层真正退出：既避免对 resolveErr 的数据竞态，也避免 goroutine 泄漏。
			<-done

			if watchdogFired {
				// 看门狗已判过「不可判定且是超时」，不再参与下面的分类：
				// 否则被取消产生的错误还会被记成 failed，让整体落到 502 而不是 504。
				timedOut.Store(true)
				recordResolveFailureMember(ctx, j.member.Name, resolveFailureKindTimeout)
				return
			}
			if resolveErr != nil {
				// 失败类别决定整体返回码：不能一律折叠成「制品不存在」。
				switch {
				case errors.Is(resolveErr, ErrNotFound) && ResolveFailureFromContext(memberCtx) == ResolveFailureConfirmed:
					// 该成员确实确认不存在：真实的 404 证据，不做不可判定记账。
				case memberCtx.Err() == context.DeadlineExceeded || errors.Is(resolveErr, ErrUpstreamTimeout):
					// 请求级 ctx 到期或回源超时：上游可能只是慢（大制品下完要更久）→ 504。
					timedOut.Store(true)
					recordResolveFailureMember(ctx, j.member.Name, resolveFailureKindTimeout)
				case errors.Is(resolveErr, ErrLocalStorage):
					// 本地写入失败：服务端自身故障，不是上游不可达也不是制品不存在，
					// 因此单独累计并按内部错误上抛（对外 500），不推进任何上游阻断。
					localFailed.Store(true)
					recordResolveFailureMember(ctx, j.member.Name, classifyResolveFailureKind(resolveErr))
				default:
					// 上游 5xx、熔断/离线跳过、成员查找失败，以及被同伴成功取消的被动终止：
					// 都无法证明「制品不存在」→ 502（宁可让客户端重试，也不给一个假的永久 404）。
					failed.Store(true)
					recordResolveFailureMember(ctx, j.member.Name, classifyResolveFailureKind(resolveErr))
				}
				return
			}
			// 该成员已把内容落盘并可本地读到：关闭本次读流，稍后按下标统一重新 localGet。
			_ = memberRC.Close()
			results[j.idx] = true
			cancel()
		}(j)
	}
	wg.Wait()

	for i, ok := range results {
		if ok {
			asset, rc, localErr := s.localGet(members[i], path)
			if localErr == nil {
				return asset, rc, nil
			}
			// 已确认某个成员持有内容却读不回本地副本（并发删除 / 清理竞态）：这是本地读失败，
			// 更不是「制品不存在」，同样不得折叠成 404。
			failed.Store(true)
		}
	}
	// 全未命中：按失败类别分流返回码。只要存在不可判定成员，就绝不返回 ErrNotFound。
	if timedOut.Load() {
		recordResolveFailure(ctx, ResolveFailureUncertain)
		return nil, nil, ErrUpstreamTimeout
	}
	if localFailed.Load() {
		// 没有任何成员超时，但有成员写本地失败：按内部错误上抛（对外 500），
		// 不要伪装成 502「上游不可达」。
		recordResolveFailure(ctx, ResolveFailureUncertain)
		return nil, nil, ErrLocalStorage
	}
	if failed.Load() {
		recordResolveFailure(ctx, ResolveFailureUncertain)
		return nil, nil, ErrUpstream
	}
	s.negCache.add(repo.ID, path)
	recordResolveFailure(ctx, ResolveFailureConfirmed)
	return nil, nil, ErrNotFound
}

// ListAssetsByPrefix 返回 hosted 仓库内指定前缀的资产元数据，供协议构造完整原子快照。
func (s *AssetService) ListAssetsByPrefix(repoName, prefix string, limit int) ([]repository.Asset, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return nil, ErrConflict
	}
	return s.assets.ListByRepo(repo.ID, prefix, limit, 0)
}

// ListPathsByPrefix 列出仓库内以 prefix 开头的全部资产路径（npm unpublish 整包删除用）。
func (s *AssetService) ListPathsByPrefix(repoName, prefix string) ([]string, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	assets, err := s.assets.ListByRepo(repo.ID, prefix, 10000, 0)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(assets))
	for _, a := range assets {
		paths = append(paths, a.Path)
	}
	return paths, nil
}

// Delete 删除制品元数据：引用归零的 blob 在本次调用返回前完成物理回收（ADR-0024），
// 仍被其它资产引用的 blob 留在活动目录（由 blob-gc 兜底清理孤立件）。
// 仓库或路径不存在均返回 ErrNotFound。
func (s *AssetService) Delete(repoName, path string) error {
	if err := s.requireBusinessWrite(); err != nil {
		return err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return mapNotFound(err)
	}
	if s.mutator != nil {
		if err := s.mutator.Delete(repo.ID, path); err != nil {
			return mapNotFound(err)
		}
	} else if err := s.assets.DeleteByPath(repo.ID, path); err != nil {
		return mapNotFound(err)
	}
	// 写路径成功：失效同路径负缓存，避免删除后重传的制品被缓存 404 挡住。
	s.negCache.remove(repo.ID, path)
	s.recordChange(EntityAsset, AssetKey(repoName, path), OpDelete, TombstoneData{Deleted: true})
	return nil
}

// upstreamFailureKind 是回源失败在断路器语境下的语义类别。
// 判定顺序与 mapUpstreamErr **必须一致**，避免两处对同一错误给出不同结论。
type upstreamFailureKind int

const (
	// upstreamFailureUnreachable 连接失败 / DNS 解析失败 / TLS 握手失败 / 上游 5xx、401、403：
	// 计入「上游不可达」，触发（或延长）仓库级阻断。
	upstreamFailureUnreachable upstreamFailureKind = iota
	// upstreamFailureTimeout 预算内没拿到答案（调用方预算耗尽或回源客户端自身超时）：
	// **没有证据表明上游不可达**，因此不参与仓库级阻断，只记判定事件。
	upstreamFailureTimeout
	// upstreamFailureGone 上游明确 410：资源永久下架，但上游本身可达，不阻断。
	upstreamFailureGone
	// upstreamFailureNotFound 上游明确 404：资源缺失，上游本身可达，不阻断。
	upstreamFailureNotFound
	// upstreamFailureTransferAborted 已拿到响应头、传输中途被中断（连接重置 / 提前 EOF）。
	// 与超时一样**没有证据表明上游不可达**，因此不阻断，对外按 504 语义返回。
	upstreamFailureTransferAborted
	// upstreamFailureLocalWrite 本地存储写入失败（磁盘满 / 权限 / 配额）。
	// **不是上游故障**：不记任何上游判定事件、不阻断，由调用方映射为内部错误（对外 500）。
	// 单列这一类就是为了不让它被误报成「上游慢」或「上游失败」，把运维引向错误方向。
	upstreamFailureLocalWrite
	// upstreamFailureCanceled 请求被取消（客户端断连、同组其它成员已成功、优雅停机）。
	// 正常的请求生命周期终点：不记事件、不阻断、不记日志，原样把取消上抛。
	upstreamFailureCanceled
)

// upstreamFailureVerdict 是一次回源失败对断路器的判定结果。
// 只做**分类**，不做副作用：记事件、放名额、写负缓存一律留在调用点，
// 便于两个回源调用点（fetchProxyAsset / ResolveOCIProxy）各自决定收口细节。
type upstreamFailureVerdict struct {
	kind   upstreamFailureKind // 失败的语义类别
	reason UpstreamBlockReason // 需要记录的判定事件类别；空串表示上游可达、不记录（404 / 410）
	block  bool                // true 表示应 recordFailure 进入（或延长）仓库级阻断
	// category 是可观测类别（见 docs/specs/0.13.0-upstream-failure-classification.md），只用于结构化日志，
	// 不参与阻断判定。与 kind 并存是因为两者面向不同读者：kind 回答「要不要阻断」，
	// category 回答「该调超时、该重试、该查上游、还是该查本地磁盘」。
	category string
}

// classifyUpstreamFailure 把回源失败分类为断路器判定结果（本函数是唯一实现，
// 由 fetchProxyAsset 与 ResolveOCIProxy 共用，防止两条回源链路给出不同结论）。
//
// 分类顺序与 mapUpstreamErr 保持一致：404 → 410 → 超时 → 其余（不可达）。
// 超时判定复用 upstream.IsTimeout，不自造第二套超时嗅探；该函数同时覆盖两条来源：
// 调用方预算耗尽（context.DeadlineExceeded，含 group 成员确认阶段的预算）与
// 回源客户端自身超时（*url.Error 实现了 Timeout()）。
// 语义要点：「慢」不等于「连不上」。超时只作为事件记录（upstream_timeout），
// 不推进退避、不改状态，避免单个慢/大制品让整个 proxy 仓库进入 AUTO_BLOCKED。
//
// 本函数只负责「**还没拿到响应头**」的失败；「拿到响应头之后」的传输失败由
// classifyTransferFailure 判定。两者共用同一个
// upstreamFailureVerdict 与 settleUpstreamFailure，因此结论口径一致。
func classifyUpstreamFailure(err error) upstreamFailureVerdict {
	// 取消必须最先摘出来，而且要看 context.Canceled 之外还可能是 DeadlineExceeded 的
	// 「我方取消」形态——但这里只认 Canceled：
	//   - group 里快成员成功后 cancel() 其余成员；
	//   - 调用方（客户端）主动断开；
	//   - 进程停机。
	// 这些都不是「上游不可达」的证据。漏掉这一条会在 group 里造成误阻断：
	// 慢成员还没拿到响应头就被同伴取消 → 落到 default → block=true →
	// recordFailure → 整个 proxy 仓库被判 AUTO_BLOCKED，40s 起逐档退避。
	// 那正好复现了本批要修的「整仓不可用」，等于用新故障换掉旧故障。
	if errors.Is(err, context.Canceled) {
		return upstreamFailureVerdict{kind: upstreamFailureCanceled, category: upstreamCategoryCanceled}
	}
	switch {
	case errors.Is(err, upstream.ErrNotFound):
		return upstreamFailureVerdict{kind: upstreamFailureNotFound, category: upstreamCategoryConfirmedMissing}
	case errors.Is(err, upstream.ErrGone):
		return upstreamFailureVerdict{kind: upstreamFailureGone, category: upstreamCategoryConfirmedMissing}
	case upstream.IsTimeout(err):
		return upstreamFailureVerdict{kind: upstreamFailureTimeout, reason: UpstreamBlockReasonTimeout, category: upstreamCategoryNoResponse}
	default:
		return upstreamFailureVerdict{kind: upstreamFailureUnreachable, reason: UpstreamBlockReasonUpstreamError, block: true, category: upstreamCategoryUnreachable}
	}
}

// settleUpstreamFailure 按共享判定收口一次「非 404/410」的回源失败：
//   - 不可达：recordFailure 进入或延长仓库级阻断（既有退避规则不变）；
//   - 超时：只记 upstream_timeout 事件并释放半开名额，**不调 recordFailure**、
//     不推进退避、不改状态——否则单个慢/大制品会让同仓库其它路径一起零连接快速失败。
//
// 超时也必须释放名额：beginUpstream 已占住本轮 HALF_OPEN 的探测名额，占着不放会让
// 后续请求在 probeInFlight 处被无限快速失败，同样复现「整仓不可用」。
// 404 / 410 的豁免与附加动作（负缓存、Confirmed 记录）留在各自调用点，逐字保留。
func (s *AssetService) settleUpstreamFailure(repoID int64, remoteURL, credentialRef string, verdict upstreamFailureVerdict) {
	if verdict.block {
		s.health.recordFailure(repoID, remoteURL, credentialRef, verdict.reason)
		return
	}
	s.health.recordBlockEvent(verdict.reason)
	s.health.releaseProbe(repoID)
}

// mapUpstreamErr 把 upstream 层错误映射为领域错误：404 视为未命中（ErrNotFound）、
// 410 同时保留通用回源失败和明确下架语义、超时映射 ErrUpstreamTimeout，其余映射 ErrUpstream。
// 分类顺序与 classifyUpstreamFailure 保持一致。
func mapUpstreamErr(err error) error {
	switch {
	case errors.Is(err, upstream.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, upstream.ErrGone):
		return fmt.Errorf("%w: %w", ErrUpstream, ErrUpstreamGone)
	case upstream.IsTimeout(err):
		return fmt.Errorf("%w: %v", ErrUpstreamTimeout, err)
	default:
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
}

// probeUpstream 后台探测上游可达性：与回源使用同一个安全客户端和凭据解析路径。
func (s *AssetService) probeUpstream(ctx context.Context, remoteURL, credentialRef string) error {
	if s.upstream == nil {
		return ErrUpstream
	}
	status, err := s.upstream.ProbeWithCredential(ctx, remoteURL, credentialRef)
	if err != nil {
		return err
	}
	if shouldBlockStatus(status) {
		return fmt.Errorf("上游返回状态码 %d", status)
	}
	return nil
}

// shouldBlockStatus 判断上游返回状态码是否应视为失败（5xx / 401 / 403）。
// 404 等其余 4xx 视为上游可达（仅资源缺失），不触发阻止。
func shouldBlockStatus(code int) bool {
	return code >= 500 || code == 401 || code == 403
}
