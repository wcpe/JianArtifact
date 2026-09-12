package domain

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newUploadFixture 装配一个完整的上传 + 导入 + 恢复链路夹具（各自独立数据目录）。
// 返回上传服务、导入服务、导入 Repo、底层 DB、数据根目录。
func newUploadFixture(t *testing.T) (*BackupUploadService, *BackupImportService, *repository.BackupImportRepo, *persistence.DB, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")
	db, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate：%v", err)
	}
	blobsDir := filepath.Join(dir, "blobs")
	_ = blobstore.NewStore(blobsDir) // 确保目录存在（恢复侧会按需写入）
	restore := NewRestoreService(db, dir, dbPath, blobsDir)
	importRepo := repository.NewBackupImportRepo(db)
	importSvc := NewBackupImportService(importRepo, restore, dir, nil) // 本地导入不经过出站 client
	uploadRepo := repository.NewBackupUploadRepo(db)
	uploadSvc := NewBackupUploadService(uploadRepo, importSvc, dir)
	return uploadSvc, importSvc, importRepo, db, dir
}

// chunkData 把完整字节切成 <= chunkSize 的若干片。
func chunkData(t *testing.T, data []byte, chunkSize int) [][]byte {
	t.Helper()
	var chunks [][]byte
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunks = append(chunks, data[i:end])
	}
	return chunks
}

// waitImport 轮询导入记录直到终态；失败则 t.Fatal。
func waitImport(t *testing.T, repo *repository.BackupImportRepo, id string) repository.BackupImport {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := repo.Get(id)
		if err == nil && rec.Status == repository.ImportStatusPendingRestart {
			return *rec
		}
		if err == nil && rec.Status == repository.ImportStatusFailed {
			t.Fatalf("导入失败：%+v", rec)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("导入未在超时内完成（id=%s）", id)
	return repository.BackupImport{}
}

// TestBackupUploadRoundTrip 覆盖 init → 逐片 PutChunk → Get 反映已传分片 → Complete 组装成包并触发本地导入。
func TestBackupUploadRoundTrip(t *testing.T) {
	ctx := context.Background()
	uploadSvc, _, importRepo, _, _ := newUploadFixture(t)

	// 造一个真实备份包作为上传源。
	src := newBackupFixture(t)
	src.seedAssets(t, "alpha", "beta", "gamma")
	rec, err := src.svc.Generate(ctx, CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	pkg := src.svc.PackagePath(rec.PackageID)
	data, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatalf("读取源包：%v", err)
	}

	initRes, err := uploadSvc.Init(ctx, InitUploadOptions{
		FileName: "pkg.tar.gz", TotalBytes: int64(len(data)), SHA256: "", Operator: "tester",
	})
	if err != nil {
		t.Fatalf("Init：%v", err)
	}
	if initRes.ChunkSize != uploadChunkSize {
		t.Fatalf("返回的分片大小 = %d，期望 %d", initRes.ChunkSize, uploadChunkSize)
	}
	uploadID := initRes.Upload.UploadID

	chunks := chunkData(t, data, int(uploadChunkSize))
	for i, c := range chunks {
		if _, err := uploadSvc.PutChunk(ctx, uploadID, i, bytes.NewReader(c), int64(len(c))); err != nil {
			t.Fatalf("PutChunk(%d)：%v", i, err)
		}
	}

	// Get 应反映已上传的全部分片。
	view, err := uploadSvc.Get(ctx, uploadID)
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	if len(view.UploadedChunks) != len(chunks) {
		t.Fatalf("UploadedChunks = %v，期望 %d 片", view.UploadedChunks, len(chunks))
	}
	for i := range chunks {
		if !containsInt(view.UploadedChunks, i) {
			t.Fatalf("缺片 %d：%+v", i, view.UploadedChunks)
		}
	}

	// 删掉磁盘上某一片后，Get 应反映缺片（证明以磁盘为准，而非数据库）。
	// 小包可能只有 1 片，故优先删序号 1，否则删唯一的 0 片。
	delIdx := 1
	if len(chunks) <= 1 {
		delIdx = 0
	}
	if err := os.Remove(uploadSvc.chunkPath(uploadID, delIdx)); err != nil {
		t.Fatalf("删除分片 %d：%v", delIdx, err)
	}
	view, _ = uploadSvc.Get(ctx, uploadID)
	if containsInt(view.UploadedChunks, delIdx) {
		t.Fatalf("删盘后 Get 仍报存在分片 %d：%+v", delIdx, view.UploadedChunks)
	}
	// 补回该分片，恢复完整。
	if _, err := uploadSvc.PutChunk(ctx, uploadID, delIdx, bytes.NewReader(chunks[delIdx]), int64(len(chunks[delIdx]))); err != nil {
		t.Fatalf("补 PutChunk(%d)：%v", delIdx, err)
	}

	// Complete：组装归档 + 触发本地导入。
	imp, err := uploadSvc.Complete(ctx, uploadID, "", false, false)
	if err != nil {
		t.Fatalf("Complete：%v", err)
	}
	_ = waitImport(t, importRepo, imp.ImportID)

	// 组装包应与源包字节一致，且能被 archive.Open 打开。
	assembled := uploadSvc.packagePath(uploadID)
	got, err := os.ReadFile(assembled)
	if err != nil {
		t.Fatalf("读取组装包：%v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("组装包与源包字节不一致（%d vs %d）", len(got), len(data))
	}
	if _, _, err := archive.Open(assembled); err != nil {
		t.Fatalf("archive.Open 组装包失败：%v", err)
	}
}

// TestBackupUploadValidation 覆盖片号越界 / 单片超额 / 累计超额 → ErrValidation 且不留半成品分片。
func TestBackupUploadValidation(t *testing.T) {
	ctx := context.Background()
	uploadSvc, _, _, _, _ := newUploadFixture(t)
	// totalBytes=10，chunkSize 服务端固定 8MiB → 期望 ceil(10/8MiB)=1 片。
	initRes, err := uploadSvc.Init(ctx, InitUploadOptions{FileName: "p", TotalBytes: 10, Operator: "t"})
	if err != nil {
		t.Fatalf("Init：%v", err)
	}
	id := initRes.Upload.UploadID

	// 片号越界（只允许 0）。
	if _, err := uploadSvc.PutChunk(ctx, id, 1, bytes.NewReader([]byte("x")), 1); !errorsIsValidation(err) {
		t.Fatalf("越界分片应 ErrValidation，得 %v", err)
	}
	assertNoChunkFile(t, uploadSvc, id, 1)

	// 单片超额。
	if _, err := uploadSvc.PutChunk(ctx, id, 0, bytes.NewReader(make([]byte, uploadChunkSize+1)), uploadChunkSize+1); !errorsIsValidation(err) {
		t.Fatalf("单片超额应 ErrValidation，得 %v", err)
	}
	assertNoChunkFile(t, uploadSvc, id, 0)

	// 正常片 0（size=8 <= totalBytes=10）。
	if _, err := uploadSvc.PutChunk(ctx, id, 0, bytes.NewReader([]byte("12345678")), 8); err != nil {
		t.Fatalf("PutChunk(0) 正常应成功：%v", err)
	}
	// 累计超额：再传一片（即便序号不存在也应先被累计护栏拒）。
	if _, err := uploadSvc.PutChunk(ctx, id, 1, bytes.NewReader([]byte("x")), 2); !errorsIsValidation(err) {
		t.Fatalf("累计超额应 ErrValidation，得 %v", err)
	}
	assertNoChunkFile(t, uploadSvc, id, 1)
}

// TestBackupUploadCompleteMissingChunk 缺片时 Complete 应返回带缺失序号的明确错误。
func TestBackupUploadCompleteMissingChunk(t *testing.T) {
	ctx := context.Background()
	uploadSvc, _, _, _, _ := newUploadFixture(t)
	data := []byte("0123456789ABCDEF") // 16 字节，按 8MiB 切片 => 1 片
	initRes, err := uploadSvc.Init(ctx, InitUploadOptions{FileName: "p", TotalBytes: int64(len(data)), Operator: "t"})
	if err != nil {
		t.Fatalf("Init：%v", err)
	}
	id := initRes.Upload.UploadID
	// 故意不上传任何分片，直接 Complete。
	_, err = uploadSvc.Complete(ctx, id, "", false, false)
	if err == nil {
		t.Fatal("缺片 Complete 应失败")
	}
	if !errorsIsValidation(err) {
		t.Fatalf("缺片应 ErrValidation，得 %v", err)
	}
	if !containsSubstring(err.Error(), "缺失") {
		t.Fatalf("错误应含缺失序号提示：%v", err)
	}
}

// TestBackupUploadCompleteSHA256Mismatch clientSHA256 不符应明确错误且删除半成品。
func TestBackupUploadCompleteSHA256Mismatch(t *testing.T) {
	ctx := context.Background()
	uploadSvc, _, _, _, _ := newUploadFixture(t)
	data := []byte("hello-jian-artifact-backup-package-content-here")
	initRes, err := uploadSvc.Init(ctx, InitUploadOptions{FileName: "p", TotalBytes: int64(len(data)), Operator: "t"})
	if err != nil {
		t.Fatalf("Init：%v", err)
	}
	id := initRes.Upload.UploadID
	chunks := chunkData(t, data, int(uploadChunkSize))
	for i, c := range chunks {
		if _, err := uploadSvc.PutChunk(ctx, id, i, bytes.NewReader(c), int64(len(c))); err != nil {
			t.Fatalf("PutChunk：%v", err)
		}
	}
	// 故意给错的 clientSHA256。
	_, err = uploadSvc.Complete(ctx, id, "deadbeef", false, false)
	if err == nil {
		t.Fatal("sha 不符应失败")
	}
	if !errorsIsValidation(err) {
		t.Fatalf("sha 不符应 ErrValidation，得 %v", err)
	}
	// 半成品组装包应被删除。
	if _, statErr := os.Stat(uploadSvc.packagePath(id)); !os.IsNotExist(statErr) {
		t.Fatal("sha 不符后组装包应被删除")
	}
}

// TestBackupUploadAbort 清磁盘；TestBackupUploadReconcileExpired 清过期；过期会话 PutChunk 被拒。
func TestBackupUploadAbortAndExpiry(t *testing.T) {
	ctx := context.Background()
	uploadSvc, _, _, db, _ := newUploadFixture(t)
	data := []byte("some-bytes-for-abort-test-1234567890")
	initRes, err := uploadSvc.Init(ctx, InitUploadOptions{FileName: "p", TotalBytes: int64(len(data)), Operator: "t"})
	if err != nil {
		t.Fatalf("Init：%v", err)
	}
	id := initRes.Upload.UploadID
	chunks := chunkData(t, data, int(uploadChunkSize))
	for i, c := range chunks {
		if _, err := uploadSvc.PutChunk(ctx, id, i, bytes.NewReader(c), int64(len(c))); err != nil {
			t.Fatalf("PutChunk：%v", err)
		}
	}
	if err := uploadSvc.Abort(ctx, id); err != nil {
		t.Fatalf("Abort：%v", err)
	}
	if _, statErr := os.Stat(uploadSvc.sessionDir(id)); !os.IsNotExist(statErr) {
		t.Fatal("Abort 后应删除会话磁盘目录")
	}

	// 过期会话：Init 后把 expires_at 拨到过去，PutChunk 应被拒。
	initRes2, err := uploadSvc.Init(ctx, InitUploadOptions{FileName: "p2", TotalBytes: 10, Operator: "t"})
	if err != nil {
		t.Fatalf("Init2：%v", err)
	}
	id2 := initRes2.Upload.UploadID
	if _, e := db.Exec(`UPDATE backup_upload SET expires_at=? WHERE upload_id=?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), id2); e != nil {
		t.Fatalf("拨过期：%v", e)
	}
	if _, err := uploadSvc.PutChunk(ctx, id2, 0, bytes.NewReader([]byte("12345678")), 8); !errorsIsValidation(err) {
		t.Fatalf("过期会话 PutChunk 应被拒，得 %v", err)
	}

	// ReconcileExpired 清理过期会话（磁盘 + 记录）。
	n, err := uploadSvc.ReconcileExpired(time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("ReconcileExpired：%v", err)
	}
	if n < 1 {
		t.Fatalf("应至少清理 1 个过期会话，得 %d", n)
	}
	if _, err := uploadSvc.Get(ctx, id2); !errorsIsNotFound(err) {
		t.Fatalf("过期会话记录应被清理，得 %v", err)
	}
}

func assertNoChunkFile(t *testing.T, svc *BackupUploadService, uploadID string, index int) {
	t.Helper()
	if _, err := os.Stat(svc.chunkPath(uploadID, index)); !os.IsNotExist(err) {
		t.Fatalf("不应残留分片文件 %d", index)
	}
	if _, err := os.Stat(svc.chunkPath(uploadID, index) + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("不应残留分片临时文件 %d", index)
	}
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func containsSubstring(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}

func errorsIsValidation(err error) bool { return errors.Is(err, ErrValidation) }
func errorsIsNotFound(err error) bool   { return errors.Is(err, ErrNotFound) }
