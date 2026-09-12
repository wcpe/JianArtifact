package domain_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestMigrationServicePassesOfflineRepositoryConfigIntoPlan(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repositories", "docker-proxy", "content"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := domain.NewMigrationService(repository.NewMigrationTaskRepo(newTestDB(t)), nil)
	result, err := svc.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType: repository.MigrationSourceOfflineDir,
		SourceConfig: map[string]any{
			"path":              root,
			"repositoryFormats": map[string]any{"docker-proxy": "docker"},
			"repositoryTypes":   map[string]any{"docker-proxy": "proxy"},
			"repositoryConfigs": map[string]any{
				"docker-proxy": map[string]any{"remoteUrl": "https://registry.example"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Repositories) != 1 || result.Plan.Repositories[0].Config["remoteUrl"] != "https://registry.example" {
		t.Fatalf("plan = %+v", result.Plan)
	}
}
