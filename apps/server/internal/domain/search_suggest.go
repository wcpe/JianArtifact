// 搜索建议（suggest）的领域实现。
//
// 规格：docs/specs/search-index-and-performance.md §3.8。本文件只做三件事——拼装候选、
// 套可读仓库范围、给出稳定序；它刻意**不新增**下面三样东西（规格 §3.7 的三处「必须共用」）：
//
//  1. 不新增包坐标解析规则：包名候选一律由 search_package.go 的 ParseCoordinates 导出
//     （的正式口径），本文件不写第二套「从 path 猜包名」的规则；
//  2. 不新增权限判定：可读仓库集合走 readableRepoIDs → RepoIDsWithPermission（批量 ACL）；
//  3. 不新增索引降级判定：候选集查询复用 buildSearchWhereIndexed，索引未就绪时自动退回
//     LIKE 路径，suggest 功能仍可用（只是没提速）。
//
// 另外两项刻意不做：服务端缓存（与「组内成员变动即时生效」的授权承诺冲突）与命中计数
// （每多一条候选就多一次聚合，违背「一次轻量查询」的目的）。
package domain

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"
)

// 候选类型（规格 §3.8 响应里的 type）。前端据此决定确认语义：
// package → 写入输入框并直接搜索；repository → 写成 repo: 限定、不立即搜索。
const (
	SuggestTypePackage    = "package"
	SuggestTypeRepository = "repository"
)

const (
	// suggestLimitDefault 是 limit 缺省值，suggestLimitMax 是上限（规格 §3.8：缺省 10，
	// 夹取到 [1, 20]）。夹取口径与 parseSearchPageSize 一致：非正数/非法值回落缺省、
	// 超出上限夹到上限，**一律不报错**——suggest 是高频被动调用，参数瑕疵不该变成错误。
	suggestLimitDefault = 10
	suggestLimitMax     = 20

	// maxSuggestScanRows 是单次 suggest 从候选集读取的路径行预算。
	//
	// 候选要去重后按稳定序截断，因此必须先有一个**确定性**的路径池：按 path 升序取前 N 行。
	// 取 500 的理由：一次请求最多只要 20 条候选，而同一包往往占据大量连续路径（一个版本的
	// jar/pom/sha1/… 连着几十行），500 行足以跨出同几个包；再往上加只会让每次键入多读一批
	// 路径，而候选早已填满。
	maxSuggestScanRows = 500
)

// SearchSuggestion 是搜索框的一条下拉候选。
type SearchSuggestion struct {
	Type  string // package | repository
	Value string // 候选值：仓库名，或可直接作为搜索词命中该包的包名
}

// clampSuggestLimit 把请求上限夹取到合法区间（见 suggestLimitDefault/Max 的注释）。
func clampSuggestLimit(limit int) int {
	if limit <= 0 {
		return suggestLimitDefault
	}
	if limit > suggestLimitMax {
		return suggestLimitMax
	}
	return limit
}

// Suggest 给出搜索框的下拉候选。
//
// keyword 是用户当前输入框的原文：**trim 后为空即返回空候选而不是报错**——suggest 是高频
// 被动调用，空输入是常态而非错误（规格 §3.8）。
//
// 候选由两类组成，仓库名优先占位（它们更短、且用户常用来做 repo: 限定），剩余名额给包名；
// 两类合计不超过 limit。排序只在**类内**进行（候选值长度升序 → 字典序升序），类间恒为
// 「仓库在前、包名在后」：两类各自排序、各自截断后直接拼接，因此合并结果**不是**全局有序的
// （例如仓库候选 `zzz-repo` 会排在包名候选 `abc` 之前）。这条顺序必须稳定——它不得依赖
// FTS 内部回表顺序，否则键盘选择的落点会随请求抖动。
//
// 权限是硬约束：非管理员只能看到「public ∪ 主体被授 read」的仓库下的候选；可读集合为空时
// 返回空候选而**不是 403**（403 会把「这里存在一个你看不到的仓库」作为信号泄漏出去）。
//
// ctx 与搜索主链路同源：入口派生一次预算（JIAN_SEARCH_TIMEOUT_SECONDS），**耗尽**时归一为
// ErrSearchTimeout；客户端断开不归一（原样返回），见 normalizeSearchErr。该预算只覆盖下发到
// SQL 的候选扫描，可读仓库集合的解析不在其中（见 searchContext）。
func (s *RepositoryService) Suggest(ctx context.Context, keyword string, subjectID int64, isAdmin bool, limit int) ([]SearchSuggestion, error) {
	limit = clampSuggestLimit(limit)
	out := make([]SearchSuggestion, 0, limit)
	q := strings.TrimSpace(keyword)
	if q == "" {
		return out, nil
	}
	// 超时预算：与搜索主链路共用同一套语义（规格 §3.7 衔接点 3）。
	ctx, cancel := s.searchContext(ctx)
	defer cancel()

	// 仓库全量列举只做一次：仓库名候选、包坐标解析所需的格式映射与 repo:/format: 收窄都复用它。
	allRepos, allErr := s.repos.List(1000, 0)
	repoIDs, readable, err := s.readableRepoIDs(subjectID, isAdmin, allRepos, allErr)
	if err != nil {
		return nil, err
	}
	if !readable {
		// 没有任何可读仓库：空候选，绝不 403（规格 §3.8 的泄漏论证）。
		return out, nil
	}

	// —— 第一类：仓库名候选，优先占位 ——
	var allowed map[int64]bool // nil 表示不限（管理员）
	if !isAdmin {
		allowed = make(map[int64]bool, len(repoIDs))
		for _, id := range repoIDs {
			allowed[id] = true
		}
	}
	repoCands := make([]SearchSuggestion, 0, limit)
	for _, r := range allRepos {
		if allowed != nil && !allowed[r.ID] {
			continue // 权限收窄：无 read 权限的仓库连名字都不出现
		}
		if !suggestNameMatches(r.Name, q) {
			continue
		}
		repoCands = append(repoCands, SearchSuggestion{Type: SuggestTypeRepository, Value: r.Name})
	}
	sortSuggestions(repoCands)
	if len(repoCands) > limit {
		repoCands = repoCands[:limit]
	}
	out = append(out, repoCands...)

	// —— 第二类：包名候选，占剩余名额 ——
	remain := limit - len(out)
	if remain == 0 {
		return out, nil
	}
	used := make(map[string]bool, len(out))
	for _, item := range out {
		// 跨类去重：某个包名恰好等于某个仓库名时，仓库候选先占位（它带 repo: 语义，更具体）。
		used[item.Value] = true
	}
	parsed := parseSearchQuery(q)
	// 没有正向包含词就没有候选驱动条件（规格 §3.3 的降级口径）：只输入 -sources / ext:jar /
	// repo:x 时不该把整个仓库的路径当候选池扫一遍，那样给出的都是无关包名。
	//
	// include-term 判定放在索引状态点查**之前**：suggest 是逐键触发的高频端点，而只输入
	// 过滤器时这里就直接返回了，不该为一次注定不发生的候选扫描先付一次索引状态查询。
	if len(parsed.filter.IncludeTerms) > 0 {
		// 索引是否就绪：与搜索同一个判定函数，每次现算不缓存（重建会把 completed_at 打回 NULL）。
		indexReady, err := s.assets.SearchIndexReady()
		if err != nil {
			return nil, err
		}
		parsed.filter.UseFullTextIndex = indexReady
		// repo:/format: 限定按仓库元数据收窄候选范围：与搜索共用同一个收窄函数（不另写判定）。
		// nil 在本函数里表示「不限」（管理员），而收窄函数的空结果同样是 nil，故另用布尔量
		// 区分「没匹配到任何仓库」（跳过资产查询）与「不限」。
		pkgRepoIDs := repoIDs
		skipPackages := false
		if parsed.hasRepoMeta() {
			// 非管理员的全量列举失败已在 readableRepoIDs 里上抛；这里兜住管理员
			// （它不走批量 ACL 分支），收窄仓库范围仍然需要 allRepos。
			if allErr != nil {
				return nil, allErr
			}
			var exprAllowed map[int64]bool
			if !isAdmin {
				exprAllowed = allowed
			}
			pkgRepoIDs = filterReposByExpr(allRepos, exprAllowed, "", parsed, true)
			skipPackages = len(pkgRepoIDs) == 0
		}
		if !skipPackages {
			rows, err := s.assets.SuggestPaths(ctx, parsed.filter, pkgRepoIDs, maxSuggestScanRows)
			if err != nil {
				return nil, normalizeSearchErr(ctx, err)
			}
			formats := make(map[int64]string, len(allRepos))
			for _, r := range allRepos {
				formats[r.ID] = r.Format
			}
			cands := make([]SearchSuggestion, 0, remain)
			seen := make(map[string]bool, remain)
			for _, row := range rows {
				// 包坐标解析的**唯一**来源：ParseCoordinates（同包 search_package.go）的解析函数。
				// 解析不出坐标的行（raw 等格式）不产出候选——降级条目的伪包维度就是仓库名，
				// 已由第一类候选覆盖。
				value := suggestPackageValue(ParseCoordinates(formats[row.RepositoryID], row.Path))
				if value == "" || used[value] || seen[value] {
					continue // 同上：去重不去掉「不同类型」的语义，但同一字符串只出现一次
				}
				seen[value] = true
				cands = append(cands, SearchSuggestion{Type: SuggestTypePackage, Value: value})
			}
			sortSuggestions(cands)
			if len(cands) > remain {
				cands = cands[:remain]
			}
			out = append(out, cands...)
		}
	}
	return out, nil
}

// suggestNameMatches 判断仓库名是否**包含**输入（子串匹配）。
//
// 规格 §3.8 原文写的是「前缀匹配」，但实际使用中仓库名多为 `maven-releases`、
// `npm-hosted` 这类「前缀是类型、后缀是用途」的命名：只做前缀会让用户输入 `rele`
// 找不到 `maven-releases`，与直觉不符。仓库数在百级，子串遍历的成本与前缀等价，
// 故取子串语义（对 `q` 为空的情况由上层提前返回，此处不重复处理）。
//
// 只折叠 ASCII 大小写：SQLite 的 LIKE 默认只对 ASCII 不区分大小写，而 repository.name 的排序
// 规则是 BINARY。这里对齐 LIKE 的口径而不是用 strings.ToLower / EqualFold 的全 Unicode 折叠，
// 否则「Go 侧判定比 SQL 更宽松」会在含非 ASCII 名字上出现两套行为。
func suggestNameMatches(name, q string) bool {
	return strings.Contains(asciiLower(name), asciiLower(q))
}

// suggestPackageValue 把包坐标转成**可直接作为搜索词命中该包**的候选值。
//
// Maven 的展示坐标是 `group:artifact`，但 asset.path 里存在的是 `group/artifact/...`——含 `:`
// 的坐标当关键词搜索会 0 命中，因此候选值取 artifact（`:` 之后的部分）。而 artifact 必然出现在
// 目录段与文件名里，作为包含词搜索一定能命中。「包名」对用户也正是这个粒度。
//
// npm 的坐标本身就是包名（`@scope/pkg` 或 `pkg`），在路径里原样出现，直接返回。
//
// 解析不出坐标（raw 等格式）返回空串，调用方据此跳过——这是「只承诺字符串候选、不承诺抽取
// 算法」的边界（规格 §3.8），值的形态可以随解析口径演进。
func suggestPackageValue(c Coordinates) string {
	if !c.HasPackage() {
		return ""
	}
	if c.Kind == PackageKindMaven {
		if i := strings.LastIndex(c.Coordinate, ":"); i >= 0 && i < len(c.Coordinate)-1 {
			return c.Coordinate[i+1:]
		}
	}
	return c.Coordinate
}

// sortSuggestions 按「候选值长度（rune 数）升序 → 字典序升序」排列（规格 §3.8 稳定序）。
//
// 长度按 rune 而不是字节：中文包名按字节算会排在明显更长的英文名之后，与「短候选优先」的
// 直觉相反。字典序用 Go 的字节序比较——对同一输入恒为同一顺序，稳定序的目的即达成。
func sortSuggestions(items []SearchSuggestion) {
	sort.Slice(items, func(i, j int) bool {
		li, lj := utf8.RuneCountInString(items[i].Value), utf8.RuneCountInString(items[j].Value)
		if li != lj {
			return li < lj
		}
		return items[i].Value < items[j].Value
	})
}
