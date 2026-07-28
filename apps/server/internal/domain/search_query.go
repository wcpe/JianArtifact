// 高级搜索表达式解析（FR 搜索优化）：
// 把用户输入的查询串解析为结构化条件，供 SearchAssets 下推 SQL。
//
// 语法（token 以空白分隔，双引号包裹的短语视为整体）：
//   词            —— 路径必须包含该子串（多词为 AND）
//   -词           —— 路径不得包含该子串（负筛选）
//   "含 空格 短语"  —— 引号内空白不切分，可与 - 组合
//   repo:名称      —— 限定仓库（多个为 OR）；-repo:名称 排除仓库
//   format:格式    —— 限定仓库格式 raw/maven/npm（多个为 OR）；-format: 排除
//   ext:扩展名     —— 路径以 .扩展名 结尾（多个为 OR）；-ext: 排除
package domain

import (
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// parsedSearch 高级搜索表达式解析结果。
type parsedSearch struct {
	filter     repository.SearchFilter // 路径子串 / 扩展名条件（下推存储层）
	repos      []string                // repo: 限定（OR）
	notRepos   []string                // -repo: 排除
	formats    []string                // format: 限定（OR，已归一化小写）
	notFormats []string                // -format: 排除
}

// hasRepoMeta 返回表达式是否包含需要仓库元数据参与的条件。
func (p parsedSearch) hasRepoMeta() bool {
	return len(p.repos) > 0 || len(p.notRepos) > 0 || len(p.formats) > 0 || len(p.notFormats) > 0
}

// tokenizeSearch 按空白切词；双引号包裹的片段内空白不切分，引号本身丢弃。
func tokenizeSearch(q string) []string {
	var tokens []string
	var b strings.Builder
	inQuote := false
	for _, ch := range q {
		switch {
		case ch == '"':
			inQuote = !inQuote
		case !inQuote && (ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'):
			if b.Len() > 0 {
				tokens = append(tokens, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(ch)
		}
	}
	if b.Len() > 0 {
		tokens = append(tokens, b.String())
	}
	return tokens
}

// parseSearchQuery 解析高级搜索表达式；空值 token（如孤立的 "repo:"）静默丢弃。
func parseSearchQuery(q string) parsedSearch {
	var p parsedSearch
	for _, tok := range tokenizeSearch(q) {
		neg := false
		if strings.HasPrefix(tok, "-") && len(tok) > 1 {
			neg = true
			tok = tok[1:]
		}
		lower := strings.ToLower(tok)
		switch {
		case strings.HasPrefix(lower, "repo:"):
			name := strings.TrimSpace(tok[len("repo:"):])
			if name == "" {
				continue
			}
			if neg {
				p.notRepos = append(p.notRepos, name)
			} else {
				p.repos = append(p.repos, name)
			}
		case strings.HasPrefix(lower, "format:"):
			f := strings.ToLower(strings.TrimSpace(tok[len("format:"):]))
			if f == "" {
				continue
			}
			if neg {
				p.notFormats = append(p.notFormats, f)
			} else {
				p.formats = append(p.formats, f)
			}
		case strings.HasPrefix(lower, "ext:"):
			e := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tok[len("ext:"):]), "."))
			if e == "" {
				continue
			}
			if neg {
				p.filter.ExcludeExts = append(p.filter.ExcludeExts, e)
			} else {
				p.filter.IncludeExts = append(p.filter.IncludeExts, e)
			}
		default:
			if neg {
				p.filter.ExcludeTerms = append(p.filter.ExcludeTerms, tok)
			} else {
				p.filter.IncludeTerms = append(p.filter.IncludeTerms, tok)
			}
		}
	}
	return p
}
