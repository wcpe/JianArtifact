package domain_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestMigrationServiceDiscoverOfflineBundlePersist(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)

	root := t.TempDir()
	manifest := `{"repositories":[{"name":"raw-data","format":"raw","type":"hosted"}]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "content", "raw-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "content", "raw-data", "a.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 失败路径：坏路径不落库
	_, beforeTotal, _ := svc.List(100, 0)
	_, err := svc.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": filepath.Join(root, "missing")},
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("坏路径 err = %v", err)
	}
	_, afterTotal, _ := svc.List(100, 0)
	if afterTotal != beforeTotal {
		t.Fatal("失败不应落库")
	}

	result, err := svc.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": root},
	})
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	if result.Task.Status != repository.MigrationStatusPlanned {
		t.Fatalf("status = %s", result.Task.Status)
	}
	if result.Task.ID <= 0 {
		t.Fatal("期望 taskId")
	}
	if len(result.Plan.Repositories) != 1 {
		t.Fatalf("plan repos = %+v", result.Plan.Repositories)
	}
	// 未 start：仍 planned
	got, _ := svc.Get(result.Task.ID)
	if got.Status != repository.MigrationStatusPlanned {
		t.Fatalf("discover 后不应 running，得 %s", got.Status)
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(got.PlanJSON), &plan); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationServiceDiscoverOnlineREST(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "maven-releases", "format": "maven2", "type": "hosted"},
			{"name": "docker-hub", "format": "docker", "type": "proxy"},
		})
	})
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]string{"p": "1"}}, "continuationToken": ""})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	db := newTestDB(t)
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)
	result, err := svc.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"url": srv.URL},
	})
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	if len(result.Plan.Repositories) != 1 {
		t.Fatalf("期望仅 maven，得 %+v", result.Plan.Repositories)
	}
	if result.Task.Status != repository.MigrationStatusPlanned {
		t.Fatal(result.Task.Status)
	}
}
