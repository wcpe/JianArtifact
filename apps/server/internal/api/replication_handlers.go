package api

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// ClusterSyncPullResponse 是复制拉取端点（FR-84）的响应体。
type ClusterSyncPullResponse struct {
	Changes   []repository.Change `json:"changes"`
	LatestSeq int64               `json:"latestSeq"`
}

// GetClusterSyncPull 返回 seq 大于 since 的变更（按 seq 升序，最多 limit 条）与对端最新 seq。
// 非契约端点，经 WithProtocolRoutes 注册；同步令牌由路由中间件校验（main 组装）。
func (h *Handlers) GetClusterSyncPull(c *gin.Context) {
	since, err := strconv.ParseInt(c.DefaultQuery("since", "0"), 10, 64)
	if err != nil || since < 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "since 须为非负整数")
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "500"))
	if limit <= 0 {
		limit = 500
	}

	changes, err := h.replication.ListSince(since, limit)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	latest, err := h.replication.LatestSeq()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, ClusterSyncPullResponse{Changes: changes, LatestSeq: latest})
}

// GetClusterSyncBlob 按内容哈希流式返回 blob；哈希不存在返回 404。
// 非契约端点，经 WithProtocolRoutes 注册；同步令牌由路由中间件校验。
func (h *Handlers) GetClusterSyncBlob(c *gin.Context) {
	rc, _, err := h.replication.OpenBlob(c.Param("hash"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.Status(http.StatusNotFound)
			return
		}
		writeDomainErr(c, err)
		return
	}
	defer func() { _ = rc.Close() }()
	c.DataFromReader(http.StatusOK, -1, "application/octet-stream", rc, nil)
}

// ClusterConfigRequest 是集群配置端点（FR-88）的请求体：可选字段，传哪个改哪个。
type ClusterConfigRequest struct {
	PeerURL   *string        `json:"peerUrl,omitempty"`   // 对端基址（空串表示清空；单值兼容）
	PeerToken *string        `json:"peerToken,omitempty"` // 对端同步令牌（单值兼容）
	Enabled   *bool          `json:"enabled,omitempty"`   // 自动同步开关
	Peers     *[]domain.Peer `json:"peers,omitempty"`     // 多对端列表（FR-D：传则全量替换对端配置）
}

// GetClusterStatus 返回集群同步状态（FR-86），仅管理员。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) GetClusterStatus(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	st := h.replication.ClusterStatus()
	// FR-C：附加最近一次同步的进度/构成摘要（同步历史最新一条，若有）。
	if h.syncLogs != nil {
		if entries, err := h.syncLogs.List(1, 0); err == nil && len(entries) > 0 {
			e := entries[0]
			st.LastSync = &domain.LastSyncSummary{
				StartedAt:    e.StartedAt,
				FinishedAt:   orEmpty(e.FinishedAt),
				Success:      e.Success,
				FromSeq:      e.FromSeq,
				ToSeq:        e.ToSeq,
				Changes:      e.Changes,
				Applied:      e.Applied,
				Failed:       e.Failed,
				Blobs:        e.Blobs,
				EntityCounts: e.EntityCounts,
				ErrorText:    e.ErrorText,
			}
		}
	}
	c.JSON(http.StatusOK, st)
}

// orEmpty 返回指针值或空串（可选字符串字段兜底）。
func orEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// PutClusterStatus 更新集群配置（对端 URL/令牌/自动同步开关，FR-88），仅管理员。
// 仅保存配置，不开始同步（自动由开关控制，手动由 sync-now 触发）。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) PutClusterStatus(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req ClusterConfigRequest
	if !bindJSON(c, &req) {
		return
	}
	// FR-D：传多对端列表则全量替换（优先于单值 peerUrl/peerToken）。
	if req.Peers != nil {
		if err := h.preservePeerTokens(*req.Peers); err != nil {
			writeDomainErr(c, err)
			return
		}
		if err := h.replication.SetPeers(*req.Peers); err != nil {
			writeDomainErr(c, err)
			return
		}
	} else if req.PeerURL != nil || req.PeerToken != nil {
		peerURL, token, err := h.replication.PeerConfig()
		if err != nil {
			writeDomainErr(c, err)
			return
		}
		if req.PeerURL != nil {
			peerURL = *req.PeerURL
		}
		if req.PeerToken != nil {
			token = *req.PeerToken
		}
		if err := h.replication.SetPeerConfig(peerURL, token); err != nil {
			writeDomainErr(c, err)
			return
		}
	}
	if req.Enabled != nil {
		if err := h.replication.SetSyncEnabled(*req.Enabled); err != nil {
			writeDomainErr(c, err)
			return
		}
	}
	peersN := 0
	if req.Peers != nil {
		peersN = len(*req.Peers)
	}
	h.AuditLog(c, "cluster.config", "cluster", "cluster", "", "peers="+strconv.Itoa(peersN)+" enabled="+orEmptyBool(req.Enabled), "ok")
	c.JSON(http.StatusOK, h.replication.ClusterStatus())
}

// orEmptyBool 返回指针布尔值或 "unset"（可选字段兜底，用于审计 detail）。
func orEmptyBool(p *bool) string {
	if p == nil {
		return "unset"
	}
	return strconv.FormatBool(*p)
}

// preservePeerTokens 为未提交新令牌的对端保留原令牌；优先按 URL 匹配，URL 修改时按原位置继承。
func (h *Handlers) preservePeerTokens(peers []domain.Peer) error {
	current, err := h.replication.Peers()
	if err != nil {
		return err
	}
	byURL := make(map[string]string, len(current))
	for _, peer := range current {
		byURL[peer.URL] = h.replication.PeerToken(peer.URL)
	}
	for i := range peers {
		if peers[i].Token != "" {
			continue
		}
		if token := byURL[peers[i].URL]; token != "" {
			peers[i].Token = token
			continue
		}
		if i < len(current) {
			peers[i].Token = h.replication.PeerToken(current[i].URL)
		}
	}
	return nil
}

// PostClusterSyncNow 触发一次立即同步（FR-88），无论自动开关状态，仅管理员。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) PostClusterSyncNow(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.replicationSched == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "同步调度器未运行")
		return
	}
	h.replicationSched.SyncNow(30 * time.Second)
	c.JSON(http.StatusOK, h.replication.ClusterStatus())
}

// SyncLogListResponse 是同步历史列表响应（FR-88 可视化）。
type SyncLogListResponse struct {
	Items []repository.SyncLogEntry `json:"items"`
	Total int                       `json:"total"`
}

// GetClusterSyncLogs 返回同步历史记录（按开始时间倒序分页），仅管理员。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) GetClusterSyncLogs(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.syncLogs == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "同步日志存储未就绪")
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if offset < 0 {
		offset = 0
	}
	items, err := h.syncLogs.List(limit, offset)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	total, err := h.syncLogs.Count()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	// 空数据时返回 [] 而非 null：前端渲染依赖数组（list.items.length），null 会崩溃。
	if items == nil {
		items = []repository.SyncLogEntry{}
	}
	c.JSON(http.StatusOK, SyncLogListResponse{Items: items, Total: total})
}

// SyncLogChangeListResponse 是某次同步的具体变更列表响应（FR-98）。
type SyncLogChangeListResponse struct {
	Items []repository.Change `json:"items"`
	Total int                 `json:"total"`
}

// GetClusterSyncLogChanges 返回某次同步记录（:id）对应的具体变更列表（FR-98）：
// 按该次同步的 fromSeq→toSeq 区间从 repl_change 反推（分页），仅管理员。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) GetClusterSyncLogChanges(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.syncLogs == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "同步日志存储未就绪")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "同步记录 id 非法")
		return
	}
	entry, err := h.syncLogs.Get(id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			auth.WriteError(c, http.StatusNotFound, "not_found", "同步记录不存在")
			return
		}
		writeDomainErr(c, err)
		return
	}
	if entry.Changes == 0 {
		c.JSON(http.StatusOK, SyncLogChangeListResponse{Items: []repository.Change{}, Total: 0})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if offset < 0 {
		offset = 0
	}
	// FR-98 增强：按实体类型分类展示（entityType 非空时后端过滤，保证分类统计准确）。
	entityType := c.Query("entityType")
	items, err := h.replication.ListChangeRange(entry.FromSeq, entry.ToSeq, limit, offset, entityType)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	total, err := h.replication.CountChangeRange(entry.FromSeq, entry.ToSeq, entityType)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	if items == nil {
		items = []repository.Change{}
	}
	c.JSON(http.StatusOK, SyncLogChangeListResponse{Items: items, Total: total})
}
