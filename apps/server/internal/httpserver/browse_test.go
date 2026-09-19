package httpserver_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// putArtifact 以管理员经 Bearer 写入一件制品到指定路径。
func (e *protocolEnv) putArtifact(t *testing.T, token, repo, path, contentType string, body []byte) {
	t.Helper()
	rec := e.rawReq(http.MethodPut, "/repository/"+repo+"/"+path, "Bearer "+token, contentType, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("写入 %s 制品失败：状态码 = %d（体：%s）", path, rec.Code, rec.Body.String())
	}
}

// browseReq 以浏览器身份发起 GET；返回响应与正文。
func (e *protocolEnv) browseReq(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("User-Agent", "Mozilla/5.0 (browser)")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestBrowseRendersDirectoryListing(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-public", "public")

	// 放置若干件制品，构造多级目录与文件。
	e.putArtifact(t, adminToken, "raw-public", "dir/sub/a.txt", "text/plain", []byte("alpha"))
	e.putArtifact(t, adminToken, "raw-public", "dir/sub/b.log", "text/plain", []byte("beta"))
	e.putArtifact(t, adminToken, "raw-public", "dir/sub2/c.txt", "text/plain", []byte("gamma"))
	e.putArtifact(t, adminToken, "raw-public", "readme.md", "text/plain", []byte("docs"))

	// 根目录浏览：匿名（public 仓）即可访问。
	rec := e.browseReq("/repository/raw-public/")
	if rec.Code != http.StatusOK {
		t.Fatalf("根目录浏览状态码 = %d，期望 200（体：%s）", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("根目录浏览 Content-Type = %q，期望 text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "raw-public") {
		t.Errorf("根目录页应含仓库名作为面包屑，体：%s", body)
	}
	// 根直接子项：dir/、readme.md（href 为绝对路径）。
	if !strings.Contains(body, `href="/repository/raw-public/dir/"`) {
		t.Errorf("根目录应列出子目录 dir/")
	}
	if !strings.Contains(body, `href="/repository/raw-public/readme.md"`) {
		t.Errorf("根目录应列出文件 readme.md")
	}

	// 子目录浏览：列出子目录 sub/、sub2/。
	rec = e.browseReq("/repository/raw-public/dir/")
	if rec.Code != http.StatusOK {
		t.Fatalf("子目录浏览状态码 = %d", rec.Code)
	}
	body = rec.Body.String()
	for _, want := range []string{`href="/repository/raw-public/dir/sub/"`, `href="/repository/raw-public/dir/sub2/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dir/ 页应含 %q（体：%s）", want, body)
		}
	}

	// 更深层级浏览：文件 a.txt、b.log。
	rec = e.browseReq("/repository/raw-public/dir/sub/")
	if rec.Code != http.StatusOK {
		t.Fatalf("深层目录浏览状态码 = %d", rec.Code)
	}
	body = rec.Body.String()
	for _, want := range []string{`href="/repository/raw-public/dir/sub/a.txt"`, `href="/repository/raw-public/dir/sub/b.log"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dir/sub/ 页应含 %q（体：%s）", want, body)
		}
	}
}

func TestBrowsePrivateRepoRequiresAuth(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-private", "private")
	e.putArtifact(t, adminToken, "raw-private", "a.txt", "text/plain", []byte("x"))

	// 匿名浏览私有仓 → 401。
	rec := e.browseReq("/repository/raw-private/")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("私有仓匿名浏览状态码 = %d，期望 401", rec.Code)
	}

	// 管理员可浏览。
	req := httptest.NewRequest(http.MethodGet, "/repository/raw-private/", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	r := httptest.NewRecorder()
	e.h.ServeHTTP(r, req)
	if r.Code != http.StatusOK {
		t.Fatalf("管理员浏览私有仓状态码 = %d，期望 200", r.Code)
	}
}

func TestBrowseNotTriggeredForNonHtmlClient(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-public", "public")
	e.putArtifact(t, adminToken, "raw-public", "a.txt", "text/plain", []byte("hello"))

	// 非 HTML Accept + 无浏览器 UA（如 curl）：不应触发浏览，按常规 404。
	req := httptest.NewRequest(http.MethodGet, "/repository/raw-public/", nil)
	req.Header.Set("Accept", "*/*")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("非浏览器客户端访问目录状态码 = %d，期望 404", rec.Code)
	}
}

func TestBrowseHeadDoesNotRender(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-public", "public")
	e.putArtifact(t, adminToken, "raw-public", "a.txt", "text/plain", []byte("x"))

	// HEAD 目录不应触发浏览（tryBrowse 仅响应 GET）。
	req := httptest.NewRequest(http.MethodHead, "/repository/raw-public/", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK && strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("HEAD 目录不应渲染 HTML 目录页，状态码=%d", rec.Code)
	}
}

func TestBrowseEscapesHTMLInNames(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-public", "public")
	// 放置名称含 HTML 元字符的制品（路径转义后存储）。
	e.putArtifact(t, adminToken, "raw-public", "<b>.txt", "text/plain", []byte("x"))

	rec := e.browseReq("/repository/raw-public/")
	if rec.Code != http.StatusOK {
		t.Fatalf("浏览状态码 = %d", rec.Code)
	}
	body := rec.Body.String()
	// 模板应转义 <，不应出现未转义的标签。
	if strings.Contains(body, "<b>.txt</a>") {
		t.Errorf("HTML 未转义文件名中的 <b>（体：%s）", body)
	}
}

// seedAssetRows 直接写 asset 表构造大批量制品（不经 PUT / blob，浏览页只读资产元数据）。
func (e *protocolEnv) seedAssetRows(t *testing.T, repoName string, paths ...string) {
	t.Helper()
	repo, err := e.repoRepo.GetByName(repoName)
	if err != nil {
		t.Fatalf("取仓库 %s：%v", repoName, err)
	}
	for _, p := range paths {
		if err := e.assetRepo.Upsert(repo.ID, p, "hash-"+p, int64(len(p)), "text/plain", "", ""); err != nil {
			t.Fatalf("Upsert %s：%v", p, err)
		}
	}
}

// TestBrowseListsAllDirsWhenOneSubtreeIsHuge 回归 FR-25c：某顶层目录下制品极多时，
// 同层的其它顶层目录必须照样列出。旧实现按「前缀下的前 1000 条制品」推导直接子项，
// 路径全局升序会让 a/ 的制品吃满配额，z/ 整片消失——列表既不完整也不正确。
func TestBrowseListsAllDirsWhenOneSubtreeIsHuge(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-public", "public")

	paths := make([]string, 0, 1201)
	for i := 0; i < 1200; i++ {
		paths = append(paths, fmt.Sprintf("a/f%04d.txt", i))
	}
	paths = append(paths, "z/only.txt")
	e.seedAssetRows(t, "raw-public", paths...)

	rec := e.browseReq("/repository/raw-public/")
	if rec.Code != http.StatusOK {
		t.Fatalf("根目录浏览状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`href="/repository/raw-public/a/"`,
		`href="/repository/raw-public/z/"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("根页应列出 %q（体：%s）", want, body)
		}
	}
	// 计数按「当前层级」口径：根只有 2 个目录、0 个直接文件（不是子树的 1201）。
	if !strings.Contains(body, "目录 2 个") {
		t.Errorf("根页目录数应为 2，体：%s", body)
	}
	if !strings.Contains(body, "文件 0 项") {
		t.Errorf("根页直接文件数应为 0，体：%s", body)
	}
	if strings.Contains(body, "仅显示前") {
		t.Errorf("不应再出现截断提示，体：%s", body)
	}
}

// TestBrowsePaginatesFilesAndKeepsDirs 验证直接文件分页：分页只作用于文件，
// 每页都带全部子目录，计数与页码基于精确总数。
func TestBrowsePaginatesFilesAndKeepsDirs(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-public", "public")

	paths := []string{"sub/x.bin"}
	for i := 0; i < 1200; i++ {
		paths = append(paths, fmt.Sprintf("f%04d.bin", i))
	}
	e.seedAssetRows(t, "raw-public", paths...)

	// 第 1 页：1 行目录 + 500 行文件；给出精确计数与末页链接。
	rec := e.browseReq("/repository/raw-public/")
	if rec.Code != http.StatusOK {
		t.Fatalf("第 1 页状态码 = %d", rec.Code)
	}
	body := rec.Body.String()
	if got := strings.Count(body, `class="name"`); got != 501 {
		t.Errorf("第 1 页行数 = %d，期望 501（1 目录 + 500 文件）", got)
	}
	for _, want := range []string{
		`href="/repository/raw-public/sub/"`,
		"f0000.bin",
		"f0499.bin",
		"目录 1 个",
		"文件 1200 项",
		"第 1 / 3 页",
		"当前显示第 1–500 项",
		"page=2&amp;per_page=500",
		"page=3&amp;per_page=500",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("第 1 页应含 %q", want)
		}
	}
	if strings.Contains(body, "f0500.bin") {
		t.Errorf("第 1 页不应出现第 501 条之后的文件")
	}

	// 末页：1200 - 1000 = 200 条，且子目录仍在。
	rec = e.browseReq("/repository/raw-public/?page=3")
	body = rec.Body.String()
	if got := strings.Count(body, `class="name"`); got != 201 {
		t.Errorf("末页行数 = %d，期望 201（1 目录 + 200 文件）", got)
	}
	for _, want := range []string{
		`href="/repository/raw-public/sub/"`,
		"f1199.bin",
		"第 3 / 3 页",
		"当前显示第 1001–1200 项",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("末页应含 %q", want)
		}
	}
	if strings.Contains(body, "f0000.bin") {
		t.Errorf("末页不应出现第 1 条文件")
	}

	// 超出末页的页码收敛到末页，不显示空页。
	rec = e.browseReq("/repository/raw-public/?page=9999")
	if body = rec.Body.String(); !strings.Contains(body, "第 3 / 3 页") {
		t.Errorf("越界页码应收敛到末页，体：%s", body)
	}

	// per_page 生效：2000/页时 1200 条一页看完，且回到第 1 页。
	rec = e.browseReq("/repository/raw-public/?per_page=2000")
	body = rec.Body.String()
	if got := strings.Count(body, `class="name"`); got != 1201 {
		t.Errorf("per_page=2000 时行数 = %d，期望 1201", got)
	}
	if !strings.Contains(body, "f1199.bin") || strings.Contains(body, "第 2 / 2 页") {
		t.Errorf("per_page=2000 应一页看完 1200 条直接文件")
	}

	// per_page 超上限时被夹到上限，不出现无界渲染。
	rec = e.browseReq("/repository/raw-public/?per_page=999999")
	if body = rec.Body.String(); !strings.Contains(body, `class="on">5000`) {
		t.Errorf("per_page 超上限应夹到 5000，体：%s", body)
	}

	// per_page 低于下限时被夹到下限：1200 条 ÷ 50 = 24 页（而非按 1 条/页渲染 1200 页）。
	rec = e.browseReq("/repository/raw-public/?per_page=1")
	body = rec.Body.String()
	if !strings.Contains(body, "第 1 / 24 页") {
		t.Errorf("per_page 低于下限应夹到 50（1200 条 → 24 页），体：%s", body)
	}
	if got := strings.Count(body, `class="name"`); got != 51 {
		t.Errorf("per_page 夹到下限后行数 = %d，期望 51（1 目录 + 50 文件）", got)
	}
}

// TestBrowsePagingLinksPreserveOtherQuery 验证翻页链接保留既有查询参数。
func TestBrowsePagingLinksPreserveOtherQuery(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-public", "public")

	paths := []string{"sub/x.bin"}
	for i := 0; i < 60; i++ {
		paths = append(paths, fmt.Sprintf("f%02d.bin", i))
	}
	e.seedAssetRows(t, "raw-public", paths...)

	rec := e.browseReq("/repository/raw-public/?per_page=50&tag=keep")
	body := rec.Body.String()
	if !strings.Contains(body, "tag=keep") {
		t.Errorf("翻页链接应保留既有查询参数 tag=keep，体：%s", body)
	}
	if !strings.Contains(body, "第 1 / 2 页") {
		t.Errorf("60 条直接文件、50/页应为 2 页，体：%s", body)
	}
}

// assetMeta 是可控元数据的制品种子（校验和与时间由用例指定）。
type assetMeta struct {
	Path      string
	Size      int64
	BlobHash  string
	Sha1      string
	Md5       string
	CreatedAt string
	UpdatedAt string
}

// seedAssetMeta 按指定元数据写 asset 表，用于断言目录页展示的字段值。
func (e *protocolEnv) seedAssetMeta(t *testing.T, repoName string, items ...assetMeta) {
	t.Helper()
	repo, err := e.repoRepo.GetByName(repoName)
	if err != nil {
		t.Fatalf("取仓库 %s：%v", repoName, err)
	}
	for _, it := range items {
		if err := e.assetRepo.UpsertWithTime(repo.ID, it.Path, it.BlobHash, it.Size, "text/plain",
			it.Sha1, it.Md5, it.CreatedAt, it.UpdatedAt); err != nil {
			t.Fatalf("UpsertWithTime %s：%v", it.Path, err)
		}
	}
}

// TestBrowseShowsTimesAndChecksums 验证目录页展示创建/修改时间与校验和：
// 时间按存储值原样展示并在表头标注 UTC；校验和折叠展示，短摘要可见、完整值可展开复制。
func TestBrowseShowsTimesAndChecksums(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-public", "public")

	const sha256 = "3f2a9b7c1d45e6f70819a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7"
	const sha1Value = "aabbccddeeff00112233445566778899aabbccdd"
	const md5Value = "00112233445566778899aabbccddeeff"
	e.seedAssetMeta(t, "raw-public", assetMeta{
		Path: "pkg/demo-1.0.jar", Size: 2048, BlobHash: sha256,
		Sha1: sha1Value, Md5: md5Value,
		CreatedAt: "2026-01-05 08:00:00", UpdatedAt: "2026-03-08 09:30:15",
	})
	// 历史数据可能未登记 sha1/md5，需回退占位符。
	e.seedAssetMeta(t, "raw-public", assetMeta{
		Path: "pkg/legacy.bin", Size: 1024, BlobHash: "deadbeef" + strings.Repeat("0", 56),
	})
	e.seedAssetMeta(t, "raw-public", assetMeta{
		Path: "pkg/old/dir-only.txt", Size: 1, BlobHash: strings.Repeat("f", 64),
	})

	rec := e.browseReq("/repository/raw-public/pkg/")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"创建时间 (UTC)",
		"修改时间 (UTC)",
		"校验和",
		"2026-01-05 08:00:00", // 创建时间按存储值原样展示
		"2026-03-08 09:30:15", // 修改时间
		"3f2a9b7c1d45…",       // 折叠摘要（短形式）
		`title="` + sha256 + `"`,
		"<details>",
		"<dt>SHA-256</dt><dd>" + sha256 + "</dd>", // 完整值在展开区
		"<dt>SHA-1</dt><dd>" + sha1Value + "</dd>",
		"<dt>MD5</dt><dd>" + md5Value + "</dd>",
		"<dd>—</dd>", // legacy.bin 缺失的 sha1/md5 回退占位
		"文件 2 项",
		"合计 3.0 KiB", // 2048 + 1024
	} {
		if !strings.Contains(body, want) {
			t.Errorf("目录页应含 %q", want)
		}
	}
	// 子目录行的时间与校验和列应为占位符（不是空单元格）。
	if !strings.Contains(body, `<td class="time">-</td>`) || !strings.Contains(body, `<td class="sum">-</td>`) {
		t.Errorf("子目录行的时间/校验和列应为占位符，体：%s", body)
	}
	// 时区口径：时间为 <time datetime=RFC3339Z>（机器可读值恒为 UTC），展示文案由页内脚本
	// 按访问者时区改写；表头两套文案由 data-local 提供，避免服务端与脚本各写一份。
	for _, want := range []string{
		`<time datetime="2026-01-05T08:00:00Z">2026-01-05 08:00:00</time>`,
		`<time datetime="2026-03-08T09:30:15Z">2026-03-08 09:30:15</time>`,
		`data-local="创建时间（本地时区）"`,
		`data-local="修改时间（本地时区）"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("目录页应含 %q（体：%s）", want, body)
		}
	}
	// 时区改写脚本必须内联在页内：无外链脚本、无 eval。
	if !strings.Contains(body, `querySelectorAll("time[datetime]")`) {
		t.Errorf("目录页应含按 <time datetime> 改写时区的内联脚本，体：%s", body)
	}
	if strings.Contains(body, "<script src") || strings.Contains(body, "eval(") {
		t.Errorf("时区脚本必须内联且不得使用 eval")
	}
}

// TestBrowseFallsBackToPlainTextForInvalidTime 验证脏时间数据不会让页面渲染出
// 无效的 <time datetime>：形态不符时退化为纯文本展示。
func TestBrowseFallsBackToPlainTextForInvalidTime(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-public", "public")
	// 直接写库构造历史脏数据（非 "YYYY-MM-DD HH:MM:SS" 形态）。
	e.seedAssetMeta(t, "raw-public", assetMeta{
		Path: "dirty/odd.bin", Size: 8, BlobHash: strings.Repeat("a", 64),
		UpdatedAt: "not-a-timestamp",
	})

	rec := e.browseReq("/repository/raw-public/dirty/")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "not-a-timestamp") {
		t.Errorf("脏时间应原样展示，体：%s", body)
	}
	if strings.Contains(body, `datetime="not-a-timestamp"`) ||
		strings.Contains(body, "T00:00:00Z") {
		t.Errorf("形态不符的时间不得渲染为 <time datetime>，体：%s", body)
	}
}

// TestBrowseShowsDirStats 验证目录行给出子树规模与最近更新时间：计数含更深层级、
// 时间为子树内最大值，并用 title 标注用途以免与文件行的「大小 / 修改时间」混淆。
func TestBrowseShowsDirStats(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-public", "public")

	e.seedAssetMeta(t, "raw-public",
		assetMeta{Path: "libs/a/one.bin", Size: 10, BlobHash: strings.Repeat("1", 64),
			UpdatedAt: "2026-02-01 10:00:00"},
		assetMeta{Path: "libs/a/deep/two.bin", Size: 20, BlobHash: strings.Repeat("2", 64),
			UpdatedAt: "2026-05-20 12:00:00"},
		assetMeta{Path: "libs/b/three.bin", Size: 30, BlobHash: strings.Repeat("3", 64),
			UpdatedAt: "2025-12-31 23:59:59"},
		assetMeta{Path: "libs/note.txt", Size: 40, BlobHash: strings.Repeat("4", 64),
			UpdatedAt: "2026-06-01 00:00:00"},
	)

	rec := e.browseReq("/repository/raw-public/libs/")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		// 子目录行：a/ 计 2 件（含 deep/ 下的制品），时间是子树内的最大 updated_at。
		`title="目录内制品总数（含子目录）">2 项</td>`,
		`title="目录内最近一次更新时间"><time datetime="2026-05-20T12:00:00Z">2026-05-20 12:00:00</time></td>`,
		`title="目录内制品总数（含子目录）">1 项</td>`,
		`title="目录内最近一次更新时间"><time datetime="2025-12-31T23:59:59Z">2025-12-31 23:59:59</time></td>`,
		// 文件行仍是文件自身的体积与时间。
		"2026-06-01 00:00:00",
		"文件 1 项",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("目录页应含 %q（体：%s）", want, body)
		}
	}

	// 根页：libs/ 的计数是整棵子树（4 件），不是当前层的 1 项。
	rec = e.browseReq("/repository/raw-public/")
	if body = rec.Body.String(); !strings.Contains(body, `title="目录内制品总数（含子目录）">4 项</td>`) {
		t.Errorf("根页的 libs/ 应为整棵子树的 4 项，体：%s", body)
	}
}
