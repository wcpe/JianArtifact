package domain

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// syncBatchLimit 是复制拉取单批上限（服务端与客户端共用语义，见 FR-84 spec）。
const syncBatchLimit = 500

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
		http:    &http.Client{Timeout: 30 * time.Second},
	}
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

// Sync 执行一轮同步：从 since 开始循环拉取变更 → 应用到本地 → blob 缺失补拉，
// 返回推进后的对端最新 seq（调度器存为本地水位）。
// 单条 Apply 失败不阻塞整批（记录日志，靠 FR-85 对账兜底）。
func (c *ReplicationClient) Sync(since int64) (int64, error) {
	latest := since
	for {
		changes, newSeq, err := c.Pull(latest, syncBatchLimit)
		if err != nil {
			return latest, err
		}
		for _, ch := range changes {
			c.applyChange(ch)
		}
		latest = newSeq
		if len(changes) < syncBatchLimit {
			break
		}
	}
	return latest, nil
}

// applyChange 应用一条对端变更，并在 asset put 时按需补拉 blob。
func (c *ReplicationClient) applyChange(ch repository.Change) {
	if err := c.repl.Apply(ch); err != nil {
		log.Printf("复制应用变更失败 seq=%d entity=%s key=%s：%v", ch.Seq, ch.EntityType, ch.EntityKey, err)
		return
	}
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
	c.fetchAndStoreBlob(d.BlobHash)
}

// fetchAndStoreBlob 从对端拉取缺失 blob 并落盘（内容寻址，返回哈希须与期望一致）。
func (c *ReplicationClient) fetchAndStoreBlob(wantHash string) {
	rc, err := c.FetchBlob(wantHash)
	if err != nil {
		log.Printf("复制补拉 blob 失败 hash=%s：%v", wantHash, err)
		return
	}
	defer func() { _ = rc.Close() }()

	gotHash, _, _, _, err := c.blobs.Put(rc)
	if err != nil {
		log.Printf("复制落盘 blob 失败 hash=%s：%v", wantHash, err)
		return
	}
	if gotHash != wantHash {
		// 传输损坏会以实际内容哈希落盘（孤儿 blob，本期不清理）；记录日志暴露问题。
		log.Printf("复制 blob 哈希不匹配 期望=%s 实得=%s", wantHash, gotHash)
	}
}
