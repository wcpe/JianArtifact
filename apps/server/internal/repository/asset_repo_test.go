package repository_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newTestRepos 打开临时 SQLite、迁移并返回资产与仓库两个 Repo。
func newTestRepos(t *testing.T) (*repository.AssetRepo, *repository.RepoRepo) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "asset.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return repository.NewAssetRepo(db), repository.NewRepoRepo(db)
}

func TestAssetUpsertInsertThenOverwrite(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("raw-hosted", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}

	// 首次插入。
	if err := assets.Upsert(repoID, "a/b.txt", "hash1", 11, "text/plain"); err != nil {
		t.Fatalf("首次 Upsert：%v", err)
	}
	got, err := assets.GetByPath(repoID, "a/b.txt")
	if err != nil {
		t.Fatalf("GetByPath：%v", err)
	}
	if got.BlobHash != "hash1" || got.Size != 11 || got.ContentType != "text/plain" {
		t.Fatalf("首次插入内容不符：%+v", got)
	}

	// 覆盖写同路径。
	if err := assets.Upsert(repoID, "a/b.txt", "hash2", 22, "application/json"); err != nil {
		t.Fatalf("覆盖 Upsert：%v", err)
	}
	got, err = assets.GetByPath(repoID, "a/b.txt")
	if err != nil {
		t.Fatalf("GetByPath（覆盖后）：%v", err)
	}
	if got.BlobHash != "hash2" || got.Size != 22 || got.ContentType != "application/json" {
		t.Fatalf("覆盖写未生效：%+v", got)
	}
}

func TestAssetGetByPathNotFound(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("raw-hosted", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := assets.GetByPath(repoID, "missing"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("应返回 ErrNotFound，实际：%v", err)
	}
}

func TestAssetDeleteByPath(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("raw-hosted", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := assets.Upsert(repoID, "x.bin", "h", 1, "application/octet-stream"); err != nil {
		t.Fatalf("Upsert：%v", err)
	}
	if err := assets.DeleteByPath(repoID, "x.bin"); err != nil {
		t.Fatalf("DeleteByPath：%v", err)
	}
	if _, err := assets.GetByPath(repoID, "x.bin"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("删除后应 ErrNotFound，实际：%v", err)
	}
	// 删除不存在的路径应返回 ErrNotFound。
	if err := assets.DeleteByPath(repoID, "nope"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("删除不存在应 ErrNotFound，实际：%v", err)
	}
}
