package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// ApplyRepositoryAssetOperation 应用统一原子制品操作（FR-105）。
func (h *Handlers) ApplyRepositoryAssetOperation(c *gin.Context, name RepoNameParam) {
	operationID := domain.NewOperationID()
	if !requireAssetOperationAdmin(c, operationID) {
		return
	}
	if h.assets == nil {
		writeAssetOperationError(c, http.StatusConflict, "conflict", "制品存储未就绪", operationID)
		return
	}
	var req AssetOperationRequest
	if !bindAssetOperationJSON(c, &req, operationID) {
		return
	}
	if strings.TrimSpace(req.OverrideReason) == "" || len([]rune(req.OverrideReason)) > 512 {
		writeAssetOperationError(c, http.StatusBadRequest, "bad_request", "overrideReason 必填且长度不得超过 512", operationID)
		return
	}
	action := assetOperationAction(domain.AssetOperationAction(req.Action))
	op := domain.AssetOperation{
		Action:  domain.AssetOperationAction(req.Action),
		Targets: make([]domain.AssetOperationTarget, 0, len(req.Targets)),
		Audit:   h.assetOperationAudit(c, action, string(name), strings.TrimSpace(req.OverrideReason)),
	}
	if req.DestinationPath != nil {
		op.DestinationPath = *req.DestinationPath
	}
	if req.NewPath != nil {
		op.NewPath = *req.NewPath
	}
	for _, target := range req.Targets {
		op.Targets = append(op.Targets, domain.AssetOperationTarget{
			Type: domain.AssetOperationTargetType(target.Type),
			Path: target.Path,
		})
	}
	result, err := h.assets.ApplyOperation(string(name), op)
	if err != nil {
		operationID := ""
		if result != nil {
			operationID = result.OperationID
		}
		writeDomainErrWithOperationID(c, err, operationID)
		return
	}
	c.JSON(http.StatusOK, AssetOperationResponse{OperationId: result.OperationID, Affected: result.Affected})
}

func requireAssetOperationAdmin(c *gin.Context, operationID string) bool {
	p, ok := auth.PrincipalFrom(c)
	if !ok {
		writeAssetOperationError(c, http.StatusUnauthorized, "unauthenticated", "未认证或凭据无效", operationID)
		return false
	}
	if p.WebLoginDisabled {
		writeAssetOperationError(c, http.StatusForbidden, "web_login_disabled", "该账号已禁止登录管理端", operationID)
		return false
	}
	if !p.IsAdmin() {
		writeAssetOperationError(c, http.StatusForbidden, "forbidden", "需要管理员权限", operationID)
		return false
	}
	return true
}

func bindAssetOperationJSON(c *gin.Context, dst any, operationID string) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		writeAssetOperationError(c, http.StatusBadRequest, "bad_request", "请求体格式错误", operationID)
		return false
	}
	return true
}

func writeAssetOperationError(c *gin.Context, status int, code, message, operationID string) {
	c.JSON(status, gin.H{
		"error":       gin.H{"code": code, "message": message},
		"operationId": operationID,
	})
}

func assetOperationAction(action domain.AssetOperationAction) string {
	switch action {
	case domain.AssetOperationMove:
		return "asset.move"
	case domain.AssetOperationRename:
		return "asset.rename"
	default:
		return "asset.delete"
	}
}

func (h *Handlers) assetOperationAudit(c *gin.Context, action, repoName, overrideReason string) domain.AssetOperationAudit {
	return h.operationAudit(c, action, repoName, "overrideReason="+overrideReason)
}

// ProtocolAssetOperationAudit 为原生协议删除构造同事务审计，不依赖管理端的覆盖理由。
func (h *Handlers) ProtocolAssetOperationAudit(c *gin.Context, action, repoName string) domain.AssetOperationAudit {
	return h.operationAudit(c, action, repoName, "protocol=native")
}

func (h *Handlers) operationAudit(c *gin.Context, action, repoName, detail string) domain.AssetOperationAudit {
	actor := repository.OperationActor{}
	var tokenID *int64
	tokenName := ""
	if principal, ok := auth.PrincipalFrom(c); ok {
		actor.Username = principal.Username
		actor.UserID = &principal.UserID
		actor.AuthSource = principal.AuthSource
		if principal.TokenID > 0 {
			tokenID = &principal.TokenID
			tokenName = principal.TokenName
		}
	}
	if h.auditLogs == nil {
		return domain.AssetOperationAudit{Actor: actor}
	}
	return domain.AssetOperationAudit{
		Actor: actor,
		Commit: func(operationID string, items []repository.AssetMutationItem) repository.MutationCompletionHook {
			return func(tx *sqlx.Tx) error {
				for _, item := range items {
					if err := h.auditLogs.InsertTx(tx, repository.AuditLogEntry{
						TS: time.Now().UTC().Format(time.RFC3339Nano), Actor: actor.Username, Action: action,
						EntityType: "asset", EntityKey: repoName + "/" + item.Path, Repo: repoName,
						Detail: "operationId=" + operationID + " " + detail, Result: "ok",
						IP: c.ClientIP(), UserID: actor.UserID, AuthSource: actor.AuthSource, TokenID: tokenID,
						TokenName: tokenName, UserAgent: c.GetHeader("User-Agent"), RequestID: requestID(c), SourceNode: h.auditSourceNode,
						CorrelationID: operationID,
					}); err != nil {
						return err
					}
				}
				return nil
			}
		},
	}
}

// mapLegacyBatchDelete 把旧 paths 契约转换为统一 Raw 删除目标。
func mapLegacyBatchDelete(req BatchDeleteAssetsRequest) domain.AssetOperation {
	targets := make([]domain.AssetOperationTarget, 0, len(req.Paths))
	for _, p := range req.Paths {
		targets = append(targets, domain.AssetOperationTarget{Type: domain.AssetTargetRawPath, Path: strings.TrimSpace(p)})
	}
	return domain.AssetOperation{Action: domain.AssetOperationDelete, Targets: targets}
}
