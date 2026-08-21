package domain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// maxResolveDepth 限制 group 成员递归解析深度，防止成员相互引用形成环导致的无限递归。
const maxResolveDepth = 16

// AssetService 编排制品的发布与拉取：元数据落 asset 表，内容落内容寻址 blob。
//
// 分层（见 internal/doc.go）：domain -> repository, blobstore, upstream。协议层
// （internal/protocol）经此服务读写制品，不直接触碰 SQLite、文件系统或上游 HTTP。
//
// 读路径按仓库 Type 分派（见 Resolve）：hosted 读本地缓存；proxy 命中即返回，
// 未命中经 upstream 回源并缓存（single-flight 收敛并发回源）；group 先本地快查、
// 未命中则并行回源（见 groupGet），并对不可达上游做 auto-block 短窗阻止（见 proxyHealth）。
type AssetService struct {
	repos    *repository.RepoRepo
	assets   *repository.AssetRepo
	blobs    *blobstore.Store
	upstream *upstream.Client
	sf       singleflight.Group
	recorder ChangeRecorder
	health   *proxyHealth
	negCache *negativeCache // FR-111：404 负缓存（proxy/group 读路径短窗缓存明确 404）
}

// NewAssetService 构造 AssetService。upstream 供 proxy 回源使用（hosted-only 部署可传 nil）。
func NewAssetService(repos *repository.RepoRepo, assets *repository.AssetRepo, blobs *blobstore.Store, up *upstream.Client) *AssetService {
	return &AssetService{repos: repos, assets: assets, blobs: blobs, upstream: up, health: newProxyHealth(probeHEAD), negCache: newNegativeCache()}
}

// SetAutoBlockBase 调整 auto-block 退避起始时长（默认 40s；测试与调优用，<=0 忽略）。
func (s *AssetService) SetAutoBlockBase(d time.Duration) { s.health.setBase(d) }

// SetNegativeCacheTTL 调整 404 负缓存 TTL（默认 60s；测试与调优用，<=0 忽略）。
func (s *AssetService) SetNegativeCacheTTL(d time.Duration) { s.negCache.setTTL(d) }

// Status 返回 proxy 仓库上游连接状态（供 FR-114 状态展示）。
func (s *AssetService) Status(repoID int64) RemoteHealth { return s.health.status(repoID) }

// RecheckConnection 手动重测 proxy 仓库上游连接（FR-114）：立即 HEAD 探测并更新
// auto-block 状态，返回最新状态视图。仅 online 仓库可调用（调用方负责校验）。
func (s *AssetService) RecheckConnection(repoID int64, remoteURL string) RemoteHealth {
	return s.health.recheck(repoID, remoteURL)
}

// SetChangeRecorder 注入复制变更日志记录器（FR-83）；nil 表示不记录。
// 记录器为 ReplicationService 时，顺带把本服务的 404 负缓存注入对端共享，
// 使复制应用成功后能失效对应负缓存键（FR-111）。ReplicationService 是
// ChangeRecorder 的唯一实现，装配时经此路径完成负缓存共享，无需额外接线。
func (s *AssetService) SetChangeRecorder(r ChangeRecorder) {
	s.recorder = r
	if rs, ok := r.(*ReplicationService); ok {
		rs.SetNegativeCache(s.negCache)
	}
}

// Put 向 hosted 仓库发布一件制品：流式写入 blob，再覆盖写 asset 元数据。
// 仓库不存在返回 ErrNotFound；非 hosted 仓库（proxy/group）返回 ErrConflict。
// 格式（raw/maven/npm）语义由协议层按仓库 format 分派后自行处理，此处仅按内容寻址存字节。
func (s *AssetService) Put(repoName, path string, r io.Reader, contentType string) (*repository.Asset, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return nil, ErrConflict
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	hash, sha1sum, md5sum, size, err := s.blobs.Put(r)
	if err != nil {
		return nil, err
	}
	if err := s.assets.Upsert(repo.ID, path, hash, size, contentType, sha1sum, md5sum); err != nil {
		return nil, err
	}
	asset, err := s.assets.GetByPath(repo.ID, path)
	if err != nil {
		return nil, err
	}
	// 写路径成功：同路径此前「确认不存在」的负缓存失效，新制品立即可见。
	s.negCache.remove(repo.ID, path)
	// 变更数据携带创建/更新时间，对端应用时回填，保证复制两侧时间一致。
	s.recordChange(EntityAsset, AssetKey(repoName, path), OpPut, AssetChangeData{
		Path: path, BlobHash: hash, Size: size, ContentType: contentType, Sha1: sha1sum, Md5: md5sum,
		CreatedAt: asset.CreatedAt, UpdatedAt: asset.UpdatedAt,
	})
	return asset, nil
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
		return nil, nil, ErrNotFound
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	return s.resolve(ctx, repo, path, 0)
}

// resolve 是 Resolve 的内部递归实现，depth 用于遏制 group 成员环引用。
// 入口先查 404 负缓存：此前「确认不存在」且 TTL 未过期的路径直接 404，
// 不再回源/探测（覆盖 Resolve/groupGet/proxyGet 各层入口，见 FR-111）。
func (s *AssetService) resolve(ctx context.Context, repo *repository.Repository, path string, depth int) (*repository.Asset, io.ReadCloser, error) {
	if depth > maxResolveDepth {
		return nil, nil, ErrNotFound
	}
	if s.negCache.hit(repo.ID, path) {
		return nil, nil, ErrNotFound
	}
	switch repo.Type {
	case "proxy":
		return s.proxyGet(ctx, repo, path)
	case "group":
		return s.groupGet(ctx, repo, path, depth)
	default: // hosted 及未知类型均按本地读处理
		return s.localGet(repo, path)
	}
}

// localGet 读本地缓存：查 asset 元数据并打开对应 blob。路径不存在返回 ErrNotFound。
func (s *AssetService) localGet(repo *repository.Repository, path string) (*repository.Asset, io.ReadCloser, error) {
	asset, err := s.assets.GetByPath(repo.ID, path)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	rc, _, err := s.blobs.Open(asset.BlobHash)
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
		return asset, rc, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, nil, err
	}
	if s.upstream == nil {
		return nil, nil, ErrNotFound
	}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return nil, nil, err
	}
	if cfg.RemoteURL == "" {
		return nil, nil, ErrNotFound
	}
	// 仓库手动 offline（FR-113）：不回源，快速 404（本地已确认未命中）。
	if !repo.Online {
		return nil, nil, ErrNotFound
	}
	// 阻止窗口内直接快速失败，不发起上游连接（group 读不拖慢的关键）。
	if s.health.shouldBlock(repo.ID) {
		return nil, nil, ErrUpstream
	}

	key := fmt.Sprintf("%d\x00%s", repo.ID, path)
	_, err, _ = s.sf.Do(key, func() (any, error) {
		// 并发回源收敛：进入临界区先复查缓存，避免重复下载。
		if _, e := s.assets.GetByPath(repo.ID, path); e == nil {
			return nil, nil
		}
		body, header, ferr := s.upstream.Fetch(ctx, cfg.RemoteURL, path)
		if ferr != nil {
			// 上游失败（404 除外，404 是正常未命中）进入 auto-block，开启退避窗口。
			if !errors.Is(ferr, upstream.ErrNotFound) {
				s.health.recordFailure(repo.ID, cfg.RemoteURL)
			} else {
				// 明确 404：上游确认该路径不存在，写负缓存（FR-111）。
				// 仅此处写——配置缺失/无上游等本地 ErrNotFound 不算「确认不存在」。
				s.negCache.add(repo.ID, path)
			}
			return nil, mapUpstreamErr(ferr)
		}
		// 回源成功：上游可达，恢复可用并重置退避。
		s.health.recordSuccess(repo.ID)
		defer func() { _ = body.Close() }()
		hash, sha1sum, md5sum, size, ferr := s.blobs.Put(body)
		if ferr != nil {
			return nil, ferr
		}
		ct := header.Get("Content-Type")
		if ct == "" {
			ct = "application/octet-stream"
		}
		return nil, s.assets.Upsert(repo.ID, path, hash, size, ct, sha1sum, md5sum)
	})
	if err != nil {
		return nil, nil, err
	}
	return s.localGet(repo, path)
}

// groupMemberTimeout 是 group 仓库逐成员解析时每个成员的最大等待时间。
// 防止单个不可达 proxy 成员阻塞后续成员的探测；配合并行探测与 auto-block 阻止，
// 值取小（3s）让不可达上游快速失败、整体响应控制在秒级。
const groupMemberTimeout = 3 * time.Second

// groupGet 处理 group 仓库读，目标是"本地命中优先、缺失快速 404"：
//  1. 先串行快查全部成员的本地缓存（hosted / 已缓存 proxy 命中即返回，不触发网络）——
//     保持成员顺序语义（先成员命中优先）。
//  2. 本地全未命中 → 对未被 auto-block 阻止的 proxy 成员并行回源（取最快成功；首个成功后取消其余），
//     全部失败或超时快速返回 ErrNotFound。
//
// 每个成员限时 groupMemberTimeout；不可达成员经 proxyHealth 阻止窗口跳过（零连接）。
//
// 全未命中的 404 是否写负缓存（FR-111 M-1）：
// 只有「确认不存在」才写——即本次探测中没有任何成员被跳过（offline/auto-block 阻止）
// 或回源失败（上游 5xx/超时）。任一成员不可判定（可能只是暂时不可用），则不写负缓存，
// 避免把上游故障误缓存成永久不存在（上游恢复后同路径立即重试成功）。
func (s *AssetService) groupGet(ctx context.Context, repo *repository.Repository, path string, depth int) (*repository.Asset, io.ReadCloser, error) {
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return nil, nil, err
	}
	if depth > maxResolveDepth {
		return nil, nil, ErrNotFound
	}

	// uncertain 标记本次 404 是否「不可判定」：任何成员被跳过或回源失败即置位。
	var uncertain atomic.Bool

	// 阶段一：串行快查本地缓存（不触发网络），命中即返回，保持成员顺序语义。
	// 跳过 offline 成员（FR-113：手动置离线的仓库不参与聚合读）；离线成员未参与判定，
	// 其 404 结果不可信，置不可判定（FR-111 M-1）。
	for _, name := range cfg.Members {
		member, err := s.repos.GetByName(name)
		if err != nil {
			// 成员解析失败（悬空引用）：存在无法判定的成员，整体 404 不可信，置不可判定。
			uncertain.Store(true)
			continue
		}
		if !member.Online {
			uncertain.Store(true)
			continue
		}
		if asset, rc, err := s.localGet(member, path); err == nil {
			return asset, rc, nil
		}
	}

	// 阶段二：本地全未命中 → 并行回源未被阻止的 proxy 成员。
	// 收集需要回源的成员（proxy 且未处于阻止窗口；hosted 已在阶段一确认未命中）。
	type job struct {
		idx    int
		repoID int64
		member *repository.Repository
	}
	jobs := make([]job, 0, len(cfg.Members))
	for _, name := range cfg.Members {
		member, err := s.repos.GetByName(name)
		if err != nil {
			// 成员解析失败（悬空引用）：存在无法判定的成员，置不可判定（FR-111 M-1）。
			uncertain.Store(true)
			continue
		}
		if !member.Online {
			// 离线成员跳过：未参与本次判定，404 不可信，置不可判定（FR-111 M-1）。
			uncertain.Store(true)
			continue
		}
		if member.Type != "proxy" {
			continue // hosted 等已在阶段一确认未命中，不算跳过
		}
		if s.health.shouldBlock(member.ID) {
			// auto-block 阻止窗口内跳过：未参与判定，置不可判定（FR-111 M-1）。
			uncertain.Store(true)
			continue
		}
		jobs = append(jobs, job{idx: len(jobs), repoID: member.ID, member: member})
	}
	if len(jobs) == 0 {
		// 无可回源成员：全部 hosted 成员已在阶段一确认未命中（确认不存在，可写负缓存）；
		// 但若有成员被 offline/阻止跳过（uncertain），404 不可信，不写（FR-111 M-1）。
		if !uncertain.Load() {
			s.negCache.add(repo.ID, path)
		}
		return nil, nil, ErrNotFound
	}

	// 并行回源：首个成功即返回并取消其余；全部失败/超时快速 404。
	results := make([]bool, len(jobs))
	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			memberCtx, mCancel := context.WithTimeout(groupCtx, groupMemberTimeout)
			defer mCancel()
			asset, rc, err := s.resolve(memberCtx, j.member, path, depth+1)
			if err != nil {
				// 明确 404（含成员自身负缓存命中）是「确认不存在」；其余错误（回源失败/
				// 超时）说明该成员上游暂时不可用，本次 404 不可判定（FR-111 M-1）。
				if !errors.Is(err, ErrNotFound) {
					uncertain.Store(true)
				}
				return
			}
			// 成功：proxyGet 已把制品落本地缓存，此处关闭读流即可；
			// 记录成功成员供后续按 members 顺序 localGet 返回。
			_ = rc.Close()
			_ = asset
			results[j.idx] = true
			cancel() // 首个成功：取消其它等待者
		}(j)
	}
	wg.Wait()

	// 有成员成功：按 members 顺序返回第一个成功成员的本地缓存制品。
	for i, ok := range results {
		if ok {
			asset, rc, err := s.localGet(jobs[i].member, path)
			if err == nil {
				return asset, rc, nil
			}
		}
	}
	// 全未命中：确认不存在（无成员被跳过/失败）才写 group 层负缓存（FR-111 M-1）。
	if !uncertain.Load() {
		s.negCache.add(repo.ID, path)
	}
	return nil, nil, ErrNotFound
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

// Delete 删除制品元数据（blob 内容不即时清理）。
// 仓库或路径不存在均返回 ErrNotFound。
func (s *AssetService) Delete(repoName, path string) error {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return mapNotFound(err)
	}
	if err := s.assets.DeleteByPath(repo.ID, path); err != nil {
		return mapNotFound(err)
	}
	// 写路径成功：失效同路径负缓存（FR-111），避免删除后重传的制品被缓存 404 挡住。
	s.negCache.remove(repo.ID, path)
	s.recordChange(EntityAsset, AssetKey(repoName, path), OpDelete, TombstoneData{Deleted: true})
	return nil
}

// mapUpstreamErr 把 upstream 层错误映射为领域错误：404 视为未命中（ErrNotFound）、
// 超时映射 ErrUpstreamTimeout、其余映射 ErrUpstream（保留底层错误便于排错）。
func mapUpstreamErr(err error) error {
	switch {
	case errors.Is(err, upstream.ErrNotFound):
		return ErrNotFound
	case upstream.IsTimeout(err):
		return fmt.Errorf("%w: %v", ErrUpstreamTimeout, err)
	default:
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
}

// probeDialer 是后台 HEAD 探测的连接拨号器：固定 IPv4（与 upstream 回源一致），
// 规避宿主无 IPv6 出口导致的额外连接延迟。
var probeDialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

// probeHTTPClient 是后台 HEAD 探测专用 HTTP 客户端：仅 HEAD、开销小，
// IPv4 优先与回源一致；独立连接池，不与回源请求争用。
var probeHTTPClient = &http.Client{
	Timeout: probeTimeout,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return probeDialer.DialContext(ctx, "tcp4", addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	},
}

// probeHEAD 后台探测上游可达性：HEAD 请求上游根地址，仅检查连通性不下载内容。
// 判定与回源失败一致：传输错误/超时、5xx、401/403 视为不可达，其余视为可达。
// 与 upstream 回源一致走 IPv4 优先连接。
func probeHEAD(ctx context.Context, remoteURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, remoteURL, nil)
	if err != nil {
		return err
	}
	resp, err := probeHTTPClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if shouldBlockStatus(resp.StatusCode) {
		return fmt.Errorf("上游返回状态码 %d", resp.StatusCode)
	}
	return nil
}

// shouldBlockStatus 判断上游返回状态码是否应视为失败（5xx / 401 / 403）。
// 404 等其余 4xx 视为上游可达（仅资源缺失），不触发阻止。
func shouldBlockStatus(code int) bool {
	return code >= 500 || code == http.StatusUnauthorized || code == http.StatusForbidden
}
