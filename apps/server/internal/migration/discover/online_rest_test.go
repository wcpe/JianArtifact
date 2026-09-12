package discover_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/migration/credential"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

func TestOnlineRESTDiscover(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); ok {
			if u != "admin" || p != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "maven-releases", "format": "maven2", "type": "hosted"},
			{"name": "npm-proxy", "format": "npm", "type": "proxy"},
			{"name": "docker-hub", "format": "docker", "type": "proxy"},
		})
	})
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		repo := r.URL.Query().Get("repository")
		items := []any{}
		switch repo {
		case "maven-releases":
			items = []any{map[string]string{"path": "a"}, map[string]string{"path": "b"}}
		case "npm-proxy":
			items = []any{map[string]string{"path": "p"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "continuationToken": ""})
	})
	mux.HandleFunc("/service/rest/v1/repositories/npm/proxy/npm-proxy", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"proxy":{"remoteUrl":"https://registry.npmjs.org"}}`))
	})
	mux.HandleFunc("/service/rest/v1/repositories/docker/proxy/docker-hub", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"proxy":{"remoteUrl":"https://registry.example"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	src := discover.NewOnlineREST(upstream.NewTestClient(time.Second))
	plan, err := src.Discover(context.Background(), discover.Config{
		URL:        srv.URL,
		SourceRef:  "NEXUS_TEST",
		Credential: "admin:secret",
	})
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	if len(plan.Repositories) != 3 {
		t.Fatalf("repos = %+v", plan.Repositories)
	}
	if !plan.Estimated {
		t.Error("期望 estimated=true")
	}
	foundDockerWarn := false
	for _, w := range plan.Warnings {
		if contains(w, "docker") {
			foundDockerWarn = true
		}
	}
	if foundDockerWarn {
		t.Errorf("docker proxy 不应再被 warning 跳过：%v", plan.Warnings)
	}
	if plan.SourceRef != "NEXUS_TEST" || contains(plan.SourceRef, srv.URL) {
		t.Errorf("sourceRef 不应暴露来源地址：%q", plan.SourceRef)
	}
}

func TestOnlineRESTListRemoteRepositories(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "r3d", "format": "maven2", "type": "hosted"},
			{"name": "r3d-mixed", "format": "maven2", "type": "hosted"},
			{"name": "docker-hub", "format": "docker", "type": "proxy"},
		})
	})
	// 故意不注册 assets：List 不应访问
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	items, err := discover.NewOnlineREST(upstream.NewTestClient(time.Second)).ListRemoteRepositories(
		context.Background(), srv.URL, "", true,
	)
	if err != nil {
		t.Fatalf("ListRemoteRepositories：%v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %+v, want 3", items)
	}
	if items[0].Name != "r3d" || items[0].Format != "maven" {
		t.Fatalf("items[0] = %+v", items[0])
	}
}

func TestOnlineRESTAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	_, err := discover.NewOnlineREST(upstream.NewTestClient(time.Second)).Discover(context.Background(), discover.Config{
		URL:        srv.URL,
		Credential: "bad:cred",
	})
	if _, ok := err.(*discover.ErrAuth); !ok {
		t.Fatalf("err = %T %v", err, err)
	}
}

func TestOnlineRESTUnreachable(t *testing.T) {
	_, err := discover.NewOnlineREST(nil).Discover(context.Background(), discover.Config{
		URL: "http://127.0.0.1:1",
	})
	if _, ok := err.(*discover.ErrUpstream); !ok {
		t.Fatalf("err = %T %v", err, err)
	}
}

func TestOnlineRESTRejectsPrivateSourceAddress(t *testing.T) {
	_, err := discover.NewOnlineREST(nil).Discover(context.Background(), discover.Config{
		URL:        "http://127.0.0.1:8081",
		Credential: "admin:不应泄露",
	})
	if !errors.Is(err, upstream.ErrUnsafeURL) {
		t.Fatalf("私网来源应被统一出站策略拒绝，得 %v", err)
	}
	if contains(err.Error(), "不应泄露") || contains(err.Error(), "127.0.0.1") {
		t.Fatalf("错误不得泄露凭据或来源地址：%v", err)
	}
}

func TestOnlineRESTRejectsDangerousRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://127.0.0.1/metadata", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	_, err := discover.NewOnlineREST(upstream.NewTestClient(time.Second)).Discover(context.Background(), discover.Config{
		URL:        srv.URL,
		Credential: "admin:不应泄露",
	})
	if !errors.Is(err, upstream.ErrUnsafeURL) {
		t.Fatalf("危险重定向应被统一出站策略拒绝，得 %v", err)
	}
	if contains(err.Error(), "不应泄露") || contains(err.Error(), "127.0.0.1") {
		t.Fatalf("错误不得泄露凭据或来源地址：%v", err)
	}
}

// TestOnlineRESTAllowsPrivateSourceAddress 验证：当用户显式声明 allowPrivateSource，
// 回环/私网来源地址（如本机/内网 Nexus）可被放行，SSRF 防护按来源维度关闭。
func TestOnlineRESTAllowsPrivateSourceAddress(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "maven-snapshots", "format": "maven2", "type": "hosted"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// NewOnlineREST(nil) 表示未注入显式客户端：以 allowPrivateSource=true 构造放行私网的客户端。
	plan, err := discover.NewOnlineREST(nil).Discover(context.Background(), discover.Config{
		URL:                srv.URL,
		AllowPrivateSource: true,
	})
	if err != nil {
		t.Fatalf("私网来源（显式放行）应成功，得 %v", err)
	}
	if len(plan.Repositories) != 1 || plan.Repositories[0].Name != "maven-snapshots" {
		t.Fatalf("repos = %+v", plan.Repositories)
	}
}

// TestOnlineRESTListAllowsPrivateSourceAddress 验证 ListRemoteRepositoriesWithAuth
// 在 allowPrivateSource=true 时放行私网来源。
func TestOnlineRESTListAllowsPrivateSourceAddress(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "r3d", "format": "maven2", "type": "hosted"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	items, err := discover.NewOnlineREST(nil).ListRemoteRepositoriesWithAuth(
		context.Background(), srv.URL, credential.SourceAuth{}, true, true,
	)
	if err != nil {
		t.Fatalf("私网来源索引（显式放行）应成功，得 %v", err)
	}
	if len(items) != 1 || items[0].Name != "r3d" {
		t.Fatalf("items = %+v", items)
	}
}

// TestOnlineRESTListRejectsPrivateSourceByDefault 验证默认（allowPrivateSource=false）
// 仍拒绝私网来源地址，确保私网放行开关默认关闭、不削弱 SSRF 防护。
func TestOnlineRESTListRejectsPrivateSourceByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)

	_, err := discover.NewOnlineREST(nil).ListRemoteRepositoriesWithAuth(
		context.Background(), srv.URL, credential.SourceAuth{}, true, false,
	)
	if !errors.Is(err, upstream.ErrUnsafeURL) {
		t.Fatalf("默认应拒绝私网来源，得 %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()))
}
