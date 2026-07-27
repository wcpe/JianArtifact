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
	h := New("test-version").Handler(nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	hs := decodeHealth(t, rec.Body.Bytes())
	if hs.Status != api.Ok {
		t.Errorf("status = %q，期望 ok", hs.Status)
	}
	// 匿名请求版本号脱敏为空串（认证路径的完整版本断言见 integration_test）。
	if hs.Version != "" {
		t.Errorf("匿名 healthz version = %q，期望空串", hs.Version)
	}
}

func TestGetReadyz(t *testing.T) {
	tests := []struct {
		name     string
		opts     []Option
		wantCode int
		wantStat api.HealthStatusStatus
	}{
		{name: "无依赖恒就绪", wantCode: http.StatusOK, wantStat: api.Ok},
		{
			name:     "任一依赖未就绪返回503",
			opts:     []Option{WithReadinessCheck(func() error { return nil }), WithReadinessCheck(func() error { return errors.New("依赖未就绪") })},
			wantCode: http.StatusServiceUnavailable,
			wantStat: api.Unavailable,
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
	assets := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<!doctype html><title>jian</title>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	h := New("v").Handler(assets)

	tests := []struct {
		name     string
		path     string
		wantCode int
		wantBody string
	}{
		{name: "根路径返回index", path: "/", wantCode: http.StatusOK, wantBody: "<!doctype html><title>jian</title>"},
		{name: "静态资源命中", path: "/assets/app.js", wantCode: http.StatusOK, wantBody: "console.log(1)"},
		{name: "未知前端路由回退index", path: "/repositories/maven", wantCode: http.StatusOK, wantBody: "<!doctype html><title>jian</title>"},
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
		})
	}
}

// 契约路由优先于静态回退：即便挂了前端资源，/healthz 仍走契约处理器。
func TestContractRouteTakesPrecedenceOverStatic(t *testing.T) {
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
	if hs := decodeHealth(t, rec.Body.Bytes()); hs.Status != api.Ok {
		t.Errorf("status = %q，期望 ok（应命中契约路由而非静态资源）", hs.Status)
	}
}
