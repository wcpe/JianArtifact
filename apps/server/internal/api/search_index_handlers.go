package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// 搜索索引的两个运维端点（状态查询与全量重建），均仅管理员。
//
// 两者按 /api/v1/search 的既有先例作为**非契约端点**注册（经 WithProtocolRoutes，
// 不进 api/openapi.yaml）：它们服务运维观测与故障修复，形状会随后续批次（查询改写、
// suggest）演进，且不属于任何协议客户端依赖的稳定面。把它写进契约会为内部的、易变的
// 进度形状引入对外承诺，代价高于收益；若将来前端需要长期展示进度，再连同生成物
// 与 devmock 一并收口。

// GetSearchIndexStatus 返回搜索索引回填状态（仅管理员）。
//
// 响应口径：
//   - ready     —— 回填是否完成。search_index_state 只有 completed_at，NULL 即未完成；
//     未完成期间搜索整体走既有 LIKE 路径（正确但尚未提速）。
//   - total     —— 回填开始时的 asset 行数快照（total_rows），仅用于进度展示。
//   - indexed   —— 已回填行数，按 last_id 水位回 asset 真源计数（COUNT(*) WHERE id <= last_id）。
//   - lastId    —— 水位（已回填到的最大 asset.id）。
//   - updatedAt —— 状态行最后一次变更时刻，用于判断「回填是否还在推进」。
//
// 为什么 indexed 不用「索引行数」：asset_path_fts 是 external content 表，
// `SELECT COUNT(*) FROM asset_path_fts` 实测返回的是**内容表**的行数（索引被清空后仍报全量），
// 不是索引条目数；而读 FTS 影子表又会把「在线同步新写入的行」与「仓库级联删除留下的孤儿行」
// 一起算进来，使其与 total 不可比。按水位回真源计数与 total 同源同口径，能直接看出剩余量。
func (h *Handlers) GetSearchIndexStatus(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.searchIndex == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "搜索索引服务未启用")
		return
	}
	status, err := h.searchIndex.Status()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ready":     status.Ready,
		"indexed":   status.Indexed,
		"total":     status.Total,
		"lastId":    status.LastID,
		"updatedAt": status.UpdatedAt,
	})
}

// RebuildSearchIndex 受理一次搜索索引的全量重建（仅管理员）。
//
// 202 只表示**已受理**：清空索引 → 水位归零 → 后台分批重灌都在服务内异步进行。
// 重建全程在线——清空后 completed_at 为 NULL，搜索自动走降级路径，正确性不受影响。
// 已有回填或重建在跑时重复触发返回 409（错误码 job_running，对齐既有运维接口
// 手动触发维护作业时的口径）。
func (h *Handlers) RebuildSearchIndex(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.searchIndex == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "搜索索引服务未启用")
		return
	}
	if err := h.searchIndex.Rebuild(); err != nil {
		if errors.Is(err, domain.ErrSearchIndexRunning) {
			auth.WriteError(c, http.StatusConflict, "job_running", "索引回填或重建正在运行，请稍后再试")
			return
		}
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
}
