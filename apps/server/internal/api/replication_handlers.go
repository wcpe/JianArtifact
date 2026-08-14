package api

import (
	"errors"
	"net/http"
	"os"
	"strconv"

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

// SetSyncEnabledRequest 是集群启停端点（FR-86）的请求体。
type SetSyncEnabledRequest struct {
	Enabled *bool `json:"enabled"`
}

// GetClusterStatus 返回集群同步状态（FR-86），仅管理员。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) GetClusterStatus(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	c.JSON(http.StatusOK, h.replication.ClusterStatus(h.clusterPeerURL, h.clusterTokenSet))
}

// PutClusterStatus 更新同步调度启停开关（FR-86），仅管理员。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) PutClusterStatus(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req SetSyncEnabledRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Enabled == nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "缺少 enabled 字段")
		return
	}
	if err := h.replication.SetSyncEnabled(*req.Enabled); err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, h.replication.ClusterStatus(h.clusterPeerURL, h.clusterTokenSet))
}
