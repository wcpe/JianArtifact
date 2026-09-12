package runner_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/formats"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestRunnerCreatesProxyBeforeGroupFromMigrationPlan(t *testing.T) {
	mig, _, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	root := t.TempDir()
	manifest := `{"repositories":[
		{"name":"raw-source","format":"raw","type":"hosted"},
		{"name":"docker-proxy","format":"docker","type":"proxy","remoteUrl":"https://registry.example"},
		{"name":"docker-group","format":"docker","type":"group","members":["docker-proxy"]}
	]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "content", "raw-source"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "content", "raw-source", "fixture.bin"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": root},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusCompleted)

	proxy, err := repos.Get("docker-proxy")
	if err != nil {
		t.Fatalf("proxy 应先于 group 创建：%v", err)
	}
	if proxy.Type != "proxy" || proxy.Format != "docker" {
		t.Fatalf("proxy = %+v", proxy)
	}
	proxyConfig, err := proxy.DecodeConfig()
	if err != nil || proxyConfig.RemoteURL != "https://registry.example" {
		t.Fatalf("proxy config = %+v, %v", proxyConfig, err)
	}
	group, err := repos.Get("docker-group")
	if err != nil {
		t.Fatalf("group 应在成员后创建：%v", err)
	}
	if group.Type != "group" || group.Format != "docker" {
		t.Fatalf("group = %+v", group)
	}
	groupConfig, err := group.DecodeConfig()
	if err != nil || len(groupConfig.Members) != 1 || groupConfig.Members[0] != "docker-proxy" {
		t.Fatalf("group config = %+v, %v", groupConfig, err)
	}
}

func TestRunnerStartRejectsIncompleteProxyPlanBeforeWriting(t *testing.T) {
	mig, _, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	root := t.TempDir()
	manifest := `{"repositories":[{"name":"docker-proxy","format":"docker","type":"proxy"}]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": root},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("不完整 proxy 应在 start 前被拒绝，得 %v", err)
	}
	if _, err := repos.Get("docker-proxy"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("拒绝后不得写入 proxy，得 %v", err)
	}
}

func TestRunnerStartRejectsDisabledFormatBeforeWriting(t *testing.T) {
	mig, _, repos, _, _ := setup(t)
	root := t.TempDir()
	manifest := `{"repositories":[{"name":"docker-proxy","format":"docker","type":"proxy","remoteUrl":"https://registry.example"}]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": root},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); !errors.Is(err, domain.ErrFormatDisabled) {
		t.Fatalf("禁用格式应在 start 前返回 ErrFormatDisabled，得 %v", err)
	}
	if _, err := repos.Get("docker-proxy"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("拒绝后不得写入 proxy，得 %v", err)
	}
}

func TestRunnerMigratesGoProxyConfigWithoutCacheAssets(t *testing.T) {
	mig, assets, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	root := t.TempDir()
	manifest := `{"repositories":[{"name":"go-proxy","format":"gomod","type":"proxy","remoteUrl":"https://proxy.example"}]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "content", "go-proxy", "example.com", "mod", "@v"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "content", "go-proxy", "example.com", "mod", "@v", "v1.0.0.zip"), []byte("source-cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusCompleted)
	repo, err := repos.Get("go-proxy")
	if err != nil || repo.Format != "gomod" || repo.Type != "proxy" {
		t.Fatalf("Go proxy 配置迁移失败：repo=%+v err=%v", repo, err)
	}
	cfg, err := repo.DecodeConfig()
	if err != nil || cfg.RemoteURL != "https://proxy.example" {
		t.Fatalf("Go proxy 上游配置不正确：cfg=%+v err=%v", cfg, err)
	}
	if _, _, err := assets.Get("go-proxy", "example.com/mod/@v/v1.0.0.zip"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Go proxy 缓存不得迁移，得 %v", err)
	}
}
