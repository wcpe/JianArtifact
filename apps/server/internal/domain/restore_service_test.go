package domain

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// restoreTarget 是一个全新的空目标实例夹具，用于恢复导入测试。
type restoreTarget struct {
	svc    *RestoreService
	db     *persistence.DB
	dir    string
	dbPath string
	store  *blobstore.Store
}

func newRestoreTargetFixture(t *testing.T) *restoreTarget {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jianartifact.db")
	db, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate：%v", err)
	}
	// 注意：迁移 0007_anonymous_setting.sql 会无条件植入内置 anonymous 主体，
	// TargetNonEmpty 已显式排除它，故"全新实例"经 Open+Migrate 后即被判为空目标，
	// 不应再清空任何表——否则就掩盖了"anonymous 不算非空"这一回归点。
	// 若某用例需要非空目标，自行 INSERT 业务行（见 TestRestoreImportTargetNotEmpty*）。
	store := blobstore.NewStore(filepath.Join(dir, "blobs"))
	svc := NewRestoreService(db, dir, dbPath, filepath.Join(dir, "blobs"))
	return &restoreTarget{svc: svc, db: db, dir: dir, dbPath: dbPath, store: store}
}

// craftRestorePackage 用指定 db 的快照与 blobstore 造一个包，mutate 可改 manifest 字段
// （如抬高 DBSchemaVersion 或设置 BasePackageID）。用于不兼容 / 增量等负路径。
func craftRestorePackage(t *testing.T, dir, dbPath string, store *blobstore.Store, localVersion int, mutate func(m *archive.Manifest)) string {
	t.Helper()
	snap := filepath.Join(dir, "craft.snap")
	if err := persistence.SnapshotFile(dbPath, snap); err != nil {
		t.Fatalf("SnapshotFile：%v", err)
	}
	blobs, err := persistence.AssetBlobs(snap)
	if err != nil {
		t.Fatalf("AssetBlobs：%v", err)
	}
	refs := make(archive.BlobIndex, 0, len(blobs))
	for h, sz := range blobs {
		refs = append(refs, archive.BlobRef{Hash: h, Size: sz})
	}
	m := archive.Manifest{
		SchemaVersion:   archive.SchemaVersion,
		Kind:            archive.Kind,
		PackageID:       "bk-craft-" + filepath.Base(dir),
		Mode:            archive.ModeHot,
		CreatedAt:       time.Now(),
		AppVersion:      "0.7.1",
		DBSchemaVersion: localVersion,
	}
	mutate(&m)
	if _, err := archive.WriteBundle(filepath.Join(dir, "craft.tar.gz"), m, snap, archive.NewBlobIndex(refs), store, nil); err != nil {
		t.Fatalf("WriteBundle：%v", err)
	}
	return filepath.Join(dir, "craft.tar.gz")
}

// TestRestoreImportRoundTrip 覆盖全新空目标的导入往返：
// 生成包 → 暂存 → 标记/暂存 db 落盘 → 应用替换 → db 被替换、pre-restore 保留、标记与暂存清理。
func TestRestoreImportRoundTrip(t *testing.T) {
	src := newBackupFixture(t)
	src.seedAssets(t, "alpha", "beta", "gamma")
	rec, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	pkg := src.svc.PackagePath(rec.PackageID)

	target := newRestoreTargetFixture(t)
	res, err := target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg, Origin: "cli"})
	if err != nil {
		t.Fatalf("Stage：%v", err)
	}
	if !res.Staged {
		t.Fatal("应已暂存")
	}
	if res.BlobMerged != 3 {
		t.Fatalf("blob 合并 = %d，期望 3", res.BlobMerged)
	}
	if res.BlobSkipped != 0 {
		t.Fatalf("blob 跳过 = %d，期望 0", res.BlobSkipped)
	}

	if _, err := os.Stat(target.svc.PendingPath()); err != nil {
		t.Fatalf("标记不存在：%v", err)
	}
	stagedDB := filepath.Join(target.svc.StagingDir(), archive.DBName)
	if _, err := os.Stat(stagedDB); err != nil {
		t.Fatalf("暂存 db 不存在：%v", err)
	}

	pending, err := target.svc.MarkedPending()
	if err != nil || pending == nil {
		t.Fatalf("读取标记：%v", err)
	}
	if pending.PackageID != rec.PackageID {
		t.Fatalf("标记包标识 = %s，期望 %s", pending.PackageID, rec.PackageID)
	}

	// 应用前先关闭目标连接（Windows 文件锁），再原子替换。
	_ = target.db.Close()
	applied, outcome, err := ApplyPendingRestore(target.dir, target.dbPath, filepath.Join(target.dir, "blobs"))
	if err != nil {
		t.Fatalf("ApplyPendingRestore：%v", err)
	}
	if !applied {
		t.Fatal("应已应用")
	}
	if outcome.PackageID != rec.PackageID {
		t.Fatalf("outcome 包标识 = %s", outcome.PackageID)
	}
	if outcome.PreRestoreDir == "" {
		t.Fatal("应保留 pre-restore 回滚备份")
	}

	reopened, err := persistence.Open(target.dbPath)
	if err != nil {
		t.Fatalf("重开 db：%v", err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Migrate(); err != nil {
		t.Fatalf("migrate：%v", err)
	}
	counts, err := persistence.RowCounts(target.dbPath, []string{"asset"})
	if err != nil {
		t.Fatalf("RowCounts：%v", err)
	}
	if counts["asset"] != 3 {
		t.Fatalf("恢复后资产数 = %d，期望 3", counts["asset"])
	}

	found := false
	for _, e := range readDirNames(t, target.dir) {
		if strings.HasPrefix(e, "pre-restore-") {
			found = true
		}
	}
	if !found {
		t.Fatal("缺少 pre-restore 备份目录")
	}

	if _, statErr := os.Stat(target.svc.PendingPath()); !os.IsNotExist(statErr) {
		t.Fatal("标记应已清理")
	}
	if _, statErr := os.Stat(target.svc.StagingDir()); !os.IsNotExist(statErr) {
		t.Fatal("暂存目录应已清理")
	}
}

func readDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s：%v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestRestoreImportTargetNotEmpty 目标非空且未 Overwrite → ErrRestoreTargetNotEmpty，不留文件。
func TestRestoreImportTargetNotEmpty(t *testing.T) {
	target := newRestoreTargetFixture(t)
	if _, err := target.db.Exec(`INSERT INTO repository (name, format, type) VALUES ('raw-repo','raw','hosted')`); err != nil {
		t.Fatalf("插入仓库：%v", err)
	}
	src := newBackupFixture(t)
	src.seedAssets(t, "x")
	rec, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	pkg := src.svc.PackagePath(rec.PackageID)

	_, err = target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg})
	if !errors.Is(err, ErrRestoreTargetNotEmpty) {
		t.Fatalf("目标非空应返回 ErrRestoreTargetNotEmpty，实际 %v", err)
	}
	if _, statErr := os.Stat(target.svc.StagingDir()); !os.IsNotExist(statErr) {
		t.Fatal("目标非空失败不应留下暂存目录")
	}
	if _, statErr := os.Stat(target.svc.PendingPath()); !os.IsNotExist(statErr) {
		t.Fatal("目标非空失败不应留下标记")
	}
}

// TestRestoreImportEmptyDBSucceeds 回归：全新实例经 persistence.Open + db.Migrate() 后
// 不做任何插入（仅迁移植入的内置 anonymous 主体），直接 Stage(Overwrite=false) 必须成功——
// 验证 TargetNonEmpty 已正确排除 anonymous，否则此用例会被误判为非空而失败。
func TestRestoreImportEmptyDBSucceeds(t *testing.T) {
	src := newBackupFixture(t)
	src.seedAssets(t, "alpha", "beta")
	rec, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	pkg := src.svc.PackagePath(rec.PackageID)

	// 目标仅为 Open + Migrate 后的全新库，不做任何业务插入。
	target := newRestoreTargetFixture(t)
	res, err := target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg, Origin: "cli"})
	if err != nil {
		t.Fatalf("空目标 Stage(Overwrite=false) 应成功，实际 %v", err)
	}
	if !res.Staged {
		t.Fatal("应已暂存")
	}
	if _, statErr := os.Stat(target.svc.PendingPath()); statErr != nil {
		t.Fatalf("标记应已写入：%v", statErr)
	}
}

// TestRestoreImportTargetNotEmptyWithUser 反向回归：插入一条非 anonymous 的 user 后，
// Stage 不带 Overwrite 必须返回 ErrRestoreTargetNotEmpty（anonymous 不算非空，但真实用户算）。
func TestRestoreImportTargetNotEmptyWithUser(t *testing.T) {
	target := newRestoreTargetFixture(t)
	if _, err := target.db.Exec(`INSERT INTO user (username, password_hash) VALUES ('real-admin','ph')`); err != nil {
		t.Fatalf("插入用户：%v", err)
	}

	src := newBackupFixture(t)
	src.seedAssets(t, "x")
	rec, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	pkg := src.svc.PackagePath(rec.PackageID)

	_, err = target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg})
	if !errors.Is(err, ErrRestoreTargetNotEmpty) {
		t.Fatalf("存在真实用户时应返回 ErrRestoreTargetNotEmpty，实际 %v", err)
	}
	if _, statErr := os.Stat(target.svc.StagingDir()); !os.IsNotExist(statErr) {
		t.Fatal("目标非空失败不应留下暂存目录")
	}
	if _, statErr := os.Stat(target.svc.PendingPath()); !os.IsNotExist(statErr) {
		t.Fatal("目标非空失败不应留下标记")
	}
}

// TestRestoreImportOverwriteKeepsPreRestore 覆盖导入成功，且 pre-restore 备份保留。
func TestRestoreImportOverwriteKeepsPreRestore(t *testing.T) {
	target := newRestoreTargetFixture(t)
	if _, err := target.db.Exec(`INSERT INTO repository (name, format, type) VALUES ('raw-repo','raw','hosted')`); err != nil {
		t.Fatalf("插入仓库：%v", err)
	}
	src := newBackupFixture(t)
	src.seedAssets(t, "x")
	rec, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	pkg := src.svc.PackagePath(rec.PackageID)

	res, err := target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg, Overwrite: true})
	if err != nil {
		t.Fatalf("Stage(overwrite)：%v", err)
	}
	if !res.Staged {
		t.Fatal("应已暂存")
	}

	_ = target.db.Close()
	applied, outcome, err := ApplyPendingRestore(target.dir, target.dbPath, filepath.Join(target.dir, "blobs"))
	if err != nil {
		t.Fatalf("ApplyPendingRestore：%v", err)
	}
	if !applied {
		t.Fatal("应已应用")
	}
	if outcome.PreRestoreDir == "" {
		t.Fatal("覆盖时应保留 pre-restore 回滚备份")
	}
	if _, statErr := os.Stat(outcome.PreRestoreDir); statErr != nil {
		t.Fatalf("pre-restore 目录应存在：%v", statErr)
	}
}

// TestRestoreImportIncompatible 包 DBSchemaVersion 高于本地 → ErrRestoreIncompatible，不留文件。
func TestRestoreImportIncompatible(t *testing.T) {
	src := newBackupFixture(t)
	src.seedAssets(t, "x")
	if _, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot}); err != nil {
		t.Fatalf("Generate：%v", err)
	}
	localVer := src.svc.dbSchemaVersion()
	pkg := craftRestorePackage(t, src.dataDir, src.dbPath, src.store, localVer, func(m *archive.Manifest) {
		m.DBSchemaVersion = 99999
	})

	target := newRestoreTargetFixture(t)
	_, err := target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg})
	if !errors.Is(err, ErrRestoreIncompatible) {
		t.Fatalf("高版本应返回 ErrRestoreIncompatible，实际 %v", err)
	}
	if _, statErr := os.Stat(target.svc.StagingDir()); !os.IsNotExist(statErr) {
		t.Fatal("不兼容不应留下暂存目录")
	}
	if _, statErr := os.Stat(target.svc.PendingPath()); !os.IsNotExist(statErr) {
		t.Fatal("不兼容不应留下标记")
	}
}

// TestRestoreImportCorrupted 归档内容损坏（翻转中段字节）→ 校验失败，不留文件、不留标记。
func TestRestoreImportCorrupted(t *testing.T) {
	src := newBackupFixture(t)
	src.seedAssets(t, "alpha", "beta")
	rec, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	pkg := src.svc.PackagePath(rec.PackageID)

	corrupt := filepath.Join(src.dataDir, "corrupt.tar.gz")
	data, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatalf("读包：%v", err)
	}
	mid := len(data) / 2
	data[mid] ^= 0xFF
	if err := os.WriteFile(corrupt, data, 0o600); err != nil {
		t.Fatalf("写损坏包：%v", err)
	}

	target := newRestoreTargetFixture(t)
	_, err = target.svc.Stage(context.Background(), RestoreRequest{SourcePath: corrupt})
	if err == nil {
		t.Fatal("损坏包应校验失败")
	}
	if _, statErr := os.Stat(target.svc.StagingDir()); !os.IsNotExist(statErr) {
		t.Fatal("损坏不应留下暂存目录")
	}
	if _, statErr := os.Stat(target.svc.PendingPath()); !os.IsNotExist(statErr) {
		t.Fatal("损坏不应留下标记")
	}
}

// TestRestoreImportIncrementalUnsupported 增量包（BasePackageID 非空）→ ErrRestoreIncrementalUnsupported。
func TestRestoreImportIncrementalUnsupported(t *testing.T) {
	src := newBackupFixture(t)
	src.seedAssets(t, "x")
	if _, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot}); err != nil {
		t.Fatalf("Generate：%v", err)
	}
	localVer := src.svc.dbSchemaVersion()
	pkg := craftRestorePackage(t, src.dataDir, src.dbPath, src.store, localVer, func(m *archive.Manifest) {
		m.BasePackageID = "bk-base-xxxx"
	})

	target := newRestoreTargetFixture(t)
	_, err := target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg})
	if !errors.Is(err, ErrRestoreIncrementalUnsupported) {
		t.Fatalf("增量包应返回 ErrRestoreIncrementalUnsupported，实际 %v", err)
	}
	if _, statErr := os.Stat(target.svc.StagingDir()); !os.IsNotExist(statErr) {
		t.Fatal("增量包不应留下暂存目录")
	}
	if _, statErr := os.Stat(target.svc.PendingPath()); !os.IsNotExist(statErr) {
		t.Fatal("增量包不应留下标记")
	}
}

// TestRestoreImportPendingExists 已存在标记时再 Stage → ErrRestorePending。
func TestRestoreImportPendingExists(t *testing.T) {
	src := newBackupFixture(t)
	src.seedAssets(t, "x")
	rec, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	pkg := src.svc.PackagePath(rec.PackageID)

	target := newRestoreTargetFixture(t)
	if _, err := target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg}); err != nil {
		t.Fatalf("首次 Stage：%v", err)
	}
	_, err = target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg})
	if !errors.Is(err, ErrRestorePending) {
		t.Fatalf("已存在标记应返回 ErrRestorePending，实际 %v", err)
	}
}

// TestRestoreImportBlobSkipped 已存在的 blob 走跳过路径，且文件 mtime 不变。
func TestRestoreImportBlobSkipped(t *testing.T) {
	src := newBackupFixture(t)
	src.seedAssets(t, "shared")
	rec, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	pkg := src.svc.PackagePath(rec.PackageID)

	target := newRestoreTargetFixture(t)
	// 目标里也写入同一 blob 并挂到 asset 表（进入 localSet）。
	hash, _, _, _, err := target.store.Put(bytes.NewReader([]byte("shared")))
	if err != nil {
		t.Fatalf("写 blob：%v", err)
	}
	if _, err := target.db.Exec(`INSERT INTO repository (name, format, type) VALUES ('raw-repo','raw','hosted')`); err != nil {
		t.Fatalf("插入仓库：%v", err)
	}
	if _, err := target.db.Exec(
		`INSERT INTO asset (repository_id, path, blob_hash, size, content_type) VALUES (1, 'a.bin', ?, ?, 'application/octet-stream')`,
		hash, len("shared")); err != nil {
		t.Fatalf("插入资产：%v", err)
	}

	blobPath := filepath.Join(target.dir, "blobs", hash[:2], hash[2:4], hash)
	before, err := os.Stat(blobPath)
	if err != nil {
		t.Fatalf("读 blob stat：%v", err)
	}

	res, err := target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg, Overwrite: true})
	if err != nil {
		t.Fatalf("Stage：%v", err)
	}
	if res.BlobSkipped < 1 {
		t.Fatalf("blob 跳过 = %d，期望 >=1", res.BlobSkipped)
	}
	if res.BlobMerged != 0 {
		t.Fatalf("blob 合并 = %d，期望 0（shared 已存在）", res.BlobMerged)
	}
	after, err := os.Stat(blobPath)
	if err != nil {
		t.Fatalf("读 blob stat after：%v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("被跳过的 blob 文件 mtime 不应改变：%v -> %v", before.ModTime(), after.ModTime())
	}
}

// TestRestoreReconcileStaleStaging 只留暂存目录、无标记 → 清理掉。
func TestRestoreReconcileStaleStaging(t *testing.T) {
	target := newRestoreTargetFixture(t)
	if err := os.MkdirAll(target.svc.StagingDir(), 0o750); err != nil {
		t.Fatalf("mkdir：%v", err)
	}
	if err := target.svc.ReconcileStaleStaging(); err != nil {
		t.Fatalf("ReconcileStaleStaging：%v", err)
	}
	if _, statErr := os.Stat(target.svc.StagingDir()); !os.IsNotExist(statErr) {
		t.Fatal("陈旧暂存目录应被清理")
	}
}

// TestRestoreReconcileKeepsWhenPending 有标记时 ReconcileStaleStaging 不动暂存与标记。
func TestRestoreReconcileKeepsWhenPending(t *testing.T) {
	target := newRestoreTargetFixture(t)
	src := newBackupFixture(t)
	src.seedAssets(t, "x")
	rec, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	pkg := src.svc.PackagePath(rec.PackageID)
	if _, err := target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg}); err != nil {
		t.Fatalf("Stage：%v", err)
	}
	if err := target.svc.ReconcileStaleStaging(); err != nil {
		t.Fatalf("ReconcileStaleStaging：%v", err)
	}
	if _, statErr := os.Stat(target.svc.PendingPath()); os.IsNotExist(statErr) {
		t.Fatal("有标记时不应清理标记")
	}
	if _, statErr := os.Stat(target.svc.StagingDir()); os.IsNotExist(statErr) {
		t.Fatal("有标记时不应清理暂存目录")
	}
}

// TestApplyPendingRestoreNoMarker 无标记时返回 applied=false 且不出错。
func TestApplyPendingRestoreNoMarker(t *testing.T) {
	target := newRestoreTargetFixture(t)
	_ = target.db.Close()
	applied, _, err := ApplyPendingRestore(target.dir, target.dbPath, filepath.Join(target.dir, "blobs"))
	if err != nil {
		t.Fatalf("ApplyPendingRestore：%v", err)
	}
	if applied {
		t.Fatal("无标记不应应用")
	}
}

// writeManifestOnlyPackage 造一个只含 manifest.json 的归档，用于构造"声明值异常"的负路径。
// 规模护栏在 archive.Reader.Verify 之前执行，因此不需要真实载荷即可触发。
func writeManifestOnlyPackage(t *testing.T, path string, m archive.Manifest) {
	t.Helper()
	data, err := archive.MarshalManifest(m)
	if err != nil {
		t.Fatalf("序列化 manifest：%v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建归档：%v", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:    archive.ManifestName,
		Mode:    0o644,
		Size:    int64(len(data)),
		ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("写条目头：%v", err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("写 manifest：%v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("收尾 tar：%v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("收尾 gzip：%v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭归档：%v", err)
	}
}

// TestRestoreTargetNonEmptyExcludesAnonymous 是"内置 anonymous 主体不计入非空判据"的回归点。
//
// 迁移 0007 会无条件植入 anonymous，若按 user 表原始行数判定，任何全新实例都会被判为
// 非空，迫使主用例（把包导入到一台新机器）也要 --overwrite —— 护栏退化成噪音。
func TestRestoreTargetNonEmptyExcludesAnonymous(t *testing.T) {
	target := newRestoreTargetFixture(t)

	nonEmpty, err := target.svc.TargetNonEmpty()
	if err != nil {
		t.Fatalf("TargetNonEmpty：%v", err)
	}
	if nonEmpty {
		t.Fatal("全新实例（仅含迁移植入的 anonymous 主体）不应被判为非空")
	}

	// 反向：出现真实业务主体后必须判为非空，否则护栏形同虚设。
	if _, err := target.db.Exec(
		`INSERT INTO user (username, password_hash, role, status) VALUES ('real-user', '!', 'admin', 'active')`,
	); err != nil {
		t.Fatalf("插入真实用户：%v", err)
	}
	nonEmpty, err = target.svc.TargetNonEmpty()
	if err != nil {
		t.Fatalf("TargetNonEmpty：%v", err)
	}
	if !nonEmpty {
		t.Fatal("存在真实用户时应判为非空")
	}
}

// TestRestoreRejectsOversizePackage 覆盖规模护栏：归档是不可信输入（可能来自 URL 拉取
// 或分片上传），tar.gz 可声明远超自身大小的内容，必须在任何磁盘写入前拦下。
func TestRestoreRejectsOversizePackage(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m *archive.Manifest)
	}{
		{"快照超上限", func(m *archive.Manifest) {
			m.DB.SizeBytes = maxRestoreDBBytes + 1
		}},
		{"快照大小不合理", func(m *archive.Manifest) {
			m.DB.SizeBytes = 0
		}},
		{"blob 总量超上限", func(m *archive.Manifest) {
			m.Blobs.TotalBytes = maxRestoreBlobBytes + 1
		}},
		{"blob 条目数超上限", func(m *archive.Manifest) {
			m.Blobs.Count = maxRestoreBlobCount + 1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := newRestoreTargetFixture(t)
			pkg := filepath.Join(target.dir, "oversize.tar.gz")
			m := archive.Manifest{
				SchemaVersion:   archive.SchemaVersion,
				Kind:            archive.Kind,
				PackageID:       "bk-oversize",
				Mode:            archive.ModeHot,
				CreatedAt:       time.Now(),
				AppVersion:      "0.7.1",
				DBSchemaVersion: target.svc.dbSchemaVersion(),
				DB:              archive.DBRef{File: archive.DBName, SizeBytes: 4096, SHA256: strings.Repeat("a", 64)},
				Blobs:           archive.BlobsRef{File: archive.BlobIndexName},
			}
			tc.mutate(&m)
			writeManifestOnlyPackage(t, pkg, m)

			if _, err := target.svc.Stage(context.Background(), RestoreRequest{
				SourcePath: pkg,
				Origin:     "cli",
			}); !errors.Is(err, ErrRestoreTooLarge) {
				t.Fatalf("应返回 ErrRestoreTooLarge，实际 %v", err)
			}
			// 触发护栏时必须不留任何痕迹。
			if _, statErr := os.Stat(target.svc.PendingPath()); !os.IsNotExist(statErr) {
				t.Fatal("触发规模护栏后不应留下待生效标记")
			}
			if _, statErr := os.Stat(target.svc.StagingDir()); !os.IsNotExist(statErr) {
				t.Fatal("触发规模护栏后不应留下暂存目录")
			}
		})
	}
}

// TestMarkedPendingReturnsNilOnFreshTarget 是 readPendingFile 缺文件归一化的回归点：
// 全新目标（无 restore.pending）调 MarkedPending 必须返回 (nil, nil) 且 err == nil——
// 这是 BackupImportService.StartURLImport 在全新节点能正常登记 queued 的前提
// （否则被误判为内部错误 → 导入恒返 500）。修复见 restore_service.go readPendingFile。
func TestMarkedPendingReturnsNilOnFreshTarget(t *testing.T) {
	target := newRestoreTargetFixture(t)
	pending, err := target.svc.MarkedPending()
	if err != nil {
		t.Fatalf("fresh target MarkedPending 应返回 (nil,nil)，实际 err=%v", err)
	}
	if pending != nil {
		t.Fatalf("fresh target MarkedPending 应返回 nil，实际 %+v", pending)
	}
}

// TestRestoreConcurrentStageRejected 覆盖导入串行化：暂存目录与待生效标记是单例资源，
// 两个并发导入（例如 HTTP 导入与 CLI 同时发起）会互相踩踏。
func TestRestoreConcurrentStageRejected(t *testing.T) {
	target := newRestoreTargetFixture(t)

	// 先占住导入权，模拟已有导入正在进行。
	if !target.svc.beginImport() {
		t.Fatal("首次应能取得导入权")
	}
	defer target.svc.endImport()

	if _, err := target.svc.Stage(context.Background(), RestoreRequest{
		SourcePath: filepath.Join(target.dir, "does-not-matter.tar.gz"),
		Origin:     "cli",
	}); !errors.Is(err, ErrRestoreInProgress) {
		t.Fatalf("并发导入应返回 ErrRestoreInProgress，实际 %v", err)
	}
}

// TestRestoreDeltaImport 覆盖差包导入主路径：先导入基线包再导入差包 → 成功，且最终库的
// blob 集合与源一致（用 persistence.AssetBlobs 比对）。
func TestRestoreDeltaImport(t *testing.T) {
	src := newBackupFixture(t)
	src.seedAssets(t, "alpha", "beta", "gamma") // 基线 blob
	rec0, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate 基线：%v", err)
	}
	src.seedAssets(t, "delta1") // 新增 blob
	rec1, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot, BasePackageID: rec0.PackageID})
	if err != nil {
		t.Fatalf("Generate 差包：%v", err)
	}
	pkg0 := src.svc.PackagePath(rec0.PackageID)
	pkg1 := src.svc.PackagePath(rec1.PackageID)

	target := newRestoreTargetFixture(t)
	blobDirT := filepath.Join(target.dir, "blobs")

	// 1) 先导入基线包。
	if _, err := target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg0, Origin: "cli"}); err != nil {
		t.Fatalf("Stage 基线：%v", err)
	}
	_ = target.db.Close()
	if _, _, err := ApplyPendingRestore(target.dir, target.dbPath, blobDirT); err != nil {
		t.Fatalf("ApplyPendingRestore 基线：%v", err)
	}

	// 2) 再导入差包（目标已非空，需 Overwrite；基线已就位，并集核对通过）。
	reopened, err := persistence.Open(target.dbPath)
	if err != nil {
		t.Fatalf("重开目标 db：%v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	svc2 := NewRestoreService(reopened, target.dir, target.dbPath, blobDirT)

	res, err := svc2.Stage(context.Background(), RestoreRequest{SourcePath: pkg1, Origin: "cli", Overwrite: true})
	if err != nil {
		t.Fatalf("Stage 差包：%v", err)
	}
	if res.Manifest.PackageID != rec1.PackageID {
		t.Fatalf("差包包标识 = %s，期望 %s", res.Manifest.PackageID, rec1.PackageID)
	}
	if res.BlobMerged != 1 {
		t.Fatalf("差包 blob 合并 = %d，期望 1（新增 delta1）", res.BlobMerged)
	}
	_ = reopened.Close()
	if _, _, err := ApplyPendingRestore(target.dir, target.dbPath, blobDirT); err != nil {
		t.Fatalf("ApplyPendingRestore 差包：%v", err)
	}

	// 3) 最终库的 blob 集合必须与源一致。
	srcFull, err := persistence.AssetBlobs(src.dbPath)
	if err != nil {
		t.Fatalf("读取源完整集合：%v", err)
	}
	tgtFull, err := persistence.AssetBlobs(target.dbPath)
	if err != nil {
		t.Fatalf("读取目标最终集合：%v", err)
	}
	if len(srcFull) != len(tgtFull) {
		t.Fatalf("目标 blob 数 = %d，期望 %d", len(tgtFull), len(srcFull))
	}
	for h, sz := range srcFull {
		ts, ok := tgtFull[h]
		if !ok {
			t.Fatalf("目标缺失源 blob %s", h)
		}
		if ts != sz {
			t.Fatalf("blob %s 大小不一致：源 %d 目标 %d", h, sz, ts)
		}
	}
}

// TestRestoreDeltaImportRejectsMissingBase 覆盖差包导入负路径：目标缺少基线包应有的 blob →
// ErrRestoreBaseMissing，且绝不落暂存目录与标记。
func TestRestoreDeltaImportRejectsMissingBase(t *testing.T) {
	src := newBackupFixture(t)
	src.seedAssets(t, "alpha", "beta", "gamma")
	rec0, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate 基线：%v", err)
	}
	src.seedAssets(t, "delta1")
	rec1, err := src.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot, BasePackageID: rec0.PackageID})
	if err != nil {
		t.Fatalf("Generate 差包：%v", err)
	}
	pkg1 := src.svc.PackagePath(rec1.PackageID)

	// 全新空目标（未导入基线包）直接导入差包。
	target := newRestoreTargetFixture(t)
	_, err = target.svc.Stage(context.Background(), RestoreRequest{SourcePath: pkg1, Origin: "cli"})
	if !errors.Is(err, ErrRestoreBaseMissing) {
		t.Fatalf("缺少基线应返回 ErrRestoreBaseMissing，实际 %v", err)
	}
	if _, statErr := os.Stat(target.svc.StagingDir()); !os.IsNotExist(statErr) {
		t.Fatal("缺少基线失败不应留下暂存目录")
	}
	if _, statErr := os.Stat(target.svc.PendingPath()); !os.IsNotExist(statErr) {
		t.Fatal("缺少基线失败不应留下标记")
	}
}
