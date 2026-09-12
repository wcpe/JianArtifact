package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// 写入冻结窗口（FR-135）的接口层窗口策略。
//
// 为什么策略放在这一层而不是 domain：`FreezeController` 刻意保持中性
// （`until` 零值 = 手动冻结、不自动解冻），时限策略是接口约定而非领域规则。
// 本层**永远**解析出一个有界的 until，因此生产路径不存在「无限期冻结」——
// 忘记解冻会让服务退化成假死，是这类开关最危险的失败模式。
const (
	freezeDefaultTTL = 2 * time.Hour
	freezeMinTTL     = time.Minute
	freezeMaxTTL     = 24 * time.Hour
)

// GetWriteFreezeState 查询当前写入冻结状态（仅管理员）。
func (h *Handlers) GetWriteFreezeState(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.freeze == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "冻结控制器未启用")
		return
	}
	c.JSON(http.StatusOK, toAPIWriteFreezeState(h.freeze.State()))
}

// FreezeWrites 冻结节点写入（仅管理员）。
func (h *Handlers) FreezeWrites(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.freeze == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "冻结控制器未启用")
		return
	}
	var req FreezeWritesRequest
	if !bindJSON(c, &req) {
		return
	}
	until, err := resolveFreezeUntil(req, time.Now())
	if err != nil {
		auth.WriteError(c, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	c.JSON(http.StatusOK, toAPIWriteFreezeState(h.freeze.Freeze(until, derefString(req.Reason))))
}

// UnfreezeWrites 解冻节点写入（仅管理员）。幂等：未冻结时同样返回当前状态。
func (h *Handlers) UnfreezeWrites(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.freeze == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "冻结控制器未启用")
		return
	}
	c.JSON(http.StatusOK, toAPIWriteFreezeState(h.freeze.Unfreeze()))
}

// resolveFreezeUntil 把请求里的窗口解析为一个有界的绝对截止时间。
// now 由调用方传入以便测试。
func resolveFreezeUntil(req FreezeWritesRequest, now time.Time) (time.Time, error) {
	if req.Until != nil {
		until := req.Until.UTC()
		if !until.After(now) {
			return time.Time{}, fmt.Errorf("until 必须晚于当前时间")
		}
		if until.After(now.Add(freezeMaxTTL)) {
			return time.Time{}, fmt.Errorf("until 不得超过当前时间 + %s", freezeMaxTTL)
		}
		return until, nil
	}
	ttl := freezeDefaultTTL
	if req.TtlSeconds != nil {
		ttl = time.Duration(*req.TtlSeconds) * time.Second
	}
	if ttl < freezeMinTTL {
		ttl = freezeMinTTL
	}
	if ttl > freezeMaxTTL {
		ttl = freezeMaxTTL
	}
	return now.Add(ttl), nil
}

// toAPIWriteFreezeState 映射为契约类型。
// 未冻结时必须**省略** until/frozenAt，否则 time.Time 的零值会被序列化成
// "0001-01-01T00:00:00Z"，前端无从区分「没有窗口」与「窗口已过期」。
func toAPIWriteFreezeState(s domain.FreezeState) WriteFreezeState {
	out := WriteFreezeState{Frozen: s.Frozen}
	if !s.Frozen {
		return out
	}
	if !s.Until.IsZero() {
		until := s.Until.UTC()
		out.Until = &until
	}
	if !s.FrozenAt.IsZero() {
		frozenAt := s.FrozenAt.UTC()
		out.FrozenAt = &frozenAt
	}
	if s.Reason != "" {
		reason := s.Reason
		out.Reason = &reason
	}
	return out
}
