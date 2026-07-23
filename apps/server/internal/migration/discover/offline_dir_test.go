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
