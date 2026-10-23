package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件实现 FR-159 的两个只读查询端点：上游健康历史的时间序列 + 汇总，以及最近采样明细。
//
// 三个共同前提（规格 §2.3）：
//   - **仅管理员**：响应含失败原因分布与上游判定细节，不宜匿名暴露；
//   - **非 proxy 仓库返回 400**：hosted 没有上游、group 没有独立上游，「上游健康」对它们
//     不存在——用 200 + 空序列会让调用方以为「只是还没采样」；
//   - **未接线返回 503**：采样仓储未装配与「窗口内没有采样」是两件事，
//     混成同一响应会让运维排查方向完全错误。

// upstreamHealthMaxRange 是查询窗口的上限：与保留期一致（30 天）。
// 超过保留期的窗口必然一半是空的，限制它既能挡住误传的超大窗口，
// 也让「时间范围无效」这条 400 的语义明确。
const upstreamHealthMaxRange = repository.UpstreamHealthRetention

// upstreamHealthDefaultRange 是未传 from/to 时的默认窗口（最近 24 小时）。
const upstreamHealthDefaultRange = 24 * time.Hour

// upstreamHealthMaxRecentLimit 是「最近明细」的单次上限；
// 与契约里的 maximum: 500 一致，避免调用方一次拉走整月数据。
const upstreamHealthMaxRecentLimit = 500

// requireUpstreamHealthRepo 做两个端点共用的前置校验：管理员、仓储已接线、仓库存在且为 proxy。
// 返回 false 表示已写出错误响应，调用方应直接返回。
func (h *Handlers) requireUpstreamHealthRepo(c *gin.Context, name RepoNameParam) (*repository.Repository, bool) {
	if _, ok := requireAdmin(c); !ok {
		return nil, false
	}
	if h.upstreamHealth == nil {
		// 503 而非 200 空序列：见文件头「未接线返回 503」。
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "上游健康历史存储未接线")
		return nil, false
	}
	repo, err := h.repos.Get(name)
	if err != nil {
		writeDomainErr(c, err)
		return nil, false
	}
	if repo.Type != "proxy" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "仅 proxy 仓库有上游健康历史")
		return nil, false
	}
	return repo, true
}

// GetRepositoryUpstreamHealth 返回窗口内的上游健康时间序列与按来源分组的汇总。
func (h *Handlers) GetRepositoryUpstreamHealth(c *gin.Context, name RepoNameParam, params GetRepositoryUpstreamHealthParams) {
	repo, ok := h.requireUpstreamHealthRepo(c, name)
	if !ok {
		return
	}
	from, to, ok := upstreamHealthRange(c, params.From, params.To)
	if !ok {
		return
	}
	source, ok := upstreamHealthSource(c, params.Source)
	if !ok {
		return
	}
	buckets, err := h.upstreamHealth.UpstreamHealthBuckets(repo.ID, from, to, source)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	points := make([]UpstreamHealthPoint, 0, len(buckets))
	for _, bucket := range buckets {
		points = append(points, toAPIUpstreamHealthPoint(bucket))
	}
	fromValue, toValue := from.UTC(), to.UTC()
	c.JSON(http.StatusOK, UpstreamHealthSeries{
		Repository: repo.Name,
		From:       &fromValue,
		To:         &toValue,
		Points:     points,
		Summaries:  upstreamHealthSummaries(buckets),
	})
}

// GetRepositoryUpstreamHealthRecent 返回最近 N 个分钟桶的明细摘要。
//
// 明细口径是**分钟桶聚合**而非逐请求日志（规格 §6 R3）：同一分钟内的多次请求
// 只会看到一行。这一点必须由契约描述与前端文案共同交代清楚，否则会被误读成丢数据。
func (h *Handlers) GetRepositoryUpstreamHealthRecent(c *gin.Context, name RepoNameParam, params GetRepositoryUpstreamHealthRecentParams) {
	repo, ok := h.requireUpstreamHealthRepo(c, name)
	if !ok {
		return
	}
	limit := 50
	if params.Limit != nil {
		limit = int(*params.Limit)
	}
	if limit <= 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "limit 必须为正整数")
		return
	}
	if limit > upstreamHealthMaxRecentLimit {
		// 夹取而不是拒绝：上限是防滥用护栏，不是调用方需要精确命中的协议细节。
		limit = upstreamHealthMaxRecentLimit
	}
	buckets, err := h.upstreamHealth.UpstreamHealthRecent(repo.ID, limit)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]UpstreamHealthPoint, 0, len(buckets))
	for _, bucket := range buckets {
		items = append(items, toAPIUpstreamHealthPoint(bucket))
	}
	c.JSON(http.StatusOK, UpstreamHealthRecent{Repository: repo.Name, Items: items})
}

// upstreamHealthRange 解析并校验查询窗口，规则与 operationsRange 同一形态：
// 缺省为最近 24 小时；from/to 必须同时提供；窗口必须为正且不超过保留期。
func upstreamHealthRange(c *gin.Context, fromValue *UpstreamHealthFromParam, toValue *UpstreamHealthToParam) (time.Time, time.Time, bool) {
	now := time.Now().UTC()
	from, to := now.Add(-upstreamHealthDefaultRange), now
	if (fromValue == nil) != (toValue == nil) {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "from 与 to 必须同时提供")
		return time.Time{}, time.Time{}, false
	}
	if fromValue != nil {
		from, to = fromValue.UTC(), toValue.UTC()
	}
	if !to.After(from) || to.Sub(from) > upstreamHealthMaxRange {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "时间范围无效或超过 30 天")
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// upstreamHealthSource 校验 source 过滤参数：省略表示两路都取。
func upstreamHealthSource(c *gin.Context, value *GetRepositoryUpstreamHealthParamsSource) (string, bool) {
	if value == nil {
		return "", true
	}
	switch string(*value) {
	case repository.UpstreamHealthSourceRequest:
		return repository.UpstreamHealthSourceRequest, true
	case repository.UpstreamHealthSourceProbe:
		return repository.UpstreamHealthSourceProbe, true
	default:
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "source 只能是 request 或 probe")
		return "", false
	}
}

// toAPIUpstreamHealthPoint 把行模型转为契约采样点。
// 延迟两列直接透传指针：无成功样本时保持 nil，序列化为 null（不伪造 0）。
func toAPIUpstreamHealthPoint(bucket repository.UpstreamHealthBucket) UpstreamHealthPoint {
	return UpstreamHealthPoint{
		BucketStart:  bucket.BucketStart,
		Source:       UpstreamHealthSource(bucket.Source),
		Samples:      bucket.Samples,
		Ok:           bucket.OK,
		Timeout:      bucket.Timeout,
		Unreachable:  bucket.Unreachable,
		Slow:         bucket.Slow,
		Aborted:      bucket.Aborted,
		AvgLatencyMs: bucket.AvgLatencyMs,
		MaxLatencyMs: bucket.MaxLatencyMs,
	}
}

// upstreamHealthSummaries 按来源分组汇总。
//
// 只输出窗口内**实际出现过**的来源：request 与 probe 的可用性含义不同，
// 各自算自己的连通率，绝不合并平均——这正是「探测 100% 成功但真实请求 60%」
// 能被看见的前提（规格 §2.4）。缺失某个来源的键表示该来源在窗口内没有采样。
func upstreamHealthSummaries(buckets []repository.UpstreamHealthBucket) map[string]UpstreamHealthSummary {
	grouped := map[string][]repository.UpstreamHealthBucket{}
	for _, bucket := range buckets {
		grouped[bucket.Source] = append(grouped[bucket.Source], bucket)
	}
	out := make(map[string]UpstreamHealthSummary, len(grouped))
	for source, items := range grouped {
		out[source] = upstreamHealthSummary(items)
	}
	return out
}

// upstreamHealthSummary 汇总一组分钟桶。
//
// 平均延迟按**成功样本数加权**（每个桶的 ok 就是它的权重），p95 也按同一权重做分位估算：
// 底表只保留了桶内平均与最大值，逐请求的精确分位数已经不可恢复，
// 因此这里给的是「按桶平均延迟加权的估算值」，契约里已写清它不是 SLA 口径的精确值。
func upstreamHealthSummary(buckets []repository.UpstreamHealthBucket) UpstreamHealthSummary {
	summary := UpstreamHealthSummary{}
	var latencyWeighted int64 // Σ(桶平均 × 桶内成功数)
	var latencyWeight int64   // Σ桶内成功数
	weighted := make([]struct {
		avg    int64
		weight int64
	}, 0, len(buckets))
	for _, bucket := range buckets {
		summary.Samples += bucket.Samples
		summary.Ok += bucket.OK
		summary.Timeout += bucket.Timeout
		summary.Unreachable += bucket.Unreachable
		summary.Slow += bucket.Slow
		summary.Aborted += bucket.Aborted
		if bucket.AvgLatencyMs != nil && bucket.OK > 0 {
			latencyWeighted += *bucket.AvgLatencyMs * bucket.OK
			latencyWeight += bucket.OK
			weighted = append(weighted, struct {
				avg    int64
				weight int64
			}{avg: *bucket.AvgLatencyMs, weight: bucket.OK})
		}
	}
	failed := summary.Timeout + summary.Unreachable + summary.Slow + summary.Aborted
	summary.Failed = &failed
	// samples=0（窗口内全是 local_write_failure / canceled，或干脆没有采样）时
	// connectivity 必须保持 nil：此处除以 0 会得到 NaN，而把 nil 写成 0 或 1
	// 都会让前端把「没数据」显示成「全坏」或「全好」。
	if summary.Samples > 0 {
		connectivity := float64(summary.Ok) / float64(summary.Samples)
		summary.Connectivity = &connectivity
	}
	if latencyWeight > 0 {
		avg := latencyWeighted / latencyWeight
		summary.AvgLatencyMs = &avg
		p95 := weightedP95(weighted, latencyWeight)
		summary.P95LatencyMs = &p95
	}
	return summary
}

// weightedP95 按权重估算 p95。
//
// 做法是把桶按平均延迟升序排，按成功数累加权重，取第一个累计权重达到 95% 的桶的平均延迟。
// 它对「大部分请求很快、少数分钟很慢」这一形态是可靠的；单桶时直接返回该桶平均值。
func weightedP95(weighted []struct {
	avg    int64
	weight int64
}, total int64) int64 {
	if len(weighted) == 0 || total <= 0 {
		return 0
	}
	sort.SliceStable(weighted, func(i, j int) bool { return weighted[i].avg < weighted[j].avg })
	threshold := total * 95 // 用整数比较避免 0.95 的浮点误差：累计权重 ×100 与 total×95 比较
	var cumulative int64
	for _, item := range weighted {
		cumulative += item.weight * 100
		if cumulative >= threshold {
			return item.avg
		}
	}
	return weighted[len(weighted)-1].avg
}
