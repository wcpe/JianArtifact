package discover_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

func TestOfflineDirPreservesTrustedProxyAndGroupConfig(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"docker-proxy", "docker-group"} {
		if err := os.MkdirAll(filepath.Join(root, "repositories", name, "content"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := (discover.OfflineDir{}).Discover(context.Background(), discover.Config{
		Path: root,
		RepositoryFormats: map[string]string{
			"docker-proxy": "docker",
			"docker-group": "docker",
		},
		RepositoryTypes: map[string]string{
			"docker-proxy": "proxy",
			"docker-group": "group",
		},
		RepositoryConfigs: map[string]discover.TargetRepositoryConfig{
			"docker-proxy": {RemoteURL: "https://registry.example"},
			"docker-group": {Members: []string{"docker-proxy"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]discover.PlanRepository{}
	for _, item := range plan.Repositories {
		byName[item.Name] = item
	}
	if got := byName["docker-proxy"]; got.MigrationMode != "proxy_config" || got.Config["remoteUrl"] != "https://registry.example" {
		t.Fatalf("proxy plan = %+v", got)
	}
	if got := byName["docker-group"]; got.MigrationMode != "group_config" || len(got.Config["members"].([]string)) != 1 {
		t.Fatalf("group plan = %+v", got)
	}
}

func TestOnlineRESTPreservesWhitelistedProxyAndGroupConfig(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "docker-proxy", "format": "docker", "type": "proxy"},
			{"name": "docker-group", "format": "docker", "type": "group"},
		})
	})
	mux.HandleFunc("/service/rest/v1/repositories/docker/proxy/docker-proxy", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"proxy":{"remoteUrl":"https://registry.example"}}`))
	})
	mux.HandleFunc("/service/rest/v1/repositories/docker/group/docker-group", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"group":{"memberNames":["docker-proxy"]}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	plan, err := discover.NewOnlineREST(upstream.NewTestClient(time.Second)).Discover(context.Background(), discover.Config{
		URL:                 srv.URL,
		SourceRef:           "NEXUS_TEST",
		IncludeRepositories: []string{"docker-proxy", "docker-group"},
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]discover.PlanRepository{}
	for _, item := range plan.Repositories {
		byName[item.Name] = item
	}
	if got := byName["docker-proxy"]; got.MigrationMode != "proxy_config" || got.Config["remoteUrl"] != "https://registry.example" {
		t.Fatalf("proxy plan = %+v", got)
	}
	if got := byName["docker-group"]; got.MigrationMode != "group_config" || len(got.Config["members"].([]string)) != 1 {
		t.Fatalf("group plan = %+v", got)
	}
}
