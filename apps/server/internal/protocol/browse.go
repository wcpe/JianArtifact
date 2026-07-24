// Package protocol：目录浏览（autoindex）HTML 索引页。
//
// 当浏览器 GET /repository/:repo/<dir>/ （尾斜杠）且 Accept 声明 text/html 时，
// 渲染该目录的直接子项列表（子目录在前、文件在后），供人用浏览器浏览仓库内容。
// 鉴权复用 RawHandler.authorize(read)：public 匿名放行、private 需登录。
// 触发判定见 tryBrowse，渲染见 serveBrowse；不引入新依赖、不改 domain 接口。
package protocol

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// browseMaxEntries 限制单次目录浏览拉取的制品条数，防止超大目录拖慢响应。
const browseMaxEntries = 1000

// browseTmpl 是目录索引页模板；html/template 自动按上下文转义，防 XSS。
var browseTmpl = template.Must(template.New("browse").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;max-width:960px;margin:1.5rem auto;padding:0 1rem;color:#24292f}
h1{font-size:1rem;font-weight:600;margin:1.5rem 0 .75rem;word-break:break-all}
.crumbs a{color:#0969da;text-decoration:none}
.crumbs a:hover{text-decoration:underline}
.crumbs .sep{color:#57606a;margin:0 .25rem}
table{border-collapse:collapse;width:100%;font-size:.9rem}
th,td{text-align:left;padding:.4rem .6rem;border-bottom:1px solid #d0d7de}
th{color:#57606a;font-weight:600}
td.name a{color:#0969da;text-decoration:none;word-break:break-all}
td.name a:hover{text-decoration:underline}
tr.dir td.name a{font-weight:600}
td.size{color:#57606a;font-variant-numeric:tabular-nums;text-align:right;white-space:nowrap}
.empty{color:#57606a;padding:1rem 0}
.hint{color:#57606a;font-size:.8rem;margin-top:.5rem}
footer{margin-top:2rem;color:#57606a;font-size:.75rem}
</style>
</head>
<body>
<h1 class="crumbs">{{range $i, $c := .Crumbs}}{{if $i}}<span class="sep">/</span>{{end}}<a href="{{$c.Href}}">{{$c.Name}}</a>{{end}}</h1>
{{if .Rows}}
<table>
<thead><tr><th>名称</th><th>大小</th></tr></thead>
<tbody>
{{range .Rows}}
<tr class="{{if .IsDir}}dir{{end}}">
<td class="name"><a href="{{.Href}}">{{.Name}}</a></td>
<td class="size">{{if .IsDir}}-{{else}}{{.SizeStr}}{{end}}</td>
</tr>
{{end}}
</tbody>
</table>
{{if .Truncated}}<p class="hint">目录较大（共 {{.Total}} 项），仅显示前 {{.Shown}} 项。</p>{{end}}
{{else}}
<p class="empty">此目录为空。</p>
{{end}}
<footer>JianArtifact 制品仓库</footer>
</body>
</html>`))

// browseCrumb 是面包屑的一个层级（显示名 + 绝对链接）。
type browseCrumb struct {
	Name string
	Href string
}

// browseRow 是目录列表中的一行：子目录或文件。
type browseRow struct {
	Name    string
	Href    string
	IsDir   bool
	SizeStr string
}

// browsePage 是目录索引页的模板数据。
type browsePage struct {
	Title     string
	Crumbs    []browseCrumb
	Rows      []browseRow
	Total     int
	Shown     int
	Truncated bool
}

// tryBrowse 检测浏览器目录浏览请求（GET + 尾斜杠 + 期望 HTML 的客户端）。
// 命中则渲染 HTML 目录索引页并返回 true；否则返回 false 由调用方继续常规处理。
// 鉴权（authorize read）须由调用方在调用前完成，故 public/private 策略与下载一致。
func (h *RawHandler) tryBrowse(c *gin.Context) bool {
	if c.Request.Method != http.MethodGet {
		return false
	}
	if !strings.HasSuffix(c.Param("artifactPath"), "/") {
		return false
	}
	if !wantsBrowseHTML(c) {
		return false
	}
	h.serveBrowse(c)
	return true
}

// wantsBrowseHTML 判定客户端是否期望 HTML：Accept 含 text/html，
// 或 User-Agent 形似浏览器（含 Mozilla）兜底。
func wantsBrowseHTML(c *gin.Context) bool {
	if acceptsHTML(c.GetHeader("Accept")) {
		return true
	}
	return strings.Contains(c.GetHeader("User-Agent"), "Mozilla")
}

// acceptsHTML 判定 Accept 头是否明确接受 text/html（不含通配 */*，以免误伤 API 客户端）。
func acceptsHTML(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		media := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if media == "text/html" || media == "text/*" {
			return true
		}
	}
	return false
}

// serveBrowse 渲染当前目录的 HTML 索引页：按 artPath 前缀取制品，推导直接子项。
func (h *RawHandler) serveBrowse(c *gin.Context) {
	repoName := c.Param("repo")
	artPath := cleanArtifactPath(c.Param("artifactPath")) // 形如 "dir/" 或 ""（根）

	assets, total, err := h.repoSvc.ListAssets(repoName, artPath, browseMaxEntries, 0)
	if err != nil {
		writeAssetErr(c, err)
		return
	}

	page := browsePage{
		Title:     repoName + "/" + artPath,
		Crumbs:    buildBrowseCrumbs(repoName, artPath),
		Rows:      collectBrowseRows(c, artPath, assets),
		Total:     total,
		Shown:     len(assets),
		Truncated: total > browseMaxEntries,
	}
	var buf bytes.Buffer
	if err := browseTmpl.Execute(&buf, page); err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", buf.Bytes())
}

// buildBrowseCrumbs 构造面包屑：仓库根 → 各级目录，每段单独 URL 编码。
func buildBrowseCrumbs(repoName, artPath string) []browseCrumb {
	base := "/repository/" + url.PathEscape(repoName) + "/"
	crumbs := []browseCrumb{{Name: repoName, Href: base}}
	for _, seg := range splitNonEmpty(artPath, "/") {
		base += url.PathEscape(seg) + "/"
		crumbs = append(crumbs, browseCrumb{Name: seg, Href: base})
	}
	return crumbs
}

// collectBrowseRows 从前缀下的制品推导直接子目录与直接文件，生成有序列表行。
// 子项 href 以当前请求路径（编码形式）为基址，子项名逐段 URL 编码。
func collectBrowseRows(c *gin.Context, artPath string, assets []repository.Asset) []browseRow {
	base := c.Request.URL.EscapedPath()
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	// artPath 末尾含 "/"（根为 ""），作前缀去除得相对路径。
	prefix := artPath
	dirSeen := make(map[string]struct{})
	rows := make([]browseRow, 0, len(assets))
	for _, a := range assets {
		rel := strings.TrimPrefix(a.Path, prefix)
		if rel == "" {
			continue
		}
		if i := strings.Index(rel, "/"); i >= 0 {
			sub := rel[:i]
			if sub == "" {
				continue
			}
			if _, ok := dirSeen[sub]; ok {
				continue
			}
			dirSeen[sub] = struct{}{}
			rows = append(rows, browseRow{
				Name:  sub + "/",
				Href:  base + url.PathEscape(sub) + "/",
				IsDir: true,
			})
		} else {
			rows = append(rows, browseRow{
				Name:    rel,
				Href:    base + url.PathEscape(rel),
				SizeStr: formatSize(a.Size),
			})
		}
	}
	sortBrowseRows(rows)
	return rows
}

// sortBrowseRows 排序：目录在前、文件在后，各自按名称升序。
func sortBrowseRows(rows []browseRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].IsDir != rows[j].IsDir {
			return rows[i].IsDir
		}
		return rows[i].Name < rows[j].Name
	})
}

// formatSize 把字节数格式化为人类可读（如 1.2 KiB）。
func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for d := n / unit; d >= unit; d /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// splitNonEmpty 按 sep 分割并丢弃空段。
func splitNonEmpty(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
