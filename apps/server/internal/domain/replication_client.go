package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// syncBatchLimit 是复制拉取单批上限（服务端与客户端共用语义，见 FR-84 spec）。
const syncBatchLimit = 500

// blobFetchWorkers 是 blob 并发拉取的工作协程数（FR-A 性能修复）：
// 批量 asset 同步时并行补拉缺失 blob，避免串行逐个拉取导致整批卡死。
const blobFetchWorkers = 8

// pullMaxRetries 是拉取变更的瞬时网络错误重试次数（修复：对端经 CDN/隧道
// 偶发 connection reset，直接失败会在同步历史留下噪声失败）。
const pullMaxRetries = 2

// pullRetryBase 是重试退避基准时长（依次 200ms、400ms）。
const pullRetryBase = 200 * time.Millisecond

// errParentNotReady 表示变更的父实体尚未同步（暂不可应用），应由客户端缓冲重试，
// 待父实体到达后重放，而非中断整轮或永久卡死。
var errParentNotReady = errors.New("父实体未就绪")

// parentRetryMax 是父未就绪变更的重试轮数上限；超过后判定为永久脏数据并跳过。
const parentRetryMax = 20

// ReplicationClient 是复制拉取方的 HTTP 客户端（FR-84）：从对端 GET 拉取变更、
// 应用到本地（ReplicationService.Apply）、blob 只传缺失。
//
// 全程 GET（无 PUT 推送），规避 Cloudflare Tunnel / CDN 上传体积限制（ADR-0013）。
// 对端 URL 与令牌由调用方（FR-85 调度 / FR-86 管理面）提供。
type ReplicationClient struct {
	peerURL   string // 对端基址（如 https://repo.wcpe.top）
	token     string // 同步令牌（与对端 JIAN_SYNC_TOKEN 一致）
	repl      *ReplicationService
	blobs     *blobstore.Store
	applyLogs *repository.ReplicationApplyLogRepo
	http      *http.Client

	// parentAttempts 记录各父实体未就绪的连续重试轮数（跨 Sync 轮次保留，内存态）。
	// 达 parentRetryMax 后判定永久脏数据并跳过（持久化到 skipped）。
	mu             sync.Mutex
	parentAttempts map[parentGroupKey]int
	// pendingGroups 跨轮保留的父未就绪分组，按对端与父键隔离。
	pendingGroups map[parentGroupKey]*parentGroup
}

// NewReplicationClient 构造 ReplicationClient。
// applyLogs 仅由客户端持有并写入，空值用于兼容不需要审计的轻量调用方。
func NewReplicationClient(peerURL, token string, repl *ReplicationService, blobs *blobstore.Store, applyLogs ...*repository.ReplicationApplyLogRepo) *ReplicationClient {
	// 变更列表拉取用适中超时；blob 下载用 blobHTTP 客户端（更宽松超时，见下）。
	// 对端可能经 CDN/隧道（如 repo1.wcpe.top → EdgeOne），CDN 会按空闲策略关闭 keep-alive
	// 连接，默认 Transport 复用失效连接会偶发 connection reset；故设置较短 IdleConnTimeout，
	// 让空闲连接主动重建（修复间歇同步失败）。
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 2
	transport.IdleConnTimeout = 30 * time.Second
	// Cloudflare 等 CDN 对复制长连接的 HTTP/2 流偶发返回 INTERNAL_ERROR；
	// 复制端点全是幂等 GET，强制 HTTP/1.1 可避免该代理层协议错误。
	transport.ForceAttemptHTTP2 = false
	var applyLogRepo *repository.ReplicationApplyLogRepo
	if len(applyLogs) > 0 {
		applyLogRepo = applyLogs[0]
	}
	return &ReplicationClient{
		peerURL:        peerURL,
		token:          token,
		repl:           repl,
		blobs:          blobs,
		applyLogs:      applyLogRepo,
		http:           &http.Client{Timeout: 60 * time.Second, Transport: transport},
		parentAttempts: map[parentGroupKey]int{},
		pendingGroups:  map[parentGroupKey]*parentGroup{},
	}
}

// blobHTTP 返回 blob 下载专用客户端：blob 可能较大且对端经隧道/慢链路，
// 固定 30s 易超时，故用更宽松的超时（FR-A 超时优化）。
func (c *ReplicationClient) blobHTTP() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

// SetPeer 更新对端基址与同步令牌（FR-88：对端配置 web 可改，调度器每轮动态更新）。
// 仅在调度器单 goroutine 内调用，无需加锁。
func (c *ReplicationClient) SetPeer(peerURL, token string) {
	c.peerURL = peerURL
	c.token = token
}

// pullResponse 是 GET /api/v1/cluster/sync/pull 的响应体。
type pullResponse struct {
	Changes   []repository.Change `json:"changes"`
	LatestSeq int64               `json:"latestSeq"`
}

// Pull 从对端拉取 seq 大于 since 的变更（最多 limit 条），返回变更与对端最新 seq。
// 对瞬时网络错误（CDN/隧道偶发 connection reset、超时、EOF 等）做有界重试：
// GET 幂等安全，重试不产生副作用（修复间歇同步失败）。
func (c *ReplicationClient) Pull(since int64, limit int) ([]repository.Change, int64, error) {
	return c.pull(c.peerURL, c.token, since, limit)
}

func (c *ReplicationClient) pull(peerURL, token string, since int64, limit int) ([]repository.Change, int64, error) {
	u := fmt.Sprintf("%s/api/v1/cluster/sync/pull?since=%d&limit=%d", peerURL, since, limit)
	var lastErr error
	for attempt := 0; attempt <= pullMaxRetries; attempt++ {
		changes, latest, err := c.pullOnce(u, token)
		if err == nil {
			return changes, latest, nil
		}
		lastErr = err
		if !isRetryableNetErr(err) || attempt == pullMaxRetries {
			break
		}
		time.Sleep(pullRetryBase * time.Duration(1<<attempt))
	}
	return nil, 0, fmt.Errorf("拉取复制变更：%w", lastErr)
}

// pullOnce 执行一次拉取请求；返回变更与对端最新 seq，或 Do/解码阶段的错误。
func (c *ReplicationClient) pullOnce(u, token string) ([]repository.Change, int64, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("拉取复制变更失败：HTTP %d", resp.StatusCode)
	}
	var out pullResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, 0, fmt.Errorf("解析复制变更响应：%w", err)
	}
	return out.Changes, out.LatestSeq, nil
}

// isRetryableNetErr 判断是否为可重试的瞬时网络错误：
// 网络层错误（连接重置、超时、拒绝等，实现 net.Error）或响应中途 EOF。
// HTTP 状态码错误不在此列（401/404 等重试无意义；5xx 由下一轮调度兜底）。
func isRetryableNetErr(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF)
}

// FetchBlob 从对端按内容哈希流式拉取 blob；调用方负责关闭返回流。
func (c *ReplicationClient) FetchBlob(hash string) (io.ReadCloser, error) {
	u := fmt.Sprintf("%s/api/v1/cluster/sync/blob/%s", c.peerURL, hash)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取 blob：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("拉取 blob 失败：HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// SyncStats 是一轮同步的统计（供同步历史日志可视化）。
type SyncStats struct {
	FromSeq  int64          // 起始水位
	ToSeq    int64          // 结束水位（失败时为已推进水位）
	Changes  int            // 拉取变更条数
	Applied  int            // 成功应用条数
	Failed   int            // 应用失败条数（含父未就绪待重试与跳过）
	Blobs    int            // 补拉 blob 数
	ByEntity map[string]int // 变更实体构成（entity_type → 条数）
	Pending  int            // 本轮仍未就绪待重试的变更条数
	Skipped  int            // 本轮判定永久脏数据跳过的变更条数

	// pendingBlobs 按哈希与来源对端去重，同时保留全部来源变更引用。
	pendingBlobs map[pendingBlobKey][]sourceRef
}

// sourceRef 标识一条远端来源变更；节点与序号共同构成唯一身份。
type sourceRef struct {
	sourceNode string
	sourceSeq  int64
}

type pendingBlobKey struct {
	hash    string
	peerURL string
	token   string
}

type parentGroupKey struct {
	peerURL   string
	parentKey string
}

// parentGroup 是同一对端、同一父实体下未就绪变更的聚合。
type parentGroup struct {
	peerURL   string
	token     string
	parentKey string
	refs      []sourceRef
	refSet    map[sourceRef]struct{}
	minSeq    int64
	maxSeq    int64
}

// newParentGroup 构造父分组。
func newParentGroup(peerURL, token, parentKey string, ref sourceRef) *parentGroup {
	g := &parentGroup{
		peerURL: peerURL, token: token, parentKey: parentKey,
		refs: []sourceRef{}, refSet: map[sourceRef]struct{}{},
	}
	g.add(ref)
	return g
}

// add 向分组追加一个未就绪来源，重复来源只保留一次。
func (g *parentGroup) add(ref sourceRef) {
	if _, ok := g.refSet[ref]; ok {
		return
	}
	g.refSet[ref] = struct{}{}
	g.refs = append(g.refs, ref)
	if len(g.refs) == 1 || ref.sourceSeq < g.minSeq {
		g.minSeq = ref.sourceSeq
	}
	if len(g.refs) == 1 || ref.sourceSeq > g.maxSeq {
		g.maxSeq = ref.sourceSeq
	}
}

// contains 判断来源是否仍在分组中。
func (g *parentGroup) contains(ref sourceRef) bool {
	_, ok := g.refSet[ref]
	return ok
}

// remove 从分组移除一个已成功重放的来源，并更新区间。
func (g *parentGroup) remove(ref sourceRef) {
	if !g.contains(ref) {
		return
	}
	delete(g.refSet, ref)
	for i, existing := range g.refs {
		if existing == ref {
			g.refs = append(g.refs[:i], g.refs[i+1:]...)
			break
		}
	}
	if len(g.refs) == 0 {
		g.minSeq, g.maxSeq = 0, 0
		return
	}
	g.minSeq, g.maxSeq = g.refs[0].sourceSeq, g.refs[0].sourceSeq
	for _, existing := range g.refs[1:] {
		if existing.sourceSeq < g.minSeq {
			g.minSeq = existing.sourceSeq
		}
		if existing.sourceSeq > g.maxSeq {
			g.maxSeq = existing.sourceSeq
		}
	}
}

// Sync 执行一轮同步：先应用全部元数据，再并发补拉缺失 blob。
// 父未就绪的变更不阻塞水位推进（进入待重试分组，待父实体就绪后区间重放）；
// 连续 parentRetryMax 轮仍未就绪判定为永久脏数据并跳过。水位始终正常推进。
func (c *ReplicationClient) Sync(since int64) (SyncStats, error) {
	peerURL, token := c.peerURL, c.token
	stats := SyncStats{FromSeq: since, ToSeq: since, ByEntity: map[string]int{}, pendingBlobs: map[pendingBlobKey][]sourceRef{}}
	cursor := since

	skipped, err := c.repl.ReadSkipped(peerURL)
	if err != nil {
		return stats, fmt.Errorf("读取跳过集合：%w", err)
	}
	skippedByPeer := map[string]map[int64]struct{}{peerURL: skipped}

	// 阶段一：正常拉取并应用（父未就绪入跨轮 pendingGroups，水位推进）。
	for {
		changes, _, err := c.pull(peerURL, token, cursor, syncBatchLimit)
		if err != nil {
			return stats, err
		}
		for _, ch := range changes {
			if _, ok := skipped[ch.Seq]; ok {
				stats.Skipped++
				continue
			}
			if err := c.applyChange(ch, &stats, peerURL, token); err != nil {
				if errors.Is(err, errParentNotReady) {
					pk := c.parentKeyOf(ch)
					if pk == "" {
						skipped[ch.Seq] = struct{}{}
						stats.Skipped++
						continue
					}
					key := parentGroupKey{peerURL: peerURL, parentKey: pk}
					ref := sourceRef{sourceNode: ch.NodeID, sourceSeq: ch.Seq}
					c.mu.Lock()
					g, ok := c.pendingGroups[key]
					if !ok {
						g = newParentGroup(peerURL, token, pk, ref)
						c.pendingGroups[key] = g
					} else {
						g.add(ref)
					}
					c.mu.Unlock()
					continue
				}
				return stats, err
			}
		}
		if len(changes) > 0 {
			cursor = changes[len(changes)-1].Seq
		}
		if len(changes) < syncBatchLimit {
			break
		}
	}

	// 阶段二：遍历全部跨轮 pendingGroups——父就绪重放，未就绪计数达上限跳过。
	c.mu.Lock()
	keys := make([]parentGroupKey, 0, len(c.pendingGroups))
	for key := range c.pendingGroups {
		keys = append(keys, key)
	}
	c.mu.Unlock()
	for _, key := range keys {
		c.mu.Lock()
		g := c.pendingGroups[key]
		c.mu.Unlock()
		if g == nil {
			continue
		}
		exists, err := c.parentExists(g.parentKey)
		if err != nil {
			return stats, err
		}
		if !exists {
			if g.peerURL != peerURL {
				stats.Pending += len(g.refs)
				continue
			}
			c.mu.Lock()
			c.parentAttempts[key]++
			attempts := c.parentAttempts[key]
			c.mu.Unlock()
			stats.Pending += len(g.refs)
			if attempts >= parentRetryMax {
				groupSkipped := skippedByPeer[g.peerURL]
				if groupSkipped == nil {
					groupSkipped, err = c.repl.ReadSkipped(g.peerURL)
					if err != nil {
						return stats, fmt.Errorf("读取跳过集合：%w", err)
					}
					skippedByPeer[g.peerURL] = groupSkipped
				}
				for _, ref := range g.refs {
					groupSkipped[ref.sourceSeq] = struct{}{}
					c.updateApplyResult(ref, ApplyResultSkippedPermanent, fmt.Sprintf("父实体 %s 连续 %d 轮未就绪", g.parentKey, attempts), "")
				}
				stats.Skipped += len(g.refs)
				log.Printf("复制调度：跳过无法应用的变更（父 %s，重试 %d 轮仍不存在，对端 %s）", g.parentKey, attempts, g.peerURL)
				c.mu.Lock()
				delete(c.pendingGroups, key)
				delete(c.parentAttempts, key)
				c.mu.Unlock()
			}
			continue
		}
		// 父已就绪：按产生分组的对端分页重放；成功者移出分组。
		if err := c.replayRange(g, &stats); err != nil {
			return stats, err
		}
		c.mu.Lock()
		if len(g.refs) == 0 {
			delete(c.pendingGroups, key)
			delete(c.parentAttempts, key)
		} else {
			stats.Pending += len(g.refs)
		}
		c.mu.Unlock()
	}

	// 阶段三：并发补拉缺失 blob。
	if err := c.fetchPendingBlobs(&stats); err != nil {
		return stats, err
	}
	// 按产生分组的对端持久化跳过集合。
	for skippedPeer, peerSkipped := range skippedByPeer {
		if err := c.repl.WriteSkipped(skippedPeer, peerSkipped); err != nil {
			log.Printf("复制调度：跳过集合持久化失败（对端 %s）：%v", skippedPeer, err)
		}
	}
	stats.ToSeq = cursor
	return stats, nil
}

// replayRange 对父分组 [minSeq, maxSeq] 做区间分页重放：逐批拉取并应用该分组来源。
// 成功者移出分组并更新区间。父已就绪时调用。
func (c *ReplicationClient) replayRange(g *parentGroup, stats *SyncStats) error {
	from := g.minSeq - 1
	for {
		changes, _, err := c.pull(g.peerURL, g.token, from, syncBatchLimit)
		if err != nil {
			return err
		}
		for _, ch := range changes {
			ref := sourceRef{sourceNode: ch.NodeID, sourceSeq: ch.Seq}
			if ch.Seq < g.minSeq || ch.Seq > g.maxSeq || !g.contains(ref) {
				continue
			}
			if err := c.applyChange(ch, stats, g.peerURL, g.token); err != nil {
				if errors.Is(err, errParentNotReady) {
					continue
				}
				return err
			}
			g.remove(ref)
		}
		if len(changes) == 0 {
			break
		}
		last := changes[len(changes)-1].Seq
		if last >= g.maxSeq {
			break
		}
		from = last
	}
	return nil
}

// parentKeyOf 返回变更的父实体自然键（经 ReplicationService 解析）。
func (c *ReplicationClient) parentKeyOf(ch repository.Change) string {
	return c.repl.ParentKeyOf(ch)
}

// parentExists 判断父实体是否存在（经 ReplicationService 查询）。
func (c *ReplicationClient) parentExists(parentKey string) (bool, error) {
	return c.repl.ParentExists(parentKey)
}

// applyChange 应用一条对端变更（仅元数据），并登记缺失 blob 待补拉。
// ReplicationClient 是接收审计唯一写入者；ReplicationService 只返回 ApplyOutcome。
func (c *ReplicationClient) applyChange(ch repository.Change, stats *SyncStats, peerURL, token string) error {
	stats.Changes++
	stats.ByEntity[ch.EntityType]++
	outcome := c.repl.ApplyOutcome(ch)
	seenAt := time.Now().UTC().Format(time.RFC3339Nano)
	if outcome.Err != nil {
		stats.Failed++
		c.observeApply(ch, outcome.Result, outcome.Detail, outcome.Err, seenAt, peerURL)
		if outcome.Result == ApplyResultPendingParent {
			return errParentNotReady
		}
		return fmt.Errorf("应用复制变更 seq=%d entity=%s key=%s：%w", ch.Seq, ch.EntityType, ch.EntityKey, outcome.Err)
	}
	stats.Applied++
	if outcome.Result != ApplyResultApplied || outcome.BlobHash == "" || c.blobs.Exists(outcome.BlobHash) {
		c.observeApply(ch, outcome.Result, outcome.Detail, nil, seenAt, peerURL)
		return nil
	}
	c.observeApply(ch, ApplyResultMetadataPendingBlob, "元数据已应用，等待补拉 blob", nil, seenAt, peerURL)
	appendPendingBlobRef(stats.pendingBlobs, pendingBlobKey{hash: outcome.BlobHash, peerURL: peerURL, token: token}, sourceRef{sourceNode: ch.NodeID, sourceSeq: ch.Seq})
	return nil
}

func appendPendingBlobRef(pending map[pendingBlobKey][]sourceRef, key pendingBlobKey, ref sourceRef) {
	for _, existing := range pending[key] {
		if existing == ref {
			return
		}
	}
	pending[key] = append(pending[key], ref)
}

func (c *ReplicationClient) observeApply(ch repository.Change, result, detail string, applyErr error, seenAt, peerURL string) {
	if c.applyLogs == nil {
		return
	}
	entry := repository.ReplicationApplyLog{
		SourceNode:  ch.NodeID,
		SourceSeq:   ch.Seq,
		PeerURL:     peerURL,
		EntityType:  ch.EntityType,
		EntityKey:   ch.EntityKey,
		Op:          ch.Op,
		Result:      result,
		Detail:      detail,
		FirstSeenAt: seenAt,
		LastSeenAt:  seenAt,
	}
	if applyErr != nil {
		entry.LastError = applyErr.Error()
		entry.LastErrorAt = seenAt
	}
	if err := c.applyLogs.Observe(entry); err != nil {
		log.Printf("复制接收审计写入失败 source=%s seq=%d：%v", ch.NodeID, ch.Seq, err)
	}
}

func (c *ReplicationClient) updateApplyResult(ref sourceRef, result, detail, lastError string) {
	if c.applyLogs == nil {
		return
	}
	seenAt := time.Now().UTC().Format(time.RFC3339Nano)
	lastErrorAt := ""
	if lastError != "" {
		lastErrorAt = seenAt
	}
	if err := c.applyLogs.UpdateResult(ref.sourceNode, ref.sourceSeq, result, detail, lastError, lastErrorAt, seenAt); err != nil {
		log.Printf("复制接收审计更新失败 source=%s seq=%d：%v", ref.sourceNode, ref.sourceSeq, err)
	}
}

// fetchPendingBlobs 用工作协程池并发补拉本轮登记的缺失 blob。
func (c *ReplicationClient) fetchPendingBlobs(stats *SyncStats) error {
	keys := make([]pendingBlobKey, 0, len(stats.pendingBlobs))
	for key := range stats.pendingBlobs {
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil
	}
	jobs := make(chan pendingBlobKey)
	counts := make([]int, blobFetchWorkers)
	errs := make([]error, blobFetchWorkers)
	var wg sync.WaitGroup
	for i := 0; i < blobFetchWorkers; i++ {
		wg.Add(1)
		go c.fetchBlobWorker(i, jobs, stats.pendingBlobs, counts, errs, &wg)
	}
	for _, key := range keys {
		jobs <- key
	}
	close(jobs)
	wg.Wait()
	for i, n := range counts {
		stats.Blobs += n
		if errs[i] != nil {
			return errs[i]
		}
	}
	return nil
}

func (c *ReplicationClient) fetchBlobWorker(idx int, jobs <-chan pendingBlobKey, refs map[pendingBlobKey][]sourceRef, counts []int, errs []error, wg *sync.WaitGroup) {
	defer wg.Done()
	hc := c.blobHTTP()
	for key := range jobs {
		if err := c.fetchBlobToStore(hc, key.hash, key.peerURL, key.token); err != nil {
			wrapped := fmt.Errorf("补拉 blob %s：%w", key.hash, err)
			log.Printf("复制补拉 blob 失败 hash=%s：%v", key.hash, err)
			for _, ref := range refs[key] {
				c.updateApplyResult(ref, ApplyResultBlobFailed, "blob 补拉失败", wrapped.Error())
			}
			if errs[idx] == nil {
				errs[idx] = wrapped
			}
			continue
		}
		for _, ref := range refs[key] {
			c.updateApplyResult(ref, ApplyResultApplied, "元数据与 blob 已应用", "")
		}
		counts[idx]++
	}
}

// fetchBlobToStore 从指定对端拉取单个缺失 blob 并落盘。
func (c *ReplicationClient) fetchBlobToStore(hc *http.Client, wantHash, peerURL, token string) error {
	rc, err := c.fetchBlob(hc, wantHash, peerURL, token)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	gotHash, _, _, _, err := c.blobs.Put(rc)
	if err != nil {
		return fmt.Errorf("落盘 blob：%w", err)
	}
	if gotHash != wantHash {
		return fmt.Errorf("blob 哈希不匹配，期望=%s 实得=%s", wantHash, gotHash)
	}
	return nil
}

// fetchBlob 使用指定客户端从对端拉取 blob 内容流。
func (c *ReplicationClient) fetchBlob(hc *http.Client, hash, peerURL, token string) (io.ReadCloser, error) {
	u := fmt.Sprintf("%s/api/v1/cluster/sync/blob/%s", peerURL, hash)

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取 blob：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("拉取 blob 失败：HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}
