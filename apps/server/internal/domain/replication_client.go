package domain

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// ReplicationClient 是复制拉取方的 HTTP 客户端（FR-84）：从对端 GET 拉取变更、
// 应用到本地（ReplicationService.Apply）、blob 只传缺失。
//
// 全程 GET（无 PUT 推送），规避 Cloudflare Tunnel / CDN 上传体积限制（ADR-0013）。
// 对端 URL 与令牌由调用方（FR-85 调度 / FR-86 管理面）提供。
type ReplicationClient struct {
	peerURL string // 对端基址（如 https://repo.wcpe.top）
	token   string // 同步令牌（与对端 JIAN_SYNC_TOKEN 一致）
	repl    *ReplicationService
	blobs   *blobstore.Store
	http    *http.Client
}

// NewReplicationClient 构造 ReplicationClient。
func NewReplicationClient(peerURL, token string, repl *ReplicationService, blobs *blobstore.Store) *ReplicationClient {
	return &ReplicationClient{
		peerURL: peerURL,
		token:   token,
		repl:    repl,
		blobs:   blobs,
		// 变更列表拉取用适中超时；blob 下载用 blobHTTP 客户端（更宽松超时，见下）。
		http: &http.Client{Timeout: 60 * time.Second},
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
func (c *ReplicationClient) Pull(since int64, limit int) ([]repository.Change, int64, error) {
	u := fmt.Sprintf("%s/api/v1/cluster/sync/pull?since=%d&limit=%d", c.peerURL, since, limit)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("拉取复制变更：%w", err)
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
	Failed   int            // 应用失败条数
	Blobs    int            // 补拉 blob 数
	ByEntity map[string]int // 变更实体构成（entity_type → 条数）

	// pendingBlobs 本轮待补拉的缺失 blob 哈希（去重）；由 Sync 在元数据应用后并发拉取。
	pendingBlobs map[string]struct{}
}

// Sync 执行一轮同步：从 since 开始循环拉取变更 → 应用到本地（元数据优先）→
// 并发补拉缺失 blob → 返回推进后的统计（含对端最新 seq 于 ToSeq，调度器存为本地水位）。
//
// FR-A/FR-B 性能修复：asset 元数据先落库（快、不阻塞），缺失 blob 用工作协程池并发拉取，
// 避免逐个串行拉 blob（每个可能 30s 超时）导致批量同步卡死。
// 单条 Apply 失败不阻塞整批（记录日志，靠 FR-85 对账兜底）。
func (c *ReplicationClient) Sync(since int64) (SyncStats, error) {
	stats := SyncStats{FromSeq: since, ToSeq: since, ByEntity: map[string]int{}, pendingBlobs: map[string]struct{}{}}
	for {
		changes, _, err := c.Pull(stats.ToSeq, syncBatchLimit)
		if err != nil {
			return stats, err
		}
		for _, ch := range changes {
			c.applyChange(ch, &stats)
		}
		// 水位推进到本批最后一条的 seq（而非对端最新 seq）：对端一次性积压大量变更时，
		// 若直接跳到对端最新会跳过中间未拉取的变更，导致数据缺失（FR-85 回归修复）。
		if len(changes) > 0 {
			stats.ToSeq = changes[len(changes)-1].Seq
		}
		if len(changes) < syncBatchLimit {
			break
		}
	}
	// 元数据全部应用后，并发补拉本轮缺失 blob（FR-A/B）。
	c.fetchPendingBlobs(&stats)
	return stats, nil
}

// applyChange 应用一条对端变更（仅元数据），并登记缺失 blob 待补拉；累计统计。
// asset 的 blob 内容不在此处同步拉取（避免阻塞），交由 fetchPendingBlobs 并发处理。
func (c *ReplicationClient) applyChange(ch repository.Change, stats *SyncStats) {
	stats.Changes++
	stats.ByEntity[ch.EntityType]++
	if err := c.repl.Apply(ch); err != nil {
		stats.Failed++
		log.Printf("复制应用变更失败 seq=%d entity=%s key=%s：%v", ch.Seq, ch.EntityType, ch.EntityKey, err)
		return
	}
	stats.Applied++
	if ch.EntityType != EntityAsset || ch.Op != OpPut {
		return
	}
	var d AssetChangeData
	if err := json.Unmarshal([]byte(ch.Data), &d); err != nil || d.BlobHash == "" {
		return
	}
	if c.blobs.Exists(d.BlobHash) {
		return // blob 已有，不重复拉取
	}
	stats.pendingBlobs[d.BlobHash] = struct{}{}
}

// fetchPendingBlobs 用工作协程池并发补拉本轮登记的缺失 blob（FR-A 性能修复）。
// 单 blob 失败仅记录日志，不阻塞整体（缺失 blob 由后续对账兜底补拉）。
func (c *ReplicationClient) fetchPendingBlobs(stats *SyncStats) {
	hashes := make([]string, 0, len(stats.pendingBlobs))
	for h := range stats.pendingBlobs {
		hashes = append(hashes, h)
	}
	if len(hashes) == 0 {
		return
	}
	// 无界缓冲，worker 消费；失败不重试（对账兜底）。
	// 每个 worker 本地累计成功数，最后汇总到 stats，避免对 stats 的并发写竞争。
	ch := make(chan string)
	counts := make([]int, blobFetchWorkers)
	var wg sync.WaitGroup
	for i := 0; i < blobFetchWorkers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// 每 worker 独立 blob 客户端（避免并发复用连接池的流式干扰）。
			bc := c.blobHTTP()
			for h := range ch {
				if err := c.fetchBlobToStore(bc, h, stats); err == nil {
					counts[idx]++
				}
			}
		}(i)
	}
	for _, h := range hashes {
		ch <- h
	}
	close(ch)
	wg.Wait()
	for _, n := range counts {
		stats.Blobs += n
	}
}

// fetchBlobToStore 从对端拉取单个缺失 blob 并落盘（内容寻址，返回哈希须与期望一致）。
func (c *ReplicationClient) fetchBlobToStore(hc *http.Client, wantHash string, stats *SyncStats) error {
	rc, err := c.fetchBlob(hc, wantHash)
	if err != nil {
		log.Printf("复制补拉 blob 失败 hash=%s：%v", wantHash, err)
		return err
	}
	defer func() { _ = rc.Close() }()

	gotHash, _, _, _, err := c.blobs.Put(rc)
	if err != nil {
		log.Printf("复制落盘 blob 失败 hash=%s：%v", wantHash, err)
		return err
	}
	if gotHash != wantHash {
		// 传输损坏会以实际内容哈希落盘（孤儿 blob，本期不清理）；记录日志暴露问题。
		log.Printf("复制 blob 哈希不匹配 期望=%s 实得=%s", wantHash, gotHash)
	}
	return nil
}

// fetchBlob 使用指定客户端从对端拉取 blob 内容流。
func (c *ReplicationClient) fetchBlob(hc *http.Client, hash string) (io.ReadCloser, error) {
	u := fmt.Sprintf("%s/api/v1/cluster/sync/blob/%s", c.peerURL, hash)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

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
