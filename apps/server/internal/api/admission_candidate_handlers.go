package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// GetPrivateAdmissionCandidate 仅向原发起的交互用户提供白名单状态，不提供字节。
func (h *Handlers) GetPrivateAdmissionCandidate(c *gin.Context, operationID PrivateAdmissionOperationIdParam) {
	c.Header("Cache-Control", "private, no-store")
	p, _ := auth.PrincipalFrom(c)
	if h.admissionPrincipal != nil {
		var err error
		p, err = h.admissionPrincipal(c.Request)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				writeAdmissionError(c, domain.ErrAdmissionUnauthenticated)
			} else {
				writeAdmissionError(c, domain.ErrAdmissionUnavailable)
			}
			return
		}
	}
	if p == nil {
		writeAdmissionError(c, domain.ErrAdmissionUnauthenticated)
		return
	}
	if p.Kind != auth.KindSession || p.AuthSource != auth.AuthSourceWebJWT || p.WebLoginDisabled {
		writeAdmissionError(c, domain.ErrAdmissionForbidden)
		return
	}
	if h.admission == nil {
		writeAdmissionError(c, domain.ErrAdmissionUnavailable)
		return
	}
	status, err := h.admission.Status(c.Request.Context(), p, operationID)
	if err != nil {
		writeAdmissionError(c, err)
		return
	}
	out := PrivateAdmissionCandidateStatus{OperationId: status.OperationID, RepositoryId: status.RepositoryID, TargetVersion: status.TargetVersion, State: PrivateAdmissionCandidateStatusState(status.State),
		ManifestVersion: status.ManifestVersion, ManifestDigest: status.ManifestDigest, ExpiresAt: status.ExpiresAt, Coverage: PrivateAdmissionCandidateStatusCoverage("unknown")}
	if status.ErrorCode != "" {
		code := PrivateAdmissionCandidateStatusErrorCode(status.ErrorCode)
		out.ErrorCode = &code
	}
	c.JSON(http.StatusOK, out)
}

func writeAdmissionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrAdmissionUnauthenticated):
		auth.WriteError(c, http.StatusUnauthorized, "unauthenticated", "未认证或凭据无效")
	case errors.Is(err, domain.ErrAdmissionForbidden):
		auth.WriteError(c, http.StatusForbidden, "admission_forbidden", "当前凭据不允许查询候选")
	case errors.Is(err, repository.ErrNotFound):
		auth.WriteError(c, http.StatusNotFound, "admission_not_found", "候选不存在")
	default:
		auth.WriteError(c, http.StatusServiceUnavailable, "admission_unavailable", "候选状态暂不可确认")
	}
}
