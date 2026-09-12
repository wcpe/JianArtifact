package repository_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func newUploadRepo(t *testing.T) *repository.BackupUploadRepo {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "upload.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return repository.NewBackupUploadRepo(db)
}

func TestBackupUploadRepoCRUD(t *testing.T) {
	repo := newUploadRepo(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rec := repository.BackupUpload{
		UploadID:   "up-test-1",
		FileName:   "pkg.tar.gz",
		TotalBytes: 1024,
		ChunkSize:  8 << 20,
		Status:     repository.UploadStatusInitialized,
		Operator:   "tester",
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano),
	}
	if err := repo.Create(rec); err != nil {
		t.Fatalf("Create：%v", err)
	}
	got, err := repo.Get("up-test-1")
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	if got.FileName != "pkg.tar.gz" || got.TotalBytes != 1024 {
		t.Fatalf("字段不符：%+v", got)
	}
	if _, err := repo.Get("ghost"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("不存在应返回 ErrNotFound，得 %v", err)
	}
	if err := repo.UpdateStatus("up-test-1", repository.UploadStatusReceiving); err != nil {
		t.Fatalf("UpdateStatus：%v", err)
	}
	got, _ = repo.Get("up-test-1")
	if got.Status != repository.UploadStatusReceiving {
		t.Fatalf("状态未更新：%s", got.Status)
	}
	n, err := repo.Count()
	if err != nil {
		t.Fatalf("Count：%v", err)
	}
	if n != 1 {
		t.Fatalf("Count = %d，期望 1", n)
	}
	if err := repo.Delete("up-test-1"); err != nil {
		t.Fatalf("Delete：%v", err)
	}
	if _, err := repo.Get("up-test-1"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("删除后应为 ErrNotFound，得 %v", err)
	}
}

func TestBackupUploadRepoAddChunkIdempotent(t *testing.T) {
	repo := newUploadRepo(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rec := repository.BackupUpload{
		UploadID:   "up-chunk-1",
		FileName:   "pkg.tar.gz",
		TotalBytes: 1024,
		ChunkSize:  8 << 20,
		Status:     repository.UploadStatusReceiving,
		Operator:   "tester",
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}
	if err := repo.Create(rec); err != nil {
		t.Fatalf("Create：%v", err)
	}
	// 首次写分片 0（size=512）。
	if err := repo.AddChunk(repository.BackupUploadChunk{UploadID: "up-chunk-1", ChunkIndex: 0, Size: 512, SHA256: "aaa"}); err != nil {
		t.Fatalf("AddChunk：%v", err)
	}
	// 覆盖同序号分片 0（size=256）——应幂等覆盖而非新增一行。
	if err := repo.AddChunk(repository.BackupUploadChunk{UploadID: "up-chunk-1", ChunkIndex: 0, Size: 256, SHA256: "bbb"}); err != nil {
		t.Fatalf("AddChunk 覆盖：%v", err)
	}
	if err := repo.AddChunk(repository.BackupUploadChunk{UploadID: "up-chunk-1", ChunkIndex: 1, Size: 512, SHA256: "ccc"}); err != nil {
		t.Fatalf("AddChunk 1：%v", err)
	}
	chunks, err := repo.ListChunks("up-chunk-1")
	if err != nil {
		t.Fatalf("ListChunks：%v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("分片数 = %d，期望 2（覆盖不应新增）", len(chunks))
	}
	// 按序号升序且覆盖生效。
	if chunks[0].ChunkIndex != 0 || chunks[0].Size != 256 || chunks[0].SHA256 != "bbb" {
		t.Fatalf("分片 0 未被覆盖：%+v", chunks[0])
	}
	if chunks[1].ChunkIndex != 1 || chunks[1].Size != 512 {
		t.Fatalf("分片 1 异常：%+v", chunks[1])
	}
}

func TestBackupUploadRepoListChunksAscending(t *testing.T) {
	repo := newUploadRepo(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rec := repository.BackupUpload{
		UploadID:   "up-order-1",
		FileName:   "pkg.tar.gz",
		TotalBytes: 1024,
		ChunkSize:  8 << 20,
		Status:     repository.UploadStatusReceiving,
		Operator:   "tester",
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}
	if err := repo.Create(rec); err != nil {
		t.Fatalf("Create：%v", err)
	}
	// 乱序插入。
	for _, idx := range []int{3, 1, 2, 0} {
		if err := repo.AddChunk(repository.BackupUploadChunk{UploadID: "up-order-1", ChunkIndex: idx, Size: 1}); err != nil {
			t.Fatalf("AddChunk：%v", err)
		}
	}
	chunks, err := repo.ListChunks("up-order-1")
	if err != nil {
		t.Fatalf("ListChunks：%v", err)
	}
	if len(chunks) != 4 {
		t.Fatalf("分片数 = %d，期望 4", len(chunks))
	}
	for i, c := range chunks {
		if c.ChunkIndex != i {
			t.Fatalf("分片未按序号升序：%+v", chunks)
		}
	}
}

func TestBackupUploadRepoDeleteExpiredOnly(t *testing.T) {
	repo := newUploadRepo(t)
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	mk := func(id, expires string) {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_ = repo.Create(repository.BackupUpload{
			UploadID: id, FileName: "p", TotalBytes: 1, ChunkSize: 8 << 20,
			Status: repository.UploadStatusInitialized, Operator: "t",
			CreatedAt: now, UpdatedAt: now, ExpiresAt: expires,
		})
		_ = repo.AddChunk(repository.BackupUploadChunk{UploadID: id, ChunkIndex: 0, Size: 1})
	}
	mk("up-expired", past)
	mk("up-live", future)

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	n, err := repo.DeleteExpired(nowStr)
	if err != nil {
		t.Fatalf("DeleteExpired：%v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteExpired 删除数 = %d，期望 1", n)
	}
	if _, err := repo.Get("up-expired"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("过期记录应被删，得 %v", err)
	}
	if _, err := repo.Get("up-live"); err != nil {
		t.Fatalf("未过期记录不应被删：%v", err)
	}
	// 外键级联：过期记录的分片元数据也应被清。
	chunks, err := repo.ListChunks("up-expired")
	if err != nil {
		t.Fatalf("ListChunks：%v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("过期记录的分片应随级联删除，剩 %d", len(chunks))
	}
}
