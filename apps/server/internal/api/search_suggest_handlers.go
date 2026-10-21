package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

// FR-153：搜索建议（suggest）端点。
//
// 与 /api/v1/search 同组同先例，作为**非契约端点**注册（不进 api/openapi.yaml）：它只服务
// 前端搜索框，候选形状会随包坐标解析口径演进，且不是任何协议客户端依赖的稳定面。把它写进
// 契约就等于为「高度易变的内部形状」引入对外承诺，代价高于收益（理由同
// search_index_handlers.go 文件头）；若将来要求入契约，必须连同生成物与 devmock 一并交付。

// SuggestSearch 返回搜索框的下拉候选。
//
// 参数口径（规格 §3.8）：
//   - q     —— 必填语义，但 trim 后为空时返回空 items 而**不是 400**：suggest 是高频被动
//     调用，空输入（清空输入框、首字符未到）是常态而非错误；
//   - limit —— 缺省 10，夹取到 [1, 20]；非正数/非法值回落缺省、超上限夹到上限，一律不报错
//     （对齐 parseSearchPageSize 的既有修正口径）。夹取本身在领域层完成，handler 只负责
//     把「缺省」与「解析失败」都表达成 0。
//
// 响应：{"items": [{"type": "package"|"repository", "value": "..."}]}。不带命中计数——为每条
// 候选再算一次 COUNT 会把「一次轻量查询」变回多次聚合，与 suggest 的目的相悖。
//
// 权限：可读范围由领域层统一判定（管理员搜全部，其余 public ∪ 被授 read）。可读集合为空时
// 返回 **200 + 空 items**，绝不 403——403 会把「这里存在一个你看不到的仓库」泄漏出去。
func (h *Handlers) SuggestSearch(c *gin.Context) {
	// limit：缺省与解析失败都传 0，由领域层回落默认值并夹取。
	limit := 0
	if v := c.Query("limit"); v != "" {
		if n, err := parseIntQuery(v); err == nil {
			limit = n
		}
	}

	// 解析主体（可能匿名）；匿名受全局开关约束（FR-66），关闭时与搜索一致地返回 401。
	p, authed := auth.PrincipalFrom(c)
	if !authed && !h.anonymousAllowed(c) {
		return
	}
	var subjectID int64
	var isAdmin bool
	if authed {
		subjectID = p.UserID
		isAdmin = p.IsAdmin()
	}

	// 超时预算由领域层统一施加（与 /api/v1/search 同一条链路）：这里只把请求上下文（含客户端
	// 断开即取消）传下去，不再额外套一层 deadline。
	items, err := h.repos.Suggest(c.Request.Context(), c.Query("q"), subjectID, isAdmin, limit)
	if err != nil {
		writeDomainErr(c, err)
		return
	}

	type suggestItem struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	out := make([]suggestItem, 0, len(items))
	for _, item := range items {
		out = append(out, suggestItem{Type: item.Type, Value: item.Value})
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}
