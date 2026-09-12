package discover_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
)

func TestOfflineBundleWithManifest(t *testing.T) {
	root := t.TempDir()
	// manifest
	manifest := `{"repositories":[
		{"name":"maven-releases","format":"maven2","type":"hosted"},
		{"name":"npm-hosted","format":"npm","type":"hosted"},
		{"name":"docker-hub","format":"docker","type":"proxy","remoteUrl":"https://registry.example"}
	]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// content
	mustWrite(t, filepath.Join(root, "content", "maven-releases", "a.jar"), []byte("jar"))
	mustWrite(t, filepath.Join(root, "content", "maven-releases", "b.pom"), []byte("pom"))
	mustWrite(t, filepath.Join(root, "content", "npm-hosted", "pkg.tgz"), []byte("tgz"))

	plan, err := discover.OfflineBundle{}.Discover(context.Background(), discover.Config{Path: root})
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	if len(plan.Repositories) != 3 {
		t.Fatalf("repos = %d，期望 3", len(plan.Repositories))
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("完整 proxy 配置不应产生 warning：%v", plan.Warnings)
	}
	byName := map[string]discover.PlanRepository{}
	for _, r := range plan.Repositories {
		byName[r.Name] = r
	}
	if byName["maven-releases"].Format != "maven" || byName["maven-releases"].EstimatedAssets != 2 {
		t.Errorf("maven = %+v", byName["maven-releases"])
	}
	if byName["npm-hosted"].Format != "npm" || byName["npm-hosted"].EstimatedAssets != 1 {
		t.Errorf("npm = %+v", byName["npm-hosted"])
	}
	if byName["docker-hub"].MigrationMode != "proxy_config" || byName["docker-hub"].EstimatedAssets != 0 || byName["docker-hub"].Config["remoteUrl"] != "https://registry.example" {
		t.Errorf("docker proxy = %+v", byName["docker-hub"])
	}
}

func TestOfflineBundleWithoutManifest(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "content", "raw-data", "f.bin"), []byte("x"))
	plan, err := discover.OfflineBundle{}.Discover(context.Background(), discover.Config{Path: root})
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	if len(plan.Repositories) != 1 || plan.Repositories[0].Format != "raw" {
		t.Fatalf("plan = %+v", plan)
	}
	if len(plan.Warnings) == 0 {
		t.Error("期望无 manifest warning")
	}
}

func TestOfflineBundleMissingPath(t *testing.T) {
	_, err := discover.OfflineBundle{}.Discover(context.Background(), discover.Config{Path: filepath.Join(t.TempDir(), "nope")})
	if _, ok := err.(*discover.ErrInvalidConfig); !ok {
		t.Fatalf("err = %v，期望 ErrInvalidConfig", err)
	}
}

func TestOfflineBundleGroupAndGoMapping(t *testing.T) {
	root := t.TempDir()
	manifest := `{"repositories":[
		{"name":"go-hosted","format":"gomod","type":"hosted"},
		{"name":"go-proxy","format":"go","type":"proxy","remoteUrl":"https://proxy.example"},
		{"name":"raw","format":"raw","type":"hosted"},
		{"name":"all","format":"raw","type":"group","members":["raw"]}
	]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := discover.OfflineBundle{}.Discover(context.Background(), discover.Config{Path: root})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]discover.PlanRepository{}
	for _, item := range plan.Repositories {
		byName[item.Name] = item
	}
	if _, ok := byName["go-hosted"]; ok {
		t.Fatal("Go hosted 不应进入可执行计划")
	}
	if byName["go-proxy"].MigrationMode != "proxy_config" {
		t.Fatalf("Go proxy = %+v", byName["go-proxy"])
	}
	if byName["all"].MigrationMode != "group_config" {
		t.Fatalf("group = %+v", byName["all"])
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
