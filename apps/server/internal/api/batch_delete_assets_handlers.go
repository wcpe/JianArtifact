package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// batchDeleteAssetsLimit 单次批量删除的路径上限（超限 400）。
const batchDeleteAssetsLimit = 500

// BatchDeleteRepositoryAssets 是兼容入口（已弃用），委托统一原子资产操作引擎。
func (h *Handlers) BatchDeleteRepositoryAssets(c *gin.Context, name RepoNameParam) {
	operationID := domain.NewOperationID()
	if !requireAssetOperationAdmin(c, operationID) {
		return
	}
	if h.assets == nil {
		writeAssetOperationError(c, http.StatusConflict, "conflict", "制品存储未就绪", operationID)
		return
	}
	// 预检仓库存在性：不存在返回 404（与契约及 devmock 行为一致），避免逐条进 failed 造成 200 假象。
	if _, err := h.repos.Get(string(name)); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeAssetOperationError(c, http.StatusNotFound, "not_found", "仓库不存在", operationID)
			return
		}
		writeAssetOperationError(c, http.StatusInternalServerError, "internal", "内部错误", operationID)
		return
	}
	var req BatchDeleteAssetsRequest
	if !bindAssetOperationJSON(c, &req, operationID) {
		return
	}
	if len(req.Paths) == 0 {
		writeAssetOperationError(c, http.StatusBadRequest, "bad_request", "paths 不能为空", operationID)
		return
	}
	if strings.TrimSpace(req.OverrideReason) == "" || len([]rune(req.OverrideReason)) > 512 {
		writeAssetOperationError(c, http.StatusBadRequest, "bad_request", "overrideReason 必填且长度不得超过 512", operationID)
		return
	}
	if len(req.Paths) > batchDeleteAssetsLimit {
		writeAssetOperationError(c, http.StatusBadRequest, "bad_request", "paths 单次最多 500 条", operationID)
		return
	}
	for _, p := range req.Paths {
		if p == "" {
			writeAssetOperationError(c, http.StatusBadRequest, "bad_request", "paths 中存在空路径", operationID)
			return
		}
	}
	op := mapLegacyBatchDelete(req)
	op.Audit = h.assetOperationAudit(c, "asset.delete", string(name), strings.TrimSpace(req.OverrideReason))
	result, err := h.assets.ApplyOperation(string(name), op)
	if err != nil {
		if result != nil {
			operationID = result.OperationID
		}
		writeDomainErrWithOperationID(c, err, operationID)
		return
	}
	c.JSON(http.StatusOK, BatchDeleteAssetsResponse{Deleted: result.Affected, Failed: []BatchDeleteAssetFailure{}})
}
