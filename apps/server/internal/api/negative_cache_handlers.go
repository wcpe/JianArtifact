package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// 负缓存清理的审计动作。取值写进审计库后不可随意更名：
// 前端 auditAction 文案与按 action 的筛选都依赖这个字面量。
// 用维护类动作名（maintenance.*）而不是仓库维度的 repo.recheck：
// 清理动作可以是全域的，与单个仓库不是同一维度。
const auditActionMaintenanceNegativeCachePurge = "maintenance.negative_cache_purge"

// 负缓存查看端点的分页口径。上限同时是契约里 limit 的 maximum：
// 条目总数已被负缓存自身的 maxEntries 钉住，这里只是让单次响应体有界。
const (
	defaultNegativeCacheLimit = 100
	maxNegativeCacheLimit     = 500
)

// GetNegativeCache 返回 404 负缓存的有效条目快照（仅管理员）。
//
// 只读：不清理任何条目、不修改任何状态，因此不写审计（与作业清单查询一致）。
// 为什么先补容量上限再暴露本端点：查询要持负缓存锁遍历整张映射，无上限时会与
// 所有读请求抢锁；有上限后才保证遍历代价有界（见 domain.negativeCacheMaxEntries）。
func (h *Handlers) GetNegativeCache(c *gin.Context, params GetNegativeCacheParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.assets == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "负缓存未启用")
		return
	}
	filter := domain.NegativeCacheFilter{Limit: negativeCacheLimit(params.Limit)}
	// 仓库名写成过滤条件时先解析成 ID：仓库名不存在按 404 处理，
	// 而不是静默返回空列表——后者会让排障者以为「这条从没被缓存过」。
	if params.Repo != nil && *params.Repo != "" {
		repo, err := h.repos.Get(*params.Repo)
		if err != nil {
			writeDomainErr(c, err)
			return
		}
		filter.RepositoryID = repo.ID
	}
	if params.PathPrefix != nil {
		filter.PathPrefix = *params.PathPrefix
	}
	snap, err := h.assets.NegativeCacheSnapshot(filter)
	if err != nil {
		// 不外显内部细节（SQL 文本等）：补名失败对调用方只有「取不到」这一个含义。
		auth.WriteError(c, http.StatusInternalServerError, "internal_error", "读取负缓存失败")
		return
	}
	c.JSON(http.StatusOK, toAPINegativeCacheSummary(snap))
}

// PurgeNegativeCache 清理 404 负缓存（仅管理员）。
//
// 语义：无参数全清；仅 repo 按仓库清；repo + path 精确清一条。
// 破坏性动作（清掉的是运行态记忆），写审计；查看端点不写。
func (h *Handlers) PurgeNegativeCache(c *gin.Context, params PurgeNegativeCacheParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.assets == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "负缓存未启用")
		return
	}
	filter := domain.NegativeCacheFilter{}
	repoName := ""
	if params.Repo != nil && *params.Repo != "" {
		repo, err := h.repos.Get(*params.Repo)
		if err != nil {
			writeDomainErr(c, err)
			return
		}
		repoName = repo.Name
		filter.RepositoryID = repo.ID
	}
	if params.Path != nil && *params.Path != "" {
		// path 单独给出没有语义（精确清必须限定在某个仓库里）：明确 400 而不是
		// 悄悄退化成全清——那会让一次参数写错的调用清掉整张表。
		if filter.RepositoryID == 0 {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "指定 path 时必须同时指定 repo")
			return
		}
		filter.ExactPath = *params.Path
	}
	removed := h.assets.PurgeNegativeCache(filter)
	h.AuditLog(c, auditActionMaintenanceNegativeCachePurge, "maintenance", "negative_cache", repoName,
		negativeCachePurgeDetail(repoName, filter.ExactPath, removed), "ok")
	c.JSON(http.StatusOK, NegativeCachePurgeResult{Removed: int64(removed)})
}

// negativeCacheLimit 归一化 limit：缺省 100，超上限截断到 500（不报错，契约里已声明
// 「超出上限由服务端截断」），非正数回落默认值。
func negativeCacheLimit(limit *NegativeCacheLimitParam) int {
	if limit == nil {
		return defaultNegativeCacheLimit
	}
	value := int(*limit)
	if value <= 0 {
		return defaultNegativeCacheLimit
	}
	if value > maxNegativeCacheLimit {
		return maxNegativeCacheLimit
	}
	return value
}

// negativeCachePurgeDetail 生成审计详情。路径经同一套控制字符转义：审计详情同样是
// 单行文本，未转义的换行会伪造出一条审计记录。
func negativeCachePurgeDetail(repoName, path string, removed int) string {
	var scope string
	switch {
	case path != "":
		scope = "范围=精确仓库 repo=" + repoName + " path=" + domain.SanitizeDiagnosticPath(path)
	case repoName != "":
		scope = "范围=按仓库 repo=" + repoName
	default:
		scope = "范围=全清"
	}
	return scope + " 删除=" + strconv.Itoa(removed)
}

// toAPINegativeCacheSummary 把领域快照映射为契约类型。
//
// 时间统一按 UTC 输出；路径走与诊断日志相同的转义助手（控制字符转义 + 截断 512），
// 保证响应与日志对同一路径的呈现一致。
func toAPINegativeCacheSummary(snap domain.NegativeCacheSnapshot) NegativeCacheSummary {
	items := make([]NegativeCacheItem, 0, len(snap.Items))
	for _, it := range snap.Items {
		remaining := int64(it.Remaining / time.Second)
		if remaining < 0 {
			remaining = 0
		}
		item := NegativeCacheItem{
			RepositoryId:     it.RepositoryID,
			Path:             domain.SanitizeDiagnosticPath(it.Path),
			WrittenAt:        it.WrittenAt.UTC(),
			RemainingSeconds: remaining,
		}
		if it.Repository != "" {
			name := it.Repository
			item.Repository = &name
		}
		items = append(items, item)
	}
	return NegativeCacheSummary{
		Total:      int64(snap.Total),
		TtlSeconds: int64(snap.TTL / time.Second),
		MaxEntries: int64(snap.MaxEntries),
		Items:      items,
	}
}
