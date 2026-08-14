package api

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
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
	PeerURL   *string `json:"peerUrl,omitempty"`   // 对端基址（空串表示清空）
	PeerToken *string `json:"peerToken,omitempty"` // 对端同步令牌
	Enabled   *bool   `json:"enabled,omitempty"`   // 自动同步开关
}

// GetClusterStatus 返回集群同步状态（FR-86），仅管理员。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) GetClusterStatus(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	c.JSON(http.StatusOK, h.replication.ClusterStatus())
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
	if req.PeerURL != nil || req.PeerToken != nil {
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
	c.JSON(http.StatusOK, h.replication.ClusterStatus())
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
