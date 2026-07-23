package runner_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestRunnerOnlineRESTWithIncludeFilter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "raw-small", "format": "raw", "type": "hosted"},
			{"name": "huge-repo", "format": "maven2", "type": "hosted"},
		})
	})
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		repo := r.URL.Query().Get("repository")
		// downloadUrl 指向本测试服务器
		host := "http://" + r.Host
		items := []map[string]string{}
		switch repo {
		case "raw-small":
			items = []map[string]string{
				{"path": "a.bin", "downloadUrl": host + "/dl/a.bin", "contentType": "application/octet-stream"},
			}
		case "huge-repo":
			items = []map[string]string{
				{"path": "big.jar", "downloadUrl": host + "/dl/big.jar", "contentType": "application/java-archive"},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "continuationToken": ""})
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/a.bin":
			_, _ = w.Write([]byte("payload-a"))
		case "/dl/big.jar":
			_, _ = w.Write([]byte("should-not-download"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mig, assets, _, _, _ := setup(t)

	// discover 仅 include raw-small
	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType: repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{
			"url":                 srv.URL,
			"includeRepositories": []any{"raw-small"},
		},
	})
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	if len(result.Plan.Repositories) != 1 || result.Plan.Repositories[0].Name != "raw-small" {
		t.Fatalf("plan 应仅含 raw-small：%+v", result.Plan.Repositories)
	}

	if _, err := mig.Start(result.Task.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := mig.Get(result.Task.ID)
		if task.Status == repository.MigrationStatusCompleted {
			break
		}
		if task.Status == repository.MigrationStatusFailed {
			t.Fatalf("失败：%s", task.ErrorMessage.String)
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, _ := mig.Get(result.Task.ID)
	if task.Status != repository.MigrationStatusCompleted {
		t.Fatalf("status=%s", task.Status)
	}

	_, rc, err := assets.Get("raw-small", "a.bin")
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "payload-a" {
		t.Fatalf("内容 = %q", body)
	}
	// huge-repo 不应被创建
	if _, _, err := assets.Get("huge-repo", "big.jar"); err == nil {
		t.Fatal("不应迁移 huge-repo")
	}
}
