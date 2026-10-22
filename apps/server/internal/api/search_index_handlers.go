package api

import (
	"context"
	"errors"
	"net/http"
	"time"

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

// searchIndexStatusBudget 是状态端点单次查询的**服务端预算**。
//
// 该端点的三个计数（水位回填数、索引行数、资产行数）都是全表扫描，成本随表大小线性增长，
// 百万级 asset 上是秒级开销（见 GetSearchIndexStatus 的说明）。预算取 15 秒：常规实例
// （秒级）留有十倍以上余量，而真正跑飞的查询不会把连接与调用方一起拖住。
const searchIndexStatusBudget = 15 * time.Second

// SearchIndexStatusContextual 是 SearchIndexController 的**可选**扩展能力：若实现方支持
// context，状态查询就能带上调用方的取消与预算语义。
//
// 做成可选接口而不是直接改 Status 的签名，是因为接口层无法单独决定这件事：domain 侧
// SearchIndexService.Status() 目前不带 ctx，其下游 SearchIndexStore 的 Load / CountIndexed /
// MeasureDrift 也都不带 ctx——只把接口层改成 Status(ctx) 会立刻打断装配层的实现匹配。
// 等 domain 侧提供 StatusContext(context.Context) (domain.SearchIndexStatus, error) 之后，
// 装配进来的实现会自动走进带 ctx 的分支，本文件无需再改。
type SearchIndexStatusContextual interface {
	StatusContext(ctx context.Context) (domain.SearchIndexStatus, error)
}

// searchIndexStatus 在给定 context 下取回填状态快照。
//
// 两条路径：
//   - 实现方支持 context → 直接把 ctx 传下去，取消 / 预算由下游查询自行响应（最优）；
//   - 实现方不支持 → 用「后台执行 + 只等 ctx」把它包起来。此时**只能限制调用方的等待时间，
//     不能中断底层计数**：Status() 不接受 ctx，goroutine 会一直跑到计数结束才退出，期间仍
//     占着连接。这是当前 domain 签名的硬限制，不是本层的选择。
func (h *Handlers) searchIndexStatus(ctx context.Context) (domain.SearchIndexStatus, error) {
	if ctxScoped, ok := h.searchIndex.(SearchIndexStatusContextual); ok {
		return ctxScoped.StatusContext(ctx)
	}
	type result struct {
		status domain.SearchIndexStatus
		err    error
	}
	// 缓冲 1：即使已经超时返回，后台 goroutine 仍能写入并退出，不会永久阻塞在发送上。
	done := make(chan result, 1)
	go func() {
		status, err := h.searchIndex.Status()
		done <- result{status: status, err: err}
	}()
	select {
	case r := <-done:
		return r.status, r.err
	case <-ctx.Done():
		return domain.SearchIndexStatus{}, ctx.Err()
	}
}

// GetSearchIndexStatus 返回搜索索引回填状态（仅管理员）。
//
// **该端点不得被前端轮询**：每次调用要在 asset 表与 FTS 影子表上各做一次全表计数，成本与
// 表大小线性相关（百万级 asset 上是秒级开销），且刻意不加缓存——缓存会把刚发生的漂移在
// 窗口内报成正常。调用场景限定为运维手动查询。
//
// **超时与取消**：单次调用有 searchIndexStatusBudget 的服务端预算，客户端断开时不再继续
// 等待。但**底层计数无法被中断**——domain 侧的 Status() 不接受 context，本层只能在等待侧
// 收口（详见 searchIndexStatus）。因此这条预算限制的是「调用方等多久」，不是「查询跑多久」。
// 若客户端断开，本端点不写响应体、也不记内部错误日志（口径与 writeDomainErr 的取消分支一致）。
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
	ctx, cancel := context.WithTimeout(c.Request.Context(), searchIndexStatusBudget)
	defer cancel()
	status, err := h.searchIndexStatus(ctx)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			// 客户端已断开：写响应体没人会读，也不能记成服务端故障（口径同 writeDomainErr
			// 的取消分支）。这里显式处理，是因为取消很可能由本函数的预算包裹直接产生。
			c.Abort()
			return
		case errors.Is(err, context.DeadlineExceeded):
			// 服务端预算耗尽。注意「底层计数并未被中断」——它仍在后台跑，见 searchIndexStatus
			// 的说明。错误码刻意不复用 search_timeout：那个表示「检索范围太大 / 该查索引」，
			// 而这里是运维诊断查询本身太重，处置方向不同（稍后重试或看日志里的回填进度）。
			auth.WriteError(c, http.StatusGatewayTimeout, "search_index_status_timeout",
				"索引状态查询超出预算（表较大时属正常），请稍后重试")
			return
		}
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
//
// 这是**破坏性管理动作**：Reset() 会清空整张 FTS 索引并把水位归零，重建期间搜索退回全表
// LIKE。同类入口都留痕（维护作业手动触发写 maintenance.job_run，空 Maven 制品清理明写
// 「唯一的批量物理删除入口，必须留痕」），这里同样在受理成功后写一条 search.index_rebuild，
// 否则「索引为什么被清空过」在事后无从追溯。
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
	// 只在**受理成功**后留痕：409（已有任务在跑）与 503（服务未启用）都没有真正触发重建，
	// 记一条「重建了」反而是假账。
	h.AuditLog(c, "search.index_rebuild", "search_index", "asset_path_fts", "",
		"全量重建已受理：清空索引并重置水位，随后后台分批重灌", "ok")
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
}
