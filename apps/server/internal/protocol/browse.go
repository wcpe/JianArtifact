// Package protocol：目录浏览（autoindex）HTML 索引页。
//
// 当浏览器 GET /repository/:repo/<dir>/ （尾斜杠）且 Accept 声明 text/html 时，
// 渲染该目录的直接子项列表（子目录在前、文件在后），供人用浏览器浏览仓库内容。
// 鉴权复用 RawHandler.authorize(read)：public 匿名放行、private 需登录。
// 触发判定见 tryBrowse，渲染见 serveBrowse；不引入新依赖、不改 domain 接口。
//
// 规模口径：子目录**全量**展示，只有直接文件分页。目录项规模只与「同层目录数」相关，
// 而对取数做条数截断会让同层的其它子目录整片消失（列表既不完整也不正确），因此分页
// 只作用于直接文件；单次响应规模与子树规模解耦。
//
// 时区口径：服务端只认 UTC（asset 表存的就是 UTC naive 串），而访问者的浏览器时区只有
// 前端拿得到。因此本页是**唯一**一处刻意的零依赖例外——内联一段十几行的原生脚本，把
// <time datetime> 的机器可读值改写为访问者本地时区；脚本失效或禁用时页面回退为服务端
// 渲染的 UTC 文案，信息不丢失。除这段脚本外仍不引入任何依赖、不使用外链资源。
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
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// browseFilePageSize 是目录索引页默认每页展示的直接文件行数。
const browseFilePageSize = 500

// browseFilePageSizeMin / browseFilePageSizeMax 是 ?per_page= 的允许区间：
// 给「一屏想多看些」留出口，同时封住单个请求渲染上万行的风险。
const (
	browseFilePageSizeMin = 50
	browseFilePageSizeMax = 5000
)

// browseMaxPage 是 ?page= 的解析护栏（真实页数另按文件总数收敛），
// 用于避免 (page-1)*pageSize 在 32 位上溢出。
const browseMaxPage = 100000

// browseFilePageSizes 是页脚提供的每页条数切换项。
var browseFilePageSizes = []int{500, 2000, 5000}

// browseTmpl 是目录索引页模板；html/template 自动按上下文转义，防 XSS。
var browseTmpl = template.Must(template.New("browse").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;max-width:1180px;margin:1.5rem auto;padding:0 1rem;color:#24292f}
h1{font-size:1rem;font-weight:600;margin:1.5rem 0 .75rem;word-break:break-all}
.crumbs a{color:#0969da;text-decoration:none}
.crumbs a:hover{text-decoration:underline}
.crumbs .sep{color:#57606a;margin:0 .25rem}
.tablewrap{overflow-x:auto}
table{border-collapse:collapse;width:100%;min-width:780px;font-size:.9rem}
th,td{text-align:left;padding:.4rem .6rem;border-bottom:1px solid #d0d7de;vertical-align:top}
th{color:#57606a;font-weight:600;white-space:nowrap}
td.name a{color:#0969da;text-decoration:none;word-break:break-all}
td.name a:hover{text-decoration:underline}
tr.dir td.name a{font-weight:600}
td.size{color:#57606a;font-variant-numeric:tabular-nums;text-align:right;white-space:nowrap}
td.time{color:#57606a;font-variant-numeric:tabular-nums;white-space:nowrap;font-size:.82rem}
td.sum{font-size:.78rem;color:#57606a;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
td.sum details summary{cursor:pointer;white-space:nowrap}
td.sum dl{margin:.35rem 0 0;font-size:.72rem}
td.sum dt{color:#57606a}
td.sum dd{margin:.1rem 0 .35rem;color:#24292f;word-break:break-all}
.empty{color:#57606a;padding:1rem 0}
.summary{display:flex;flex-wrap:wrap;gap:.25rem 1rem;color:#57606a;font-size:.8rem;margin-top:.75rem}
.pager{display:flex;flex-wrap:wrap;gap:.5rem 1rem;align-items:baseline;font-size:.85rem;margin-top:.75rem}
.pager a{color:#0969da;text-decoration:none}
.pager a:hover{text-decoration:underline}
.pager .current{color:#57606a}
.sizes{display:flex;flex-wrap:wrap;gap:.25rem .5rem;align-items:baseline;color:#57606a;font-size:.8rem;margin-top:.5rem}
.sizes a{color:#0969da;text-decoration:none}
.sizes a:hover{text-decoration:underline}
.sizes .on{color:#24292f;font-weight:600}
footer{margin-top:2rem;color:#57606a;font-size:.75rem}
</style>
</head>
<body>
<h1 class="crumbs">{{range $i, $c := .Crumbs}}{{if $i}}<span class="sep">/</span>{{end}}<a href="{{$c.Href}}">{{$c.Name}}</a>{{end}}</h1>
{{if .Rows}}
<div class="tablewrap">
<table>
<thead><tr><th>名称</th><th>大小</th><th data-local="创建时间（本地时区）">创建时间 (UTC)</th><th data-local="修改时间（本地时区）">修改时间 (UTC)</th><th>校验和</th></tr></thead>
<tbody>
{{range .Rows}}
<tr class="{{if .IsDir}}dir{{end}}">
<td class="name"><a href="{{.Href}}">{{.Name}}</a></td>
<td class="size"{{if .SizeTitle}} title="{{.SizeTitle}}"{{end}}>{{.SizeStr}}</td>
<td class="time">{{if .CreatedISO}}<time datetime="{{.CreatedISO}}">{{.CreatedStr}}</time>{{else}}{{.CreatedStr}}{{end}}</td>
<td class="time"{{if .UpdatedTitle}} title="{{.UpdatedTitle}}"{{end}}>{{if .UpdatedISO}}<time datetime="{{.UpdatedISO}}">{{.UpdatedStr}}</time>{{else}}{{.UpdatedStr}}{{end}}</td>
<td class="sum">{{if .HasChecksum}}<details><summary title="{{.Sha256}}">{{.Sha256Short}}</summary><dl><dt>SHA-256</dt><dd>{{.Sha256}}</dd><dt>SHA-1</dt><dd>{{.Sha1}}</dd><dt>MD5</dt><dd>{{.Md5}}</dd></dl></details>{{else}}-{{end}}</td>
</tr>
{{end}}
</tbody>
</table>
</div>
<div class="summary">
<span>目录 {{.DirCount}} 个</span>
<span>文件 {{.FileTotal}} 项</span>
<span>合计 {{.FileBytesStr}}</span>
{{if .Paged}}<span>当前显示第 {{.Pager.From}}–{{.Pager.To}} 项</span>{{end}}
</div>
{{if gt .Pager.PageCount 1}}
<nav class="pager">
<a href="{{.Pager.FirstHref}}">首页</a>
{{if .Pager.HasPrev}}<a href="{{.Pager.PrevHref}}">上一页</a>{{end}}
<span class="current">第 {{.Pager.Page}} / {{.Pager.PageCount}} 页</span>
{{if .Pager.HasNext}}<a href="{{.Pager.NextHref}}">下一页</a>{{end}}
<a href="{{.Pager.LastHref}}">末页</a>
</nav>
{{end}}
<p class="sizes">每页：{{range $i, $s := .Pager.SizeLinks}}{{if $i}}·{{end}}{{if $s.Active}}<span class="on">{{$s.Size}}</span>{{else}}<a href="{{$s.Href}}">{{$s.Size}}</a>{{end}}{{end}}</p>
{{else}}
<p class="empty">此目录为空。</p>
{{end}}
<footer>JianArtifact 制品仓库</footer>
<script>
// 时区渐进增强：服务端只认 UTC（asset 表存的就是 UTC naive 串），浏览器时区只有前端拿得到。
// 因此保留服务端渲染的 UTC 文案作为**无脚本回退**，再由本脚本按 <time datetime> 的机器可读值
// 改写为访问者本地时区；表头文案同样由 data-local 提供，避免两套文案分叉。脚本内联、无外链、无 eval。
(function () {
  var pad = function (n) { return String(n).length < 2 ? "0" + n : String(n); };
  var nodes = document.querySelectorAll("time[datetime]");
  for (var i = 0; i < nodes.length; i++) {
    var at = new Date(nodes[i].getAttribute("datetime"));
    if (isNaN(at.getTime())) continue;
    nodes[i].textContent =
      at.getFullYear() + "-" + pad(at.getMonth() + 1) + "-" + pad(at.getDate()) + " " +
      pad(at.getHours()) + ":" + pad(at.getMinutes()) + ":" + pad(at.getSeconds());
  }
  var heads = document.querySelectorAll("[data-local]");
  for (var j = 0; j < heads.length; j++) {
    heads[j].textContent = heads[j].getAttribute("data-local");
  }
})();
</script>
</body>
</html>`))

// browseCrumb 是面包屑的一个层级（显示名 + 绝对链接）。
type browseCrumb struct {
	Name string
	Href string
}

// browseRow 是目录列表中的一行：子目录或文件。
// 时间字段是 asset 表存的 UTC "YYYY-MM-DD HH:MM:SS"，原样展示并在表头标注 UTC；
// ISO 字段是同一时刻的 RFC3339（带 Z），供 <time datetime> 承载机器可读值，
// 便于前端脚本按浏览器时区改写展示文案而不丢失原始口径。
// 目录行的「大小」是子树内制品总数、「修改时间」是子树内最近一次更新时间（目录没有
// 自身的创建/修改时间），故用 title 说明该差异。
type browseRow struct {
	Name         string
	Href         string
	IsDir        bool
	SizeStr      string
	SizeTitle    string
	CreatedStr   string
	CreatedISO   string
	UpdatedStr   string
	UpdatedISO   string
	UpdatedTitle string
	Sha256       string
	Sha256Short  string
	Sha1         string
	Md5          string
	HasChecksum  bool
}

// browseSizeLink 是页脚「每页条数」的一个切换项。
type browseSizeLink struct {
	Size   int
	Href   string
	Active bool
}

// browsePager 是直接文件的分页状态（子目录不参与分页）。
type browsePager struct {
	Page      int
	PageCount int
	From      int // 本页首条序号（1-based；本页无文件时为 0）
	To        int // 本页末条序号
	FirstHref string
	PrevHref  string
	NextHref  string
	LastHref  string
	HasPrev   bool
	HasNext   bool
	SizeLinks []browseSizeLink
}

// browsePage 是目录索引页的模板数据。
type browsePage struct {
	Title        string
	Crumbs       []browseCrumb
	Rows         []browseRow
	DirCount     int
	FileTotal    int
	FileBytesStr string
	Paged        bool
	Pager        browsePager
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

// serveBrowse 渲染当前目录的 HTML 索引页：子目录全量、直接文件分页。
func (h *RawHandler) serveBrowse(c *gin.Context) {
	repoName := c.Param("repo")
	artPath := cleanArtifactPath(c.Param("artifactPath")) // 形如 "dir/" 或 ""（根）

	pageSize := clampAtoi(c.Query("per_page"), browseFilePageSize, browseFilePageSizeMin, browseFilePageSizeMax)
	page := clampAtoi(c.Query("page"), 1, 1, browseMaxPage)

	children, err := h.repoSvc.ListDirectoryPage(repoName, artPath, pageSize, (page-1)*pageSize)
	if err != nil {
		writeAssetErr(c, err)
		return
	}
	// 已知直接文件总数后再收敛页码，避免落在末页之后显示空页。
	pageCount := (children.FileTotal + pageSize - 1) / pageSize
	if pageCount < 1 {
		pageCount = 1
	}
	if page > pageCount {
		page = pageCount
		if children, err = h.repoSvc.ListDirectoryPage(repoName, artPath, pageSize, (page-1)*pageSize); err != nil {
			writeAssetErr(c, err)
			return
		}
	}

	pager := browsePager{
		Page:      page,
		PageCount: pageCount,
		FirstHref: browseHref(c, 1, pageSize),
		PrevHref:  browseHref(c, page-1, pageSize),
		NextHref:  browseHref(c, page+1, pageSize),
		LastHref:  browseHref(c, pageCount, pageSize),
		HasPrev:   page > 1,
		HasNext:   page < pageCount,
		SizeLinks: buildBrowseSizeLinks(c, pageSize),
	}
	if n := len(children.Files); n > 0 {
		pager.From = (page-1)*pageSize + 1
		pager.To = pager.From + n - 1
	}

	data := browsePage{
		Title:        repoName + "/" + artPath,
		Crumbs:       buildBrowseCrumbs(repoName, artPath),
		Rows:         collectBrowseRows(c, children.Dirs, children.DirStats, children.Files),
		DirCount:     len(children.Dirs),
		FileTotal:    children.FileTotal,
		FileBytesStr: formatSize(children.FileBytes),
		Paged:        pageCount > 1,
		Pager:        pager,
	}
	var buf bytes.Buffer
	if err := browseTmpl.Execute(&buf, data); err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", buf.Bytes())
}

// browseHref 构造同目录下的翻页链接：保留当前请求的既有查询参数，只覆盖 page 与 per_page。
func browseHref(c *gin.Context, page, pageSize int) string {
	q := c.Request.URL.Query()
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", strconv.Itoa(pageSize))
	return c.Request.URL.EscapedPath() + "?" + q.Encode()
}

// buildBrowseSizeLinks 生成「每页条数」切换链接；切换后回到第 1 页。
func buildBrowseSizeLinks(c *gin.Context, current int) []browseSizeLink {
	links := make([]browseSizeLink, 0, len(browseFilePageSizes))
	for _, size := range browseFilePageSizes {
		links = append(links, browseSizeLink{
			Size:   size,
			Href:   browseHref(c, 1, size),
			Active: size == current,
		})
	}
	return links
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

// collectBrowseRows 把当前层级的子目录与直接文件整理成表格行。
// dirs 为自仓库根起算的完整路径（取末段作显示名），dirStats 与 dirs 同序给出子树计数与
// 最近更新时间；files 已是当前层的直接文件。
// 子项 href 以当前请求路径（编码形式）为基址，子项名单独 URL 编码。
func collectBrowseRows(c *gin.Context, dirs []string, dirStats []repository.DirStat, files []repository.Asset) []browseRow {
	base := c.Request.URL.EscapedPath()
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	stats := make(map[string]repository.DirStat, len(dirStats))
	for _, st := range dirStats {
		stats[st.Path] = st
	}
	rows := make([]browseRow, 0, len(dirs)+len(files))
	for _, dir := range dirs {
		name := lastSegment(dir)
		if name == "" {
			continue
		}
		st := stats[dir]
		rows = append(rows, browseRow{
			Name:         name + "/",
			Href:         base + url.PathEscape(name) + "/",
			IsDir:        true,
			SizeStr:      fmt.Sprintf("%d 项", st.Count),
			SizeTitle:    "目录内制品总数（含子目录）",
			CreatedStr:   "-", // 目录没有自身的创建时间
			UpdatedStr:   orDash(st.Latest),
			UpdatedISO:   toISOUTC(st.Latest),
			UpdatedTitle: "目录内最近一次更新时间",
		})
	}
	for _, f := range files {
		name := lastSegment(f.Path)
		if name == "" {
			continue
		}
		rows = append(rows, browseRow{
			Name:        name,
			Href:        base + url.PathEscape(name),
			SizeStr:     formatSize(f.Size),
			CreatedStr:  orDash(f.CreatedAt),
			CreatedISO:  toISOUTC(f.CreatedAt),
			UpdatedStr:  orDash(f.UpdatedAt),
			UpdatedISO:  toISOUTC(f.UpdatedAt),
			Sha256:      f.BlobHash, // 内容寻址键，即内容 SHA-256（与下载 ETag 同值）
			Sha256Short: shortHash(f.BlobHash),
			Sha1:        orDash(f.Sha1),
			Md5:         orDash(f.Md5),
			HasChecksum: f.BlobHash != "",
		})
	}
	sortBrowseRows(rows)
	return rows
}

// shortHash 取摘要前 12 位作折叠摘要（完整值在展开区与 title 里给出）。
func shortHash(hash string) string {
	const keep = 12
	if len(hash) <= keep {
		return hash
	}
	return hash[:keep] + "…"
}

// orDash 空值回退为占位符（历史数据可能未登记 sha1/md5）。
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// toISOUTC 把 asset 表存的 UTC "YYYY-MM-DD HH:MM:SS" 转成机器可读的 RFC3339（带 Z），
// 供 <time datetime> 承载。展示文案可按访问者时区改写，这个机器可读值恒为 UTC，
// 因此下载校验、日志比对与人工核对仍以同一口径为准。
// 形态不符（含空值与历史脏数据）返回空串，模板据此退化为纯文本而不渲染 <time>。
func toISOUTC(s string) string {
	at, err := time.Parse(assetTimeLayout, s)
	if err != nil {
		return ""
	}
	return at.Format("2006-01-02T15:04:05Z")
}

// assetTimeLayout 是 asset 表 created_at/updated_at 的存储形态（UTC，无时区后缀）。
const assetTimeLayout = "2006-01-02 15:04:05"

// lastSegment 返回路径的最后一段（无分隔符时返回原值）。
func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
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
