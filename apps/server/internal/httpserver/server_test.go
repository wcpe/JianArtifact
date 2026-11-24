package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

func decodeHealth(t *testing.T, body []byte) api.HealthStatus {
	t.Helper()
	var hs api.HealthStatus
	if err := json.Unmarshal(body, &hs); err != nil {
		t.Fatalf("解析 HealthStatus 失败：%v（体：%s）", err, body)
	}
	return hs
}

func TestGetHealthz(t *testing.T) {
	t.Parallel()
	h := New("test-version").Handler(nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	hs := decodeHealth(t, rec.Body.Bytes())
	if hs.Status != api.HealthStatusStatusOk {
		t.Errorf("status = %q，期望 ok", hs.Status)
	}
	// 匿名请求版本号脱敏为空串（认证路径的完整版本断言见 integration_test）。
	if hs.Version != "" {
		t.Errorf("匿名 healthz version = %q，期望空串", hs.Version)
	}
}

func TestGetReadyz(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		opts     []Option
		wantCode int
		wantStat api.HealthStatusStatus
	}{
		{name: "无依赖恒就绪", wantCode: http.StatusOK, wantStat: api.HealthStatusStatusOk},
		{
			name:     "任一依赖未就绪返回503",
			opts:     []Option{WithReadinessCheck(func() error { return nil }), WithReadinessCheck(func() error { return errors.New("依赖未就绪") })},
			wantCode: http.StatusServiceUnavailable,
			wantStat: api.HealthStatusStatusUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New("v", tt.opts...).Handler(nil)
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("状态码 = %d，期望 %d", rec.Code, tt.wantCode)
			}
			hs := decodeHealth(t, rec.Body.Bytes())
			if hs.Status != tt.wantStat {
				t.Errorf("status = %q，期望 %q", hs.Status, tt.wantStat)
			}
		})
	}
}

func TestStaticAndSPAFallback(t *testing.T) {
	t.Parallel()
	assets := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<!doctype html><title>jian</title>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	h := New("v").Handler(assets)

	tests := []struct {
		name      string
		path      string
		wantCode  int
		wantBody  string
		wantCache string
	}{
		{name: "根路径返回index", path: "/", wantCode: http.StatusOK, wantBody: "<!doctype html><title>jian</title>", wantCache: "no-cache"},
		{name: "静态资源命中", path: "/assets/app.js", wantCode: http.StatusOK, wantBody: "console.log(1)", wantCache: "public, max-age=31536000, immutable"},
		{name: "未知前端路由回退index", path: "/repositories/maven", wantCode: http.StatusOK, wantBody: "<!doctype html><title>jian</title>", wantCache: "no-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Fatalf("状态码 = %d，期望 %d", rec.Code, tt.wantCode)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("体 = %q，期望 %q", got, tt.wantBody)
			}
			if got := rec.Header().Get("Cache-Control"); got != tt.wantCache {
				t.Errorf("Cache-Control = %q，期望 %q", got, tt.wantCache)
			}
		})
	}
}

func TestReservedProtocolPrefixDoesNotFallbackToSPA(t *testing.T) {
	t.Parallel()
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}
	h := New("v", WithProtocolPrefixes("/repository", "/npm")).Handler(assets)
	for _, path := range []string{"/repository/raw/a.txt", "/npm/demo", "/npm"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s 状态码 = %d，期望 404", path, rec.Code)
		}
		if rec.Body.String() == "index" {
			t.Errorf("%s 不应回退到 SPA 首页", path)
		}
	}
}

// 契约路由优先于静态回退：即便挂了前端资源，/healthz 仍走契约处理器。
func TestContractRouteTakesPrecedenceOverStatic(t *testing.T) {
	t.Parallel()
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html></html>")},
	}
	h := New("v").Handler(assets)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if hs := decodeHealth(t, rec.Body.Bytes()); hs.Status != api.HealthStatusStatusOk {
		t.Errorf("status = %q，期望 ok（应命中契约路由而非静态资源）", hs.Status)
	}
}

const spaTestIndex = "<!doctype html><title>jian</title>"

// spaTestAssets 构造一份贴近真实产物的最小前端资源集合：
// index.html 供 SPA 回退，assets/ 下的带 hash 资源与根目录 favicon 供静态命中，
// .well-known 与 favicon 一样属于「首段不在前端路由表里但真实存在」的文件。
func spaTestAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           &fstest.MapFile{Data: []byte(spaTestIndex)},
		"assets/app.js":        &fstest.MapFile{Data: []byte("console.log(1)")},
		"favicon.svg":          &fstest.MapFile{Data: []byte("<svg/>")},
		"mockServiceWorker.js": &fstest.MapFile{Data: []byte("self.addEventListener")},
		".well-known/security": &fstest.MapFile{Data: []byte("mailto:sec@example.com")},
	}
}

// assertJSONNotFound 断言响应为 404 + JSON 错误体，且响应体不是前端首页内容。
// 这是本次收窄的核心：调用方（AI agent / SDK）必须能凭 Content-Type 与错误码
// 判定"资源不存在"，而不是把 HTML 当数据解析。
func assertJSONNotFound(t *testing.T, rec *httptest.ResponseRecorder, path string) {
	t.Helper()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%s 状态码 = %d，期望 404（体：%s）", path, rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("%s Content-Type = %q，期望 application/json; charset=utf-8", path, got)
	}
	if rec.Body.String() == spaTestIndex {
		t.Errorf("%s 不得回退到 SPA 首页", path)
	}
	var body api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s 响应体不是合法 JSON：%v（体：%s）", path, err, rec.Body.String())
	}
	if body.Error.Code != "not_found" {
		t.Errorf("%s 错误码 = %q，期望 not_found", path, body.Error.Code)
	}
}

// 非前端路由一律 404 + JSON：历史上这些路径返回 200 + HTML，
// 导致 AI agent 把 HTML 误当 XML / JSON 数据源。
func TestUnknownPathReturnsJSONNotFound(t *testing.T) {
	t.Parallel()
	h := New("v").Handler(spaTestAssets())
	for _, path := range []string{
		"/api/v1/nonexistent",
		"/org/x/maven-metadata.xml",
		"/llms.txt",
		"/robots.txt",
		"/.well-known/ai-plugin.json",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assertJSONNotFound(t, rec, path)
		})
	}
}

// 白名单内的前端路由（含其子路径）继续回退 index.html，且缓存策略不变。
func TestWhitelistedFrontendRoutesFallbackToIndex(t *testing.T) {
	t.Parallel()
	h := New("v").Handler(spaTestAssets())
	for _, path := range []string{
		"/",
		"/setup",
		"/login",
		"/repositories",
		"/repositories/foo",
		"/search",
		"/licenses",
		"/settings",
		"/audit-logs",
		"/host-monitoring",
		"/dashboard",
		"/users",
		"/user-groups",
		"/tokens",
		"/migrations",
		"/migrations/new",
		"/maintenance",
		"/security",
		"/p/somename",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s 状态码 = %d，期望 200（体：%s）", path, rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != spaTestIndex {
				t.Errorf("%s 体 = %q，期望 index.html 内容", path, got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
				t.Errorf("%s Cache-Control = %q，期望 no-cache", path, got)
			}
		})
	}
}

// 真实静态资源先于白名单判定命中：它们的首段不在前端路由表里，
// 若反过来先判白名单就会全部 404。这里同时锁定两类缓存策略。
func TestStaticAssetServedBeforeWhitelistCheck(t *testing.T) {
	t.Parallel()
	h := New("v").Handler(spaTestAssets())
	tests := []struct {
		name      string
		path      string
		wantBody  string
		wantCache string
	}{
		{name: "带hash的assets目录", path: "/assets/app.js", wantBody: "console.log(1)", wantCache: "public, max-age=31536000, immutable"},
		{name: "根目录无hash资源", path: "/favicon.svg", wantBody: "<svg/>", wantCache: "no-cache"},
		{name: "服务worker脚本", path: "/mockServiceWorker.js", wantBody: "self.addEventListener", wantCache: "no-cache"},
		// 首段 ".well-known" 不在前端路由白名单里，但文件真实存在，必须先命中文件。
		{name: "well-known目录下的真实文件", path: "/.well-known/security", wantBody: "mailto:sec@example.com", wantCache: "no-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200（体：%s）", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("体 = %q，期望 %q", got, tt.wantBody)
			}
			if got := rec.Header().Get("Cache-Control"); got != tt.wantCache {
				t.Errorf("Cache-Control = %q，期望 %q", got, tt.wantCache)
			}
		})
	}
}

// 协议前缀即使首段碰巧与前端路由无关也仍被拒绝（双保险）。
// 这里断言的是「不回退 HTML」，与既有实现一致，不额外要求 JSON。
func TestReservedProtocolPrefixStillNotFallbackAfterWhitelist(t *testing.T) {
	t.Parallel()
	h := New("v", WithProtocolPrefixes("/repository", "/npm")).Handler(spaTestAssets())
	for _, path := range []string{"/repository/raw/a.txt", "/npm/demo", "/npm"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("状态码 = %d，期望 404", rec.Code)
			}
			if rec.Body.String() == spaTestIndex {
				t.Errorf("%s 不应回退到 SPA 首页", path)
			}
		})
	}
}

// 非 GET/HEAD 未命中路由时仍是裸 404（保持既有行为，不改成 JSON）。
func TestNonGetMethodStillPlainNotFound(t *testing.T) {
	t.Parallel()
	h := New("v").Handler(spaTestAssets())
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/repositories", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("状态码 = %d，期望 404", rec.Code)
			}
			if rec.Body.String() == spaTestIndex {
				t.Errorf("%s 不应回退到 SPA 首页", method)
			}
		})
	}
}

// 归一化后的路径决定判定结果：带 ".." 的路径先被 path.Clean 折叠，
// 不能靠"看上去首段是 repositories"骗到 HTML 回退。
func TestDotSegmentsDoNotBypassWhitelist(t *testing.T) {
	t.Parallel()
	h := New("v").Handler(spaTestAssets())
	for _, path := range []string{"/repositories/../api/v1/nonexistent", "/repositories/../llms.txt"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Body.String() == spaTestIndex {
				t.Fatalf("%s 不得回退到 SPA 首页（状态码 %d）", path, rec.Code)
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s 状态码 = %d，期望 404（体：%s）", path, rec.Code, rec.Body.String())
			}
		})
	}
}

// spaFirstSegment 的边界：重复斜杠、尾斜杠、缺失前导斜杠都要归一到同一首段。
func TestSPAFirstSegment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want string
	}{
		{path: "/", want: ""},
		{path: "", want: ""},
		{path: "/repositories", want: "repositories"},
		{path: "/repositories/", want: "repositories"},
		{path: "//repositories/foo", want: "repositories"},
		{path: "repositories/foo", want: "repositories"},
		{path: "/api/v1/nonexistent", want: "api"},
		{path: "/.well-known/ai-plugin.json", want: ".well-known"},
		{path: "/p/somename", want: "p"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := spaFirstSegment(tt.path); got != tt.want {
				t.Errorf("spaFirstSegment(%q) = %q，期望 %q", tt.path, got, tt.want)
			}
		})
	}
}
