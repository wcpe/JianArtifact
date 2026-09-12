package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

type backupFixture struct {
	svc     *BackupService
	repo    *repository.BackupPackageRepo
	store   *blobstore.Store
	db      *persistence.DB
	dataDir string
	dbPath  string
	repoSeq int
}

func newBackupFixture(t *testing.T) *backupFixture {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "jianartifact.db")
	db, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate：%v", err)
	}

	store := blobstore.NewStore(filepath.Join(dataDir, "blobs"))
	repo := repository.NewBackupPackageRepo(db)
	svc := NewBackupService(db, repo, store, dataDir, dbPath, "0.7.1", func() string { return "np-test" })
	return &backupFixture{svc: svc, repo: repo, store: store, db: db, dataDir: dataDir, dbPath: dbPath}
}

// seedAssets 建一个仓库并写入若干资产（blob 同步落盘），返回 blob 哈希。
// 每次调用使用唯一仓库名，支持在同一夹具内多次调用（基线与差包各自写入新资产）。
func (f *backupFixture) seedAssets(t *testing.T, contents ...string) []string {
	t.Helper()
	f.repoSeq++
	res, err := f.db.Exec(`INSERT INTO repository (name, format, type) VALUES (?, 'raw','hosted')`, fmt.Sprintf("raw-repo-%d", f.repoSeq))
	if err != nil {
		t.Fatalf("插入仓库：%v", err)
	}
	repoID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取仓库 id：%v", err)
	}
	hashes := make([]string, 0, len(contents))
	for i, body := range contents {
		hash, _, _, _, err := f.store.Put(bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("写入 blob：%v", err)
		}
		if _, err := f.db.Exec(
			`INSERT INTO asset (repository_id, path, blob_hash, size, content_type) VALUES (?, ?, ?, ?, 'application/octet-stream')`,
			repoID, fmt.Sprintf("a/%d.bin", i), hash, len(body)); err != nil {
			t.Fatalf("插入资产：%v", err)
		}
		hashes = append(hashes, hash)
	}
	return hashes
}

func TestBackupGenerateHotProducesVerifiablePackage(t *testing.T) {
	f := newBackupFixture(t)
	f.seedAssets(t, "alpha", "beta", "gamma")

	rec, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot, Label: "上线前"})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	if rec.Status != repository.BackupStatusDone {
		t.Fatalf("状态 = %s，期望 done", rec.Status)
	}
	if rec.SizeBytes <= 0 {
		t.Fatalf("包体大小 = %d，期望 > 0", rec.SizeBytes)
	}
	if rec.NodeID != "np-test" || rec.AppVersion != "0.7.1" {
		t.Fatalf("来源身份未记录：%+v", rec)
	}
	if rec.FinishedAt == nil {
		t.Fatal("完成的包应有 finishedAt")
	}
	if _, err := os.Stat(f.svc.PackagePath(rec.PackageID)); err != nil {
		t.Fatalf("包体文件不存在：%v", err)
	}

	manifest, err := f.svc.Verify(rec.PackageID, true)
	if err != nil {
		t.Fatalf("Verify(deep)：%v", err)
	}
	if manifest.Kind != archive.Kind || manifest.PackageID != rec.PackageID {
		t.Fatalf("manifest 身份有误：%+v", manifest)
	}
	if manifest.Counts.Assets != 3 || manifest.Counts.Repositories != 1 {
		t.Fatalf("计数有误：%+v", manifest.Counts)
	}
	if manifest.Blobs.Count != 3 {
		t.Fatalf("blob 数 = %d，期望 3", manifest.Blobs.Count)
	}
	if manifest.IsIncremental() {
		t.Fatal("全量包不应带基线")
	}
}

// TestBackupSkipsUnreferencedBlobs 验证 blob 集合以 asset 表为准：
// 目录里未被引用的 blob 不应进入包，否则包体会被历史残留撑大。
func TestBackupSkipsUnreferencedBlobs(t *testing.T) {
	f := newBackupFixture(t)
	f.seedAssets(t, "referenced")
	// 再写入一个不挂到任何资产上的 blob。
	if _, _, _, _, err := f.store.Put(bytes.NewReader([]byte("orphan"))); err != nil {
		t.Fatalf("写入孤立 blob：%v", err)
	}

	rec, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	manifest, err := f.svc.Verify(rec.PackageID, true)
	if err != nil {
		t.Fatalf("Verify：%v", err)
	}
	if manifest.Blobs.Count != 1 {
		t.Fatalf("blob 数 = %d，期望 1（只含被引用的）", manifest.Blobs.Count)
	}
}

// TestBackupGenerateRejectsConcurrent 覆盖生成串行化：并发第二次生成应被拒绝，
// 避免两条快照/打包互相挤压磁盘与内存。
func TestBackupGenerateRejectsConcurrent(t *testing.T) {
	f := newBackupFixture(t)
	f.seedAssets(t, "one")

	var once sync.Once
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Generate(context.Background(), CreateBackupOptions{
			Mode: archive.ModeHot,
			Progress: func(_, _ int) {
				once.Do(func() { close(entered) })
				<-release
			},
		})
		done <- err
	}()

	<-entered
	_, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if !errors.Is(err, ErrBackupInProgress) {
		t.Fatalf("并发生成应返回 ErrBackupInProgress，实际 %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("首个生成应成功：%v", err)
	}
}

// TestBackupGenerateFailureCleansUp 覆盖失败路径：引用一个磁盘上不存在的 blob 时，
// 生成必须失败、登记置为 failed，且不留下可被误用的半成品包。
func TestBackupGenerateFailureCleansUp(t *testing.T) {
	f := newBackupFixture(t)
	res, err := f.db.Exec(`INSERT INTO repository (name, format, type) VALUES ('raw-repo','raw','hosted')`)
	if err != nil {
		t.Fatalf("插入仓库：%v", err)
	}
	repoID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取仓库 id：%v", err)
	}
	// 哈希合法但磁盘上没有对应 blob。
	missing := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := f.db.Exec(
		`INSERT INTO asset (repository_id, path, blob_hash, size) VALUES (?, 'x.bin', ?, 5)`,
		repoID, missing); err != nil {
		t.Fatalf("插入资产：%v", err)
	}

	rec, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err == nil {
		t.Fatal("引用缺失 blob 时生成应当失败")
	}
	// 返回的记录必须与库内登记一致（失败状态不能在返回路径上丢失）。
	if rec.Status != repository.BackupStatusFailed || rec.ErrorSummary == "" {
		t.Fatalf("失败返回值有误：%+v", rec)
	}
	got, getErr := f.svc.Get(rec.PackageID)
	if getErr != nil {
		t.Fatalf("失败记录应仍可查询：%v", getErr)
	}
	if got.Status != repository.BackupStatusFailed || got.ErrorSummary == "" {
		t.Fatalf("失败登记有误：%+v", got)
	}
	if _, statErr := os.Stat(f.svc.PackagePath(rec.PackageID)); !os.IsNotExist(statErr) {
		t.Fatalf("失败后不应留下包体文件，stat err=%v", statErr)
	}
	// 快照临时文件也必须清理。
	entries, err := os.ReadDir(f.svc.Dir())
	if err != nil {
		t.Fatalf("读取备份目录：%v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".snap" {
			t.Fatalf("残留快照文件：%s", e.Name())
		}
	}
}

func TestBackupGenerateRejectsUnknownMode(t *testing.T) {
	f := newBackupFixture(t)
	if _, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: "weird"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("未知模式应返回 ErrValidation，实际 %v", err)
	}
}

func TestBackupOpenReaderRejectsIncomplete(t *testing.T) {
	f := newBackupFixture(t)
	if err := f.repo.Create(repository.BackupPackage{
		PackageID: "bk-pending",
		Mode:      repository.BackupModeHot,
		Status:    repository.BackupStatusPacking,
	}); err != nil {
		t.Fatalf("Create：%v", err)
	}
	if _, _, err := f.svc.OpenReader("bk-pending"); !errors.Is(err, ErrBackupIncomplete) {
		t.Fatalf("未完成的包应返回 ErrBackupIncomplete，实际 %v", err)
	}
}

func TestBackupOpenReaderReportsMissingFile(t *testing.T) {
	f := newBackupFixture(t)
	if err := f.repo.Create(repository.BackupPackage{
		PackageID: "bk-ghost",
		Mode:      repository.BackupModeHot,
		Status:    repository.BackupStatusDone,
	}); err != nil {
		t.Fatalf("Create：%v", err)
	}
	if _, _, err := f.svc.OpenReader("bk-ghost"); !errors.Is(err, ErrBackupFileMissing) {
		t.Fatalf("文件缺失应返回 ErrBackupFileMissing，实际 %v", err)
	}
}

func TestBackupGetUnknown(t *testing.T) {
	f := newBackupFixture(t)
	if _, err := f.svc.Get("nope"); !errors.Is(err, ErrBackupNotFound) {
		t.Fatalf("不存在的包应返回 ErrBackupNotFound，实际 %v", err)
	}
}

func TestBackupDeleteLifecycle(t *testing.T) {
	f := newBackupFixture(t)
	f.seedAssets(t, "payload")
	rec, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	if err := f.svc.Delete(rec.PackageID); err != nil {
		t.Fatalf("Delete：%v", err)
	}
	if _, err := f.svc.Get(rec.PackageID); !errors.Is(err, ErrBackupNotFound) {
		t.Fatalf("删除后应查不到，实际 %v", err)
	}
	if _, statErr := os.Stat(f.svc.PackagePath(rec.PackageID)); !os.IsNotExist(statErr) {
		t.Fatal("删除后包体文件应消失")
	}
	if err := f.svc.Delete("nope"); !errors.Is(err, ErrBackupNotFound) {
		t.Fatalf("删除不存在的包应返回 ErrBackupNotFound，实际 %v", err)
	}
}

// TestBackupDeleteRefusesBaseWithDerived 验证被增量包引用的基线不可删除，
// 否则派生包在目标端永远无法还原。
func TestBackupDeleteRefusesBaseWithDerived(t *testing.T) {
	f := newBackupFixture(t)
	if err := f.repo.Create(repository.BackupPackage{
		PackageID: "bk-base", Mode: repository.BackupModeHot, Status: repository.BackupStatusDone,
	}); err != nil {
		t.Fatalf("Create base：%v", err)
	}
	if err := f.repo.Create(repository.BackupPackage{
		PackageID: "bk-delta", Mode: repository.BackupModeFrozen, Status: repository.BackupStatusDone,
		BasePackageID: "bk-base",
	}); err != nil {
		t.Fatalf("Create delta：%v", err)
	}
	if err := f.svc.Delete("bk-base"); !errors.Is(err, ErrBackupHasDerived) {
		t.Fatalf("基线有派生包时应拒绝删除，实际 %v", err)
	}
	// 派生包本身没有下级，可以删。
	if err := f.svc.Delete("bk-delta"); err != nil {
		t.Fatalf("删除派生包：%v", err)
	}
}

func TestBackupReconcileStartupMarksInterrupted(t *testing.T) {
	f := newBackupFixture(t)
	for id, status := range map[string]string{
		"bk-q": repository.BackupStatusQueued,
		"bk-s": repository.BackupStatusSnapshotting,
		"bk-p": repository.BackupStatusPacking,
		"bk-d": repository.BackupStatusDone,
	} {
		if err := f.repo.Create(repository.BackupPackage{
			PackageID: id, Mode: repository.BackupModeHot, Status: status,
		}); err != nil {
			t.Fatalf("Create %s：%v", id, err)
		}
	}
	n, err := f.svc.ReconcileStartup()
	if err != nil {
		t.Fatalf("ReconcileStartup：%v", err)
	}
	if n != 3 {
		t.Fatalf("标记行数 = %d，期望 3", n)
	}
	done, err := f.svc.Get("bk-d")
	if err != nil {
		t.Fatalf("Get bk-d：%v", err)
	}
	if done.Status != repository.BackupStatusDone {
		t.Fatalf("已完成包不应被改动，实际 %s", done.Status)
	}
}

func TestBackupListReflectsGeneratedPackages(t *testing.T) {
	f := newBackupFixture(t)
	f.seedAssets(t, "x")
	for i := 0; i < 2; i++ {
		if _, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot}); err != nil {
			t.Fatalf("Generate #%d：%v", i, err)
		}
	}
	items, err := f.svc.List(10, 0)
	if err != nil {
		t.Fatalf("List：%v", err)
	}
	if len(items) != 2 {
		t.Fatalf("列表长度 = %d，期望 2", len(items))
	}
	n, err := f.svc.Count()
	if err != nil || n != 2 {
		t.Fatalf("Count = %d err=%v，期望 2", n, err)
	}
}

// TestBackupDeltaGenerate 覆盖增量差包生成：包体只含新增 blob、manifest 带 BasePackageID 与
// Expected、Expected == 完整集合（并集）摘要、侧车 == 完整集合；基线侧车 == 基线完整集合。
func TestBackupDeltaGenerate(t *testing.T) {
	f := newBackupFixture(t)
	baseHashes := f.seedAssets(t, "alpha", "beta", "gamma")

	rec0, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate 基线：%v", err)
	}
	if rec0.IsIncremental() {
		t.Fatal("基线包不应是增量")
	}
	// 全量包不带 Expected，且侧车 = 完整集合。
	baseReader, baseM, err := archive.Open(f.svc.PackagePath(rec0.PackageID))
	if err != nil {
		t.Fatalf("Open 基线：%v", err)
	}
	if baseM.Expected != nil {
		t.Fatal("全量包 Expected 应为 nil")
	}
	baseSidecar, err := f.svc.readSidecar(rec0.PackageID)
	if err != nil {
		t.Fatalf("读基线侧车：%v", err)
	}
	if len(baseSidecar) != 3 {
		t.Fatalf("基线侧车 blob 数 = %d，期望 3", len(baseSidecar))
	}
	if err := baseReader.Verify(true); err != nil {
		t.Fatalf("基线校验：%v", err)
	}

	// 新增一个 blob，生成相对基线的差包。
	newHashes := f.seedAssets(t, "delta-new")
	if len(newHashes) != 1 {
		t.Fatalf("新增 blob 数 = %d，期望 1", len(newHashes))
	}
	rec1, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot, BasePackageID: rec0.PackageID})
	if err != nil {
		t.Fatalf("Generate 差包：%v", err)
	}
	if !rec1.IsIncremental() {
		t.Fatal("差包应被识别为增量")
	}
	if rec1.BasePackageID != rec0.PackageID {
		t.Fatalf("差包基线 = %s，期望 %s", rec1.BasePackageID, rec0.PackageID)
	}

	r1, m1, err := archive.Open(f.svc.PackagePath(rec1.PackageID))
	if err != nil {
		t.Fatalf("Open 差包：%v", err)
	}
	if m1.Expected == nil {
		t.Fatal("差包 Expected 不应为 nil")
	}
	// 包体只携带差集 blob（1 个）。
	pkgIdx, err := r1.BlobIndex()
	if err != nil {
		t.Fatalf("读差包索引：%v", err)
	}
	if len(pkgIdx) != 1 {
		t.Fatalf("差包包体 blob 数 = %d，期望 1（只含新增）", len(pkgIdx))
	}
	if pkgIdx[0].Hash != newHashes[0] {
		t.Fatalf("差包携带的 blob = %s，期望新增 %s", pkgIdx[0].Hash, newHashes[0])
	}
	// Expected == 应用后完整集合（基线 3 ∪ 新增 1 = 4）。
	if m1.Expected.Count != 4 {
		t.Fatalf("Expected.Count = %d，期望 4", m1.Expected.Count)
	}
	// 差包侧车 == 并集（4）。
	deltaSidecar, err := f.svc.readSidecar(rec1.PackageID)
	if err != nil {
		t.Fatalf("读差包侧车：%v", err)
	}
	if len(deltaSidecar) != 4 {
		t.Fatalf("差包侧车 blob 数 = %d，期望 4（并集）", len(deltaSidecar))
	}
	if deltaSidecar.Summary() != *m1.Expected {
		t.Fatalf("差包侧车摘要与 Expected 不一致：%v != %v", deltaSidecar.Summary(), *m1.Expected)
	}
	// Expected 必须与基线侧车并集（即完整集合）一致。
	union := baseSidecar.Union(pkgIdx)
	if union.Summary() != *m1.Expected {
		t.Fatalf("基线∪差集 摘要应与 Expected 一致：%v != %v", union.Summary(), *m1.Expected)
	}
	// 完整性校验仍通过（差包是合法包）。
	if err := r1.Verify(true); err != nil {
		t.Fatalf("差包校验：%v", err)
	}
	_ = baseHashes
}

// TestBackupDeltaGenerateChained 覆盖"差包的差包"（链式）：在差包之上再派生差包。
func TestBackupDeltaGenerateChained(t *testing.T) {
	f := newBackupFixture(t)
	f.seedAssets(t, "alpha", "beta", "gamma") // 基线 3 个 blob
	if _, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot}); err != nil {
		t.Fatalf("Generate 基线：%v", err)
	}
	// 基线包 id 需已知：再查列表取最新。
	items, err := f.svc.List(10, 0)
	if err != nil {
		t.Fatalf("List：%v", err)
	}
	if len(items) == 0 {
		t.Fatal("未生成基线包")
	}
	baseID := items[0].PackageID

	// 第一层差包：新增 d1。
	f.seedAssets(t, "d1")
	recA, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot, BasePackageID: baseID})
	if err != nil {
		t.Fatalf("Generate 差包A：%v", err)
	}
	// 第二层差包：再新增 d2（基于差包A 的侧车，链式）。
	f.seedAssets(t, "d2")
	recB, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot, BasePackageID: recA.PackageID})
	if err != nil {
		t.Fatalf("Generate 差包B：%v", err)
	}
	rB, mB, err := archive.Open(f.svc.PackagePath(recB.PackageID))
	if err != nil {
		t.Fatalf("Open 差包B：%v", err)
	}
	pkgB, err := rB.BlobIndex()
	if err != nil {
		t.Fatalf("读差包B 索引：%v", err)
	}
	if len(pkgB) != 1 {
		t.Fatalf("差包B 包体 blob 数 = %d，期望 1（只含 d2）", len(pkgB))
	}
	// 完整集合 = 基线 3 ∪ d1 ∪ d2 = 5。
	if mB.Expected == nil || mB.Expected.Count != 5 {
		t.Fatalf("差包B Expected.Count = %v，期望 5", mB.Expected)
	}
	scB, err := f.svc.readSidecar(recB.PackageID)
	if err != nil {
		t.Fatalf("读差包B 侧车：%v", err)
	}
	if len(scB) != 5 {
		t.Fatalf("差包B 侧车 = %d，期望 5", len(scB))
	}
}

// TestBackupDeltaRejectsMissingBase 覆盖负路径：基线不存在 → ErrBackupBaseUnavailable，且不留下半成品包。
func TestBackupDeltaRejectsMissingBase(t *testing.T) {
	f := newBackupFixture(t)
	f.seedAssets(t, "x")
	_, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot, BasePackageID: "bk-does-not-exist"})
	if !errors.Is(err, ErrBackupBaseUnavailable) {
		t.Fatalf("基线不存在应返回 ErrBackupBaseUnavailable，实际 %v", err)
	}
	// 未进入 prepare 的记录创建流程：不应有任何包体文件（Dir 可能尚未创建）。
	entries, err := os.ReadDir(f.svc.Dir())
	if err == nil && len(entries) != 0 {
		t.Fatalf("基线缺失不应留下任何文件，实际 %d 个：%v", len(entries), entries)
	}
}

// TestBackupDeltaRejectsBaseNotDone 覆盖负路径：基线未完成（非 done）→ ErrBackupBaseUnavailable。
func TestBackupDeltaRejectsBaseNotDone(t *testing.T) {
	f := newBackupFixture(t)
	// 登记一个未完成（packing）的基线包。
	if err := f.repo.Create(repository.BackupPackage{
		PackageID: "bk-notdone", Mode: repository.BackupModeHot, Status: repository.BackupStatusPacking,
	}); err != nil {
		t.Fatalf("Create 基线：%v", err)
	}
	f.seedAssets(t, "x")
	_, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot, BasePackageID: "bk-notdone"})
	if !errors.Is(err, ErrBackupBaseUnavailable) {
		t.Fatalf("基线未完成应返回 ErrBackupBaseUnavailable，实际 %v", err)
	}
}

// TestBackupDeltaRejectsBaseSidecarMissing 覆盖负路径：基线已完成但其侧车索引缺失 →
// ErrBackupBaseUnavailable，不得静默退化成扫描整包。
func TestBackupDeltaRejectsBaseSidecarMissing(t *testing.T) {
	f := newBackupFixture(t)
	f.seedAssets(t, "alpha", "beta")
	rec0, err := f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate 基线：%v", err)
	}
	// 删掉基线侧车，模拟"侧车缺失"。
	if err := os.Remove(f.svc.SidecarPath(rec0.PackageID)); err != nil {
		t.Fatalf("删除基线侧车：%v", err)
	}
	f.seedAssets(t, "gamma")
	_, err = f.svc.Generate(context.Background(), CreateBackupOptions{Mode: archive.ModeHot, BasePackageID: rec0.PackageID})
	if !errors.Is(err, ErrBackupBaseUnavailable) {
		t.Fatalf("基线侧车缺失应返回 ErrBackupBaseUnavailable，实际 %v", err)
	}
}
