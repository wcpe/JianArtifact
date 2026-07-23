package runner_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestStartWithMultiSelectRepos 离线包多仓发现后 start 仅迁勾选仓库。
func TestStartWithMultiSelectRepos(t *testing.T) {
	mig, assets, _, _, _ := setup(t)
	root := t.TempDir()
	manifest := `{"repositories":[
		{"name":"repo-a","format":"raw","type":"hosted"},
		{"name":"repo-b","format":"raw","type":"hosted"}
	]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, "content", "repo-a", "a.bin"), []byte("AAA"))
	mustWriteFile(t, filepath.Join(root, "content", "repo-b", "b.bin"), []byte("BBB"))

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": root},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Repositories) != 2 {
		t.Fatalf("plan repos = %d", len(result.Plan.Repositories))
	}

	// 只选 repo-a
	if _, err := mig.Start(result.Task.ID, []string{"repo-a"}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, mig, result.Task.ID)

	_, rc, err := assets.Get("repo-a", "a.bin")
	if err != nil {
		t.Fatalf("repo-a：%v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "AAA" {
		t.Fatalf("内容 = %q", body)
	}
	if _, _, err := assets.Get("repo-b", "b.bin"); err == nil {
		t.Fatal("repo-b 不应被迁移")
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitDone(t *testing.T, mig *domain.MigrationService, id int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := mig.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == repository.MigrationStatusCompleted {
			return
		}
		if task.Status == repository.MigrationStatusFailed {
			t.Fatalf("失败：%s", task.ErrorMessage.String)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("超时")
}
