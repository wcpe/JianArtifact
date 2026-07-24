package httpserver_test

import (
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
