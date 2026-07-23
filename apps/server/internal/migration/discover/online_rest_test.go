package discover_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
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
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	src := discover.NewOnlineREST(srv.Client())
	plan, err := src.Discover(context.Background(), discover.Config{
		URL:        srv.URL,
		Credential: "admin:secret",
	})
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	if len(plan.Repositories) != 2 {
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
	if !foundDockerWarn {
		t.Errorf("warnings = %v", plan.Warnings)
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

	items, err := discover.NewOnlineREST(srv.Client()).ListRemoteRepositories(
		context.Background(), srv.URL, "", true,
	)
	if err != nil {
		t.Fatalf("ListRemoteRepositories：%v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v, want 2 (docker 应过滤)", items)
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
	_, err := discover.NewOnlineREST(srv.Client()).Discover(context.Background(), discover.Config{
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
