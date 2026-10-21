package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
)

// 本文件是 FR-149 §3.2 两个运维端点的**路由级**冒烟：走生产路由表与生产中间件链，
// 确认 main.go 的注册真的生效、且鉴权边界与契约端点一致。
//
// 端点级的 202/409/403 细节由 internal/api/search_index_handlers_test.go 用桩逐条覆盖
// （那里的 409 是确定性用例，不依赖后台任务时序）；这里只补「注册 + 中间件」这一层。
func TestSearchIndexRoutesRegisteredThroughProductionRouter(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv(config.EnvJWTSecret, "search-index-routes-test-secret-key-32b")
	t.Setenv(config.EnvEnabledFormats, "raw")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	t.Cleanup(func() { _ = svc.db.Close() })
	server := httptest.NewServer(newApplicationHandler(cfg, svc, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}))
	t.Cleanup(server.Close)

	request := func(method, path, token string, body, out any) (int, string) {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求：%v", err)
		}
		req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("构造 %s %s 请求：%v", method, path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("执行 %s %s 请求：%v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("读取 %s %s 响应：%v", method, path, err)
		}
		if out != nil && len(raw) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatalf("解析 %s %s 响应：%v（体：%s）", method, path, err, raw)
			}
		}
		return resp.StatusCode, string(raw)
	}

	// 匿名：authMW 允许通过，admin 校验在 handler 内 → 401。
	if code, _ := request(http.MethodGet, "/api/v1/search/index", "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("匿名读状态 = %d，期望 401（端点未注册时会落到 SPA 回退，通常不是 401）", code)
	}
	if code, _ := request(http.MethodPost, "/api/v1/search/index/rebuild", "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("匿名触发重建 = %d，期望 401", code)
	}

	// 管理员：先自举再登录。
	if code, body := request(http.MethodPost, "/api/v1/auth/bootstrap", "", api.BootstrapRequest{
		Username: "admin", Password: "admin-pass-123",
	}, nil); code != http.StatusCreated {
		t.Fatalf("自举状态 = %d：%s", code, body)
	}
	var login api.LoginResponse
	if code, body := request(http.MethodPost, "/api/v1/auth/login", "", api.LoginRequest{
		Username: "admin", Password: "admin-pass-123",
	}, &login); code != http.StatusOK {
		t.Fatalf("登录状态 = %d：%s", code, body)
	}

	// 状态端点：字段齐全（全新库上回填未开始 → ready=false、计数为 0）。
	var status map[string]any
	if code, body := request(http.MethodGet, "/api/v1/search/index", login.Token, nil, &status); code != http.StatusOK {
		t.Fatalf("管理员读状态 = %d：%s", code, body)
	}
	for _, key := range []string{"ready", "indexed", "total", "lastId", "updatedAt"} {
		if _, ok := status[key]; !ok {
			t.Fatalf("状态响应缺字段 %s：%v", key, status)
		}
	}
	if status["ready"] != false {
		t.Fatalf("全新库上的 ready 应为 false（回填尚未开始）：%v", status)
	}

	// 重建端点：202 受理。随后轮询状态直到 ready=true——空库回填是瞬时的，
	// 这一步同时证明「受理后后台任务真的跑起来了」。
	var accepted map[string]any
	if code, body := request(http.MethodPost, "/api/v1/search/index/rebuild", login.Token, nil, &accepted); code != http.StatusAccepted {
		t.Fatalf("受理重建 = %d，期望 202：%s", code, body)
	}
	for i := 0; i < 200; i++ {
		if code, body := request(http.MethodGet, "/api/v1/search/index", login.Token, nil, &status); code != http.StatusOK {
			t.Fatalf("轮询状态 = %d：%s", code, body)
		}
		if status["ready"] == true {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待重建完成超时：%v", status)
}
