package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// batchDeleteAssetsLimit 单次批量删除的路径上限（超限 400）。
const batchDeleteAssetsLimit = 500

// BatchDeleteRepositoryAssets 批量删除仓库制品（仅管理员，FR-103）。
// 对每个 path 复用 AssetService.Delete（元数据删 + 复制 tombstone）并逐条写 asset.delete 审计；
// 逐条尽力：部分失败进入 failed 明细，不做整体回滚。
func (h *Handlers) BatchDeleteRepositoryAssets(c *gin.Context, name RepoNameParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.assets == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "制品存储未就绪")
		return
	}
	// 预检仓库存在性：不存在返回 404（与契约及 devmock 行为一致），避免逐条进 failed 造成 200 假象。
	if _, err := h.repos.Get(string(name)); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			auth.WriteError(c, http.StatusNotFound, "not_found", "仓库不存在")
			return
		}
		auth.WriteError(c, http.StatusInternalServerError, "internal", "内部错误")
		return
	}
	var req BatchDeleteAssetsRequest
	if !bindJSON(c, &req) {
		return
	}
	if len(req.Paths) == 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "paths 不能为空")
		return
	}
	if len(req.Paths) > batchDeleteAssetsLimit {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "paths 单次最多 500 条")
		return
	}
	for _, p := range req.Paths {
		if p == "" {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "paths 中存在空路径")
			return
		}
	}
	resp := BatchDeleteAssetsResponse{Failed: make([]BatchDeleteAssetFailure, 0)}
	for _, p := range req.Paths {
		if err := h.assets.Delete(name, p); err != nil {
			resp.Failed = append(resp.Failed, BatchDeleteAssetFailure{Path: p, Error: err.Error()})
			continue
		}
		resp.Deleted++
		h.AuditLog(c, "asset.delete", "asset", string(name)+"/"+p, string(name), "", "ok")
	}
	c.JSON(http.StatusOK, resp)
}
