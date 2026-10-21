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
// **该端点不得被前端轮询**：每次调用要在 asset 表与 FTS 影子表上各做一次全表计数，成本与
// 表大小线性相关（百万级 asset 上是秒级开销），且刻意不加缓存——缓存会把刚发生的漂移在
// 窗口内报成正常。调用场景限定为运维手动查询。
//
// 响应口径：
//   - ready       —— 索引可用于加速搜索：回填已完成（completed_at 非空）且没有**漏索引**。
//     回填未完成期间搜索整体走既有 LIKE 路径（正确但尚未提速）；发现漏索引时 ready 也会转
//     false，此时索引仍被查询路径使用、只是可能少返回行，必须重建才能恢复。
//     注意：**孤儿索引行不会让 ready 转 false**（它不丢结果），因此不能只看 ready 判断索引
//     是否干净，要看 missingRows / orphanRows。
//   - total       —— 回填开始时的 asset 行数快照（total_rows），仅用于进度展示。
//   - indexed     —— 已回填行数，按 last_id 水位回 asset 真源计数（COUNT(*) WHERE id <= last_id）。
//   - lastId      —— 水位（已回填到的最大 asset.id）。
//   - updatedAt   —— 状态行最后一次变更时刻，用于判断「回填是否还在推进」。
//   - indexedRows —— 索引里**真实存在**的文档数（FTS5 的 docsize 影子表行数），与 indexed 的
//     「水位以内资产行数」不是同一口径。
//   - assetRows   —— asset 真源当前行数。
//   - missingRows —— **漏索引**的行数：asset 里有、索引里没有。这是危险方向（这些制品搜不到），
//     ready 只因它转 false。监控要盯的就是这个字段。
//   - orphanRows  —— **孤儿索引行**的行数：索引里有、asset 已不存在。仓库删除经级联带走 asset
//     行时天然产生，只浪费索引空间、不丢结果，因此不影响 ready。
//
// 两个方向必须分开报：合成一个差额（indexedRows - assetRows）时，「漏 k 行 + 孤儿 k 行」
// 恰好抵消成 0，而且任何删过仓库的实例都会带着孤儿行让差额恒为正——无论按差额还是按
// 「差额非 0」告警，第一次删仓库之后告警就永久在响，真正的漏索引信号被埋在噪音里。
//
// **口径盲区：这四个数字一致不代表索引内容正确。** 若某条 asset 行的 path 被就地
// `UPDATE asset SET path = ?`，external content 表不会重建该行索引，行数却完全一致，于是
// 端点报「无漂移」，而新名字搜不到、旧名字还能搜到。现任写入路径不产生这种 UPDATE（冲突键
// 就是 path，改名走「插新行 + 删旧行」），但运维脚本/手工 SQL 可以；那种情况下唯一的手段是
// 全量重建。
//
// 为什么 indexed 不用「索引行数」：asset_path_fts 是 external content 表，
// `SELECT COUNT(*) FROM asset_path_fts` 实测返回的是**内容表**的行数（索引被清空后仍报全量），
// 不是索引条目数；而读 FTS 影子表又会把「在线同步新写入的行」与「仓库级联删除留下的孤儿行」
// 一起算进来，使其与 total 不可比。按水位回真源计数与 total 同源同口径，能直接看出剩余量。
//
// 为什么还要额外报漂移：索引正确性依赖「所有 asset 写入必经仓储层」的约定，而回填只会跑一次；
// 一旦有旁路写入（运维脚本、手工 SQL、未来的迁移），漂移就是**永久静默**的，端点若只报进度，
// 用户唯一能感知到的就是「某些制品搜不到」。这几个字段把那份约定变成可观测事实。
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
		"ready":       status.Ready,
		"indexed":     status.Indexed,
		"total":       status.Total,
		"lastId":      status.LastID,
		"updatedAt":   status.UpdatedAt,
		"indexedRows": status.IndexRows,
		"assetRows":   status.AssetRows,
		"missingRows": status.MissingRows,
		"orphanRows":  status.OrphanRows,
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
