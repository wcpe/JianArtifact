package discover_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/migration/discover"
)

func TestOfflineDirFixture(t *testing.T) {
	root := t.TempDir()
	// repositories/maven-releases
	repo := filepath.Join(root, "repositories", "maven-releases")
	mustWrite(t, filepath.Join(repo, ".format"), []byte("maven2"))
	mustWrite(t, filepath.Join(repo, "content", "com/a.jar"), []byte("x"))
	// docker 应 warning
	docker := filepath.Join(root, "repositories", "docker-local")
	mustWrite(t, filepath.Join(docker, ".format"), []byte("docker"))
	mustWrite(t, filepath.Join(docker, "content", "layer"), []byte("y"))
	// raw 无 format
	raw := filepath.Join(root, "repositories", "raw-files")
	mustWrite(t, filepath.Join(raw, "content", "f.bin"), []byte("z"))

	plan, err := discover.OfflineDir{}.Discover(context.Background(), discover.Config{Path: root})
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	names := map[string]string{}
	for _, r := range plan.Repositories {
		names[r.Name] = r.Format
	}
	if names["maven-releases"] != "maven" {
		t.Errorf("maven format = %q", names["maven-releases"])
	}
	if names["raw-files"] != "raw" {
		t.Errorf("raw format = %q", names["raw-files"])
	}
	if _, ok := names["docker-local"]; ok {
		t.Error("docker 不应进入 repositories")
	}
	if len(plan.Warnings) < 1 {
		t.Error("期望 warnings")
	}
}

func TestOfflineDirMissing(t *testing.T) {
	_, err := discover.OfflineDir{}.Discover(context.Background(), discover.Config{Path: filepath.Join(t.TempDir(), "missing")})
	if _, ok := err.(*discover.ErrInvalidConfig); !ok {
		t.Fatalf("err = %T %v", err, err)
	}
	_ = os.ErrNotExist
}

func TestOfflineDirNexusBlobStore(t *testing.T) {
	root := t.TempDir()
	// content/vol-01/chap-01/{uuid}.properties + .bytes
	chap := filepath.Join(root, "content", "vol-01", "chap-01")
	if err := os.MkdirAll(chap, 0o755); err != nil {
		t.Fatal(err)
	}
	// r3d 有效
	writeBlob(t, chap, "a1", "r3d", "com/example/a.jar", false, []byte("jar-a"))
	// r3d-mixed 不应被 include r3d 命中
	writeBlob(t, chap, "a2", "r3d-mixed", "com/other/b.jar", false, []byte("jar-b"))
	// r3d deleted 应跳过
	writeBlob(t, chap, "a3", "r3d", "com/example/c.jar", true, []byte("jar-c"))
	// 缺 .bytes 应跳过
	mustWrite(t, filepath.Join(chap, "a4.properties"), []byte(
		"@Bucket.repo-name=r3d\n@BlobStore.blob-name=com/example/d.jar\n",
	))

	// 无 include → 拒绝
	_, err := discover.OfflineDir{}.Discover(context.Background(), discover.Config{Path: root})
	if _, ok := err.(*discover.ErrInvalidConfig); !ok {
		t.Fatalf("无 include 期望 ErrInvalidConfig，got %v", err)
	}

	plan, err := discover.OfflineDir{}.Discover(context.Background(), discover.Config{
		Path:                 root,
		IncludeRepositories:  []string{"r3d"},
	})
	if err != nil {
		t.Fatalf("Discover：%v", err)
	}
	if len(plan.Repositories) != 1 || plan.Repositories[0].Name != "r3d" {
		t.Fatalf("repos = %+v", plan.Repositories)
	}
	if plan.Repositories[0].EstimatedAssets != 1 {
		t.Fatalf("EstimatedAssets = %d, want 1", plan.Repositories[0].EstimatedAssets)
	}
	if plan.Repositories[0].Format != "maven" {
		t.Fatalf("format = %q", plan.Repositories[0].Format)
	}

	// 枚举与计数一致
	items, err := discover.EnumerateNexusBlobAssets(filepath.Join(root, "content"), []string{"r3d"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Path != "com/example/a.jar" {
		t.Fatalf("items = %+v", items)
	}
}

func writeBlob(t *testing.T, chap, id, repo, blobName string, deleted bool, body []byte) {
	t.Helper()
	prop := "@Bucket.repo-name=" + repo + "\n@BlobStore.blob-name=" + blobName + "\n"
	if deleted {
		prop += "deleted=true\n"
	}
	mustWrite(t, filepath.Join(chap, id+".properties"), []byte(prop))
	mustWrite(t, filepath.Join(chap, id+".bytes"), body)
}
