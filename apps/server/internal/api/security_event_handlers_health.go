package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// SecurityAuditCollectionProbe 将内存快照收敛为公开探针与指标共用的有界状态。
func SecurityAuditCollectionProbe(h domain.SecurityAuditHealth) HealthStatusAuditCollection {
	switch h.State {
	case "stopping", "stopped":
		return HealthStatusAuditCollectionUnavailable
	case "degraded":
		return HealthStatusAuditCollectionDegraded
	case "running":
		if h.Failure != "" || h.Coverage == "gapped" {
			return HealthStatusAuditCollectionDegraded
		}
		if h.Coverage == "complete" && !h.LastSuccess.IsZero() && !h.CoveredFrom.IsZero() && h.CoveredThrough.After(h.CoveredFrom) {
			return HealthStatusAuditCollectionHealthy
		}
	}
	return HealthStatusAuditCollectionUnknown
}

func (h *Handlers) GetSecurityAuditCollectionHealth(c *gin.Context) {
	if _, ok := requireSecurityAuditSession(c); !ok {
		return
	}
	if _, ok := securityAuditQueryParams(c, ""); !ok {
		return
	}
	if h.securityAuditQuery == nil {
		securityAuditQueryError(c, nil)
		return
	}
	snapshot := h.securityAuditQuery.collector.Health()
	c.JSON(http.StatusOK, toAPISecurityAuditCollectionHealth(snapshot))
}

func toAPISecurityAuditCollectionHealth(h domain.SecurityAuditHealth) SecurityAuditCollectionHealth {
	state := SecurityAuditCollectionHealthState(h.State)
	if !state.Valid() {
		state = SecurityAuditCollectionHealthStateStarting
	}
	coverage := SecurityAuditCoverage(h.Coverage)
	if !coverage.Valid() || h.CoveredFrom.IsZero() || !h.CoveredThrough.After(h.CoveredFrom) {
		coverage = SecurityAuditCoverageUnknown
	}
	response := SecurityAuditCollectionHealth{
		State: state, Coverage: coverage, LastSuccess: securityAuditTime(h.LastSuccess), HeartbeatAt: securityAuditTime(h.HeartbeatAt),
		Queued: ptr(h.Queued), QueueBytes: ptr(h.QueueBytes), ObservedCount: ptr(h.ObservedCount), SuppressedCount: ptr(h.SuppressedCount),
	}
	if h.CoveredThrough.After(h.CoveredFrom) && !h.CoveredFrom.IsZero() {
		response.CoveredFrom, response.CoveredThrough = securityAuditTime(h.CoveredFrom), securityAuditTime(h.CoveredThrough)
	}
	if response.LastSuccess != nil && response.HeartbeatAt != nil {
		response.Watermark = ptr(h.Watermark)
	}
	if failure := SecurityAuditCollectionHealthFailure(h.Failure); failure.Valid() {
		response.Failure = &failure
	}
	gap := h.LastGap
	gapCoverage, reason := SecurityAuditCollectionGapCoverage(gap.Coverage), SecurityAuditCollectionGapGapReason(gap.Reason)
	if gapCoverage.Valid() && reason.Valid() && !gap.Start.IsZero() && gap.End.After(gap.Start) {
		response.LastGap = &SecurityAuditCollectionGap{Coverage: gapCoverage, GapReason: reason, WindowStart: gap.Start.UTC(), WindowEnd: gap.End.UTC()}
		if gapCoverage == SecurityAuditCollectionGapCoverageGapped {
			response.LastGap.ObservedCount, response.LastGap.SuppressedCount = gap.ObservedCount, gap.SuppressedCount
		}
	}
	return response
}

func securityAuditTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return ptr(t.UTC())
}
