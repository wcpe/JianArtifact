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

// uploadFixture 聚合上传链路测试所需的全部依赖。
// 用具名字段而不是多返回值：新增依赖（如本轮加入的备份登记与恢复服务）时，
// 不必再改动六个调用点的位置参数序列。
type uploadFixture struct {
	uploads  *BackupUploadService
	imports  *BackupImportService
	impRepo  *repository.BackupImportRepo
	db       *persistence.DB
	dir      string
	restore  *RestoreService
	backupRp *repository.BackupPackageRepo
}

// newUploadFixture 装配一个完整的上传链路夹具（各自独立数据目录）。
//
// FR-40 追加八改变了上传的终点：组装完成后不再发起恢复，而是把包收进备份列表，
// 因此夹具需要一套真实的 BackupService + 登记仓库供 adopt 服务使用。
func newUploadFixture(t *testing.T) *uploadFixture {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")
	db := openMigratedTestDB(t, dir, "data.db")
	blobsDir := filepath.Join(dir, "blobs")
	blobsStore := blobstore.NewStore(blobsDir)
	restore := NewRestoreService(db, dir, dbPath, blobsDir)
	importRepo := repository.NewBackupImportRepo(db)
	importSvc := NewBackupImportService(importRepo, restore, dir, nil) // 本地导入不经过出站 client
	backupRepo := repository.NewBackupPackageRepo(db)
	backupSvc := NewBackupService(db, backupRepo, blobsStore, dir, dbPath, "0.11.0", func() string { return "np-test" })
	adoptSvc := NewBackupAdoptService(backupSvc, backupRepo)
	uploadRepo := repository.NewBackupUploadRepo(db)
	uploadSvc := NewBackupUploadService(uploadRepo, adoptSvc, dir)
	return &uploadFixture{
		uploads:  uploadSvc,
		imports:  importSvc,
		impRepo:  importRepo,
		db:       db,
		dir:      dir,
		restore:  restore,
		backupRp: backupRepo,
	}
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
// TestBackupUploadRoundTrip 覆盖 init → 逐片 PutChunk → Get 反映已传分片 →
// Complete 组装成包并**收进备份列表**（FR-40 追加八起不再触发恢复）。
func TestBackupUploadRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUploadFixture(t)
	uploadSvc := f.uploads

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

	// Complete：组装归档 → 收进备份列表（不再触发恢复）。
	imp, err := uploadSvc.Complete(ctx, uploadID, "", false, false)
	if err != nil {
		t.Fatalf("Complete：%v", err)
	}
	if imp.Status != repository.ImportStatusDone {
		t.Fatalf("回执状态应为 done（已入库），实际 %s", imp.Status)
	}
	if imp.PackageID == "" {
		t.Fatalf("回执应带回入库的包标识")
	}

	// 核心断言：包已出现在备份列表里，且状态为 done。
	stored, err := f.backupRp.Get(imp.PackageID)
	if err != nil {
		t.Fatalf("上传后应能在备份列表里读到该包：%v", err)
	}
	if stored.Status != repository.BackupStatusDone {
		t.Fatalf("入库状态应为 done，实际 %s", stored.Status)
	}
	if stored.Origin != repository.BackupOriginManual {
		t.Fatalf("上传入库的来源应为 manual，实际 %s", stored.Origin)
	}

	// 核心断言：不再写 restore.pending —— 这是本轮语义修正的全部意义。
	if pending, perr := f.restore.MarkedPending(); perr != nil {
		t.Fatalf("检查待生效恢复：%v", perr)
	} else if pending != nil {
		t.Fatalf("上传备份包不应产生待重启恢复，实际 %+v", pending)
	}

	// 归档应已搬运到 backups/ 下（不再留在上传会话目录里）。
	backupPath := filepath.Join(f.dir, "backups", imp.PackageID+".tar.gz")
	got, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("读取入库包：%v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("入库包与源包字节不一致（%d vs %d）", len(got), len(data))
	}
	if _, _, err := archive.Open(backupPath); err != nil {
		t.Fatalf("archive.Open 入库包失败：%v", err)
	}
}

// TestBackupUploadRetransmitSameChunkIsIdempotent 覆盖分片重传：
// 网络抖动后重发同一序号分片是覆盖语义（契约写明「重复片幂等覆盖」），
// 不应被"累计分片 + 本次 超过声明总字节"误拒。
func TestBackupUploadRetransmitSameChunkIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUploadFixture(t)
	uploadSvc := f.uploads

	src := newBackupFixture(t)
	src.seedAssets(t, "alpha", "beta", "gamma")
	rec, err := src.svc.Generate(ctx, CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	data, err := os.ReadFile(src.svc.PackagePath(rec.PackageID))
	if err != nil {
		t.Fatalf("读取源包：%v", err)
	}

	initRes, err := uploadSvc.Init(ctx, InitUploadOptions{
		FileName: "pkg.tar.gz", TotalBytes: int64(len(data)), Operator: "tester",
	})
	if err != nil {
		t.Fatalf("Init：%v", err)
	}
	uploadID := initRes.Upload.UploadID
	chunks := chunkData(t, data, int(uploadChunkSize))
	for i, c := range chunks {
		if _, err := uploadSvc.PutChunk(ctx, uploadID, i, bytes.NewReader(c), int64(len(c))); err != nil {
			t.Fatalf("PutChunk(%d)：%v", i, err)
		}
	}

	// 全部传完后重传最后一片：累计已等于声明总字节，覆盖语义应仍被接受。
	last := len(chunks) - 1
	if _, err := uploadSvc.PutChunk(ctx, uploadID, last, bytes.NewReader(chunks[last]), int64(len(chunks[last]))); err != nil {
		t.Fatalf("重传同一分片应幂等成功（当前实现会误报累计超额）：%v", err)
	}
	view, err := uploadSvc.Get(ctx, uploadID)
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	if len(view.UploadedChunks) != len(chunks) {
		t.Fatalf("重传不应改变已上传分片集合：%+v，期望 %d 片", view.UploadedChunks, len(chunks))
	}
}

// TestBackupUploadValidation 覆盖片号越界 / 单片超额 / 累计超额 → ErrValidation 且不留半成品分片。
func TestBackupUploadValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUploadFixture(t)
	uploadSvc := f.uploads
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
	t.Parallel()
	ctx := context.Background()
	f := newUploadFixture(t)
	uploadSvc := f.uploads
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
	t.Parallel()
	ctx := context.Background()
	f := newUploadFixture(t)
	uploadSvc := f.uploads
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
	t.Parallel()
	ctx := context.Background()
	f := newUploadFixture(t)
	uploadSvc := f.uploads
	db := f.db
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
