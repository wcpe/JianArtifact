package runner_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/migration/runner"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func setup(t *testing.T) (*domain.MigrationService, *domain.AssetService, *domain.RepositoryService, *repository.MigrationTaskRepo, *persistence.DB) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	repoRepo := repository.NewRepoRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	taskRepo := repository.NewMigrationTaskRepo(db)
	blobs := blobstore.NewStore(filepath.Join(t.TempDir(), "blobs"))
	repoSvc := domain.NewRepositoryService(repoRepo, repository.NewAclRepo(db), assetRepo, domain.NewSettingService(repository.NewSettingRepo(db)), repository.NewUserRepo(db))
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, nil)
	r := runner.New(
		runner.TaskStoreAdapter{Repo: taskRepo},
		runner.AssetServiceAdapter{Assets: assetSvc, Repos: repoRepo, AssetR: assetRepo},
		runner.RepoAdminAdapter{Repos: repoSvc},
	)
	mig := domain.NewMigrationService(taskRepo, r)
	return mig, assetSvc, repoSvc, taskRepo, db
}

func writeBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	manifest := `{"repositories":[{"name":"raw-data","format":"raw","type":"hosted"}]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "content", "raw-data", "hello.bin")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("hello-migration"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func waitStatus(t *testing.T, svc *domain.MigrationService, id int64, want string) *repository.MigrationTask {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := svc.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == want {
			return task
		}
		if task.Status == repository.MigrationStatusFailed && want != repository.MigrationStatusFailed {
			t.Fatalf("任务失败：%s", task.ErrorMessage.String)
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, _ := svc.Get(id)
	t.Fatalf("超时等待 status=%s，当前=%s", want, task.Status)
	return nil
}

func TestRunnerOfflineBundleEndToEnd(t *testing.T) {
	mig, assets, _, _, _ := setup(t)
	root := writeBundle(t)

	// discover 未 start：目标无资产
	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": root},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := assets.Get("raw-data", "hello.bin"); err == nil {
		t.Fatal("discover 后不应有资产")
	}

	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	task := waitStatus(t, mig, result.Task.ID, repository.MigrationStatusCompleted)
	if task.Status != repository.MigrationStatusCompleted {
		t.Fatal(task.Status)
	}

	meta, rc, err := assets.Get("raw-data", "hello.bin")
	if err != nil {
		t.Fatalf("Get 资产：%v", err)
	}
	defer func() { _ = rc.Close() }()
	body, _ := io.ReadAll(rc)
	if string(body) != "hello-migration" {
		t.Fatalf("内容 = %q", body)
	}
	if meta.Size != int64(len("hello-migration")) {
		t.Errorf("size = %d", meta.Size)
	}
}

func TestRunnerConflictSkip(t *testing.T) {
	mig, assets, repos, _, _ := setup(t)
	root := writeBundle(t)
	// 预先写入冲突内容
	if _, err := repos.Create("raw-data", "raw", "hosted", "private", repository.RepositoryConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := assets.Put("raw-data", "hello.bin", bytes.NewReader([]byte("OLD")), ""); err != nil {
		t.Fatal(err)
	}

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:     repository.MigrationSourceOfflineBundle,
		SourceConfig:   map[string]any{"path": root},
		ConflictPolicy: repository.MigrationConflictSkip,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusCompleted)
	_, rc, err := assets.Get("raw-data", "hello.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	body, _ := io.ReadAll(rc)
	if string(body) != "OLD" {
		t.Fatalf("skip 应保留 OLD，得 %q", body)
	}
}

func TestRunnerConflictOverwrite(t *testing.T) {
	mig, assets, repos, _, _ := setup(t)
	root := writeBundle(t)
	if _, err := repos.Create("raw-data", "raw", "hosted", "private", repository.RepositoryConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := assets.Put("raw-data", "hello.bin", bytes.NewReader([]byte("OLD")), ""); err != nil {
		t.Fatal(err)
	}
	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:     repository.MigrationSourceOfflineBundle,
		SourceConfig:   map[string]any{"path": root},
		ConflictPolicy: repository.MigrationConflictOverwrite,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusCompleted)
	_, rc, err := assets.Get("raw-data", "hello.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	body, _ := io.ReadAll(rc)
	if string(body) != "hello-migration" {
		t.Fatalf("overwrite 应得新内容，得 %q", body)
	}
}

func TestRunnerConflictFail(t *testing.T) {
	mig, assets, repos, _, _ := setup(t)
	root := writeBundle(t)
	if _, err := repos.Create("raw-data", "raw", "hosted", "private", repository.RepositoryConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := assets.Put("raw-data", "hello.bin", bytes.NewReader([]byte("OLD")), ""); err != nil {
		t.Fatal(err)
	}
	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:     repository.MigrationSourceOfflineBundle,
		SourceConfig:   map[string]any{"path": root},
		ConflictPolicy: repository.MigrationConflictFail,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusFailed)
}

func TestRunnerFinalizeDelta(t *testing.T) {
	mig, assets, _, _, _ := setup(t)
	root := writeBundle(t)
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

	// 源新增文件
	extra := filepath.Join(root, "content", "raw-data", "extra.bin")
	if err := os.WriteFile(extra, []byte("extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Finalize(context.Background(), result.Task.ID); err != nil {
		t.Fatalf("Finalize：%v", err)
	}
	_, rc, err := assets.Get("raw-data", "extra.bin")
	if err != nil {
		t.Fatalf("增量资产：%v", err)
	}
	defer func() { _ = rc.Close() }()
	body, _ := io.ReadAll(rc)
	if string(body) != "extra" {
		t.Fatalf("内容 = %q", body)
	}
	task, _ := mig.Get(result.Task.ID)
	if !bytes.Contains([]byte(task.ReportJSON), []byte("delta")) {
		t.Errorf("report 应含 delta：%s", task.ReportJSON)
	}
}

func TestFailInterruptedNoAutoResume(t *testing.T) {
	mig, _, _, taskRepo, _ := setup(t)
	root := writeBundle(t)
	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{
		SourceType:   repository.MigrationSourceOfflineBundle,
		SourceConfig: map[string]any{"path": root},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃：手动标 running
	if err := taskRepo.UpdateStatus(result.Task.ID, repository.MigrationStatusRunning, nil, true, false); err != nil {
		t.Fatal(err)
	}
	n, err := mig.FailInterruptedRunning()
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got, _ := mig.Get(result.Task.ID)
	if got.Status != repository.MigrationStatusFailed {
		t.Fatalf("status=%s", got.Status)
	}
	// 未 resume 前无资产
	// resume 后续传
	if _, err := mig.Resume(result.Task.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusCompleted)
}
