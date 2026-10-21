package domain

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestRestoreKeepsImportHistory 是「失败记录被自己的恢复覆盖掉」的回归守卫。
//
// # 曾经的 bug
//
// backup_import 是被恢复的那个数据库里的一张表，恢复会把它一并回滚。于是
// "这次恢复失败了吗、为什么失败"的记录，会被**它自己的恢复动作**覆盖消失——
// 界面查不到、日志里也没有，现场完全无法定位。这真实发生过一次。
//
// # 本用例守住
//
// 恢复前存在的导入记录，恢复后必须仍能查到（从 pre-restore 快照搬回）。
func TestRestoreKeepsImportHistory(t *testing.T) {
	t.Parallel()

	source := newRestoreTargetFixture(t)
	source.seedAssetsForRestore(t, "hist-payload")
	pkgPath := source.generateRestorePackage(t)

	target := newRestoreTargetFixture(t)
	// 目标实例里先写几条历史导入记录——其中一条是 failed，模拟"刚失败过"。
	importRepo := repository.NewBackupImportRepo(target.db)
	recs := []repository.BackupImport{
		{
			ImportID:     "imp-20260101-000001-hist01",
			Origin:       repository.ImportOriginCLI,
			Status:       repository.ImportStatusFailed,
			Operator:     "tester",
			ErrorCode:    "base_missing",
			ErrorSummary: "缺少基线包",
		},
		{
			ImportID: "imp-20260101-000002-hist02",
			Origin:   repository.ImportOriginCLI,
			Status:   repository.ImportStatusDone,
			Operator: "tester",
		},
	}
	for _, r := range recs {
		if err := importRepo.Create(r); err != nil {
			t.Fatalf("建导入记录 %s：%v", r.ImportID, err)
		}
	}

	if _, err := target.svc.Stage(t.Context(), RestoreRequest{
		SourcePath: pkgPath, Origin: "upload", Operator: "tester", Overwrite: true,
	}); err != nil {
		t.Fatalf("Stage：%v", err)
	}
	_ = target.db.Close()
	applied, outcome, err := ApplyPendingRestore(target.dir, target.dbPath, filepath.Join(target.dir, "blobs"))
	if err != nil || !applied {
		t.Fatalf("ApplyPendingRestore：applied=%v err=%v", applied, err)
	}
	if outcome.ImportsRestored == 0 {
		t.Fatal("应搬回导入记录，实际 0 条")
	}

	reopened, err := persistence.Open(target.dbPath)
	if err != nil {
		t.Fatalf("重开数据库：%v", err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedRepo := repository.NewBackupImportRepo(reopened)

	// 核心断言：那条失败记录必须还在，且原因可读。
	failed, err := reopenedRepo.Get("imp-20260101-000001-hist01")
	if err != nil {
		t.Fatalf("失败记录应被搬回，实际查不到：%v", err)
	}
	if failed.Status != repository.ImportStatusFailed {
		t.Fatalf("状态应为 failed，实得 %s", failed.Status)
	}
	if failed.ErrorCode != "base_missing" {
		t.Fatalf("错误码应保留，实得 %q", failed.ErrorCode)
	}
	if !strings.Contains(failed.ErrorSummary, "基线") {
		t.Fatalf("错误原因应保留，实得 %q", failed.ErrorSummary)
	}

	// 摘要里要体现搬回数量，运维据此判断是否发生过覆盖。
	if !strings.Contains(outcome.Summary(), "搬回导入记录") {
		t.Fatalf("恢复摘要应体现搬回数量：%s", outcome.Summary())
	}
}

// TestScanReportsDuplicateInsteadOfHiding 是「文件卡在两条列表之间」的回归守卫。
//
// # 曾经的 bug
//
// Scan 用 manifest id 判断"是否已登记"，一旦已登记就 continue——那个文件从结果里
// 消失了。于是它既不在正常列表、也不在未管理列表，既不能用也不能删，永久占盘。
// 实测测试站上就卡了一个 1MB 的副本。
//
// # 本用例守住
//
// 同一份包的多余副本必须**被列出来**，标记为 valid=false + duplicate=true，
// 并给出可解释的原因。
func TestScanReportsDuplicateInsteadOfHiding(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)

	// 生成一个真实包，它会被登记。
	pkgID := f.generateRealPackage(t, "原件")

	// 再复制一份，文件名与 manifest 不同——模拟"同一份包的手工副本"。
	dupName := "bk-20260101-000000-dup001"
	src := f.svc.PackagePath(pkgID)
	dst := filepath.Join(f.svc.Dir(), dupName+".tar.gz")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读源包：%v", err)
	}
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		t.Fatalf("写副本：%v", err)
	}

	adopt := NewBackupAdoptService(f.svc, f.repo)
	items, err := adopt.Scan()
	if err != nil {
		t.Fatalf("Scan：%v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应列出 1 个候选（重复副本），实际 %d 个：%+v", len(items), items)
	}
	got := items[0]
	if got.FileID != dupName {
		t.Fatalf("候选应是副本 %s，实得 %s", dupName, got.FileID)
	}
	if got.Valid {
		t.Fatal("重复副本不应可导入（valid 必须为 false）")
	}
	if !got.Duplicate {
		t.Fatal("必须标记 duplicate=true，否则用户不知道为什么不能导入")
	}
	if !strings.Contains(got.Reason, pkgID) {
		t.Fatalf("原因里应点明与哪个已登记包重复，实得 %q", got.Reason)
	}

	// 导入它应被跳过（而不是失败或静默成功）。
	res, err := adopt.Adopt([]string{dupName})
	if err != nil {
		t.Fatalf("Adopt：%v", err)
	}
	if res.Adopted != 0 || res.Skipped != 1 {
		t.Fatalf("重复副本应被跳过（0 成功 1 跳过），实得 %+v", res)
	}
}

// TestOrphanCleanupKeepsDuplicates 覆盖与 GC 的配合：
// 重复副本的内容有效，GC 不得把它当垃圾删掉。
func TestOrphanCleanupKeepsDuplicates(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)

	pkgID := f.generateRealPackage(t, "原件")
	dupName := "bk-20260101-000000-dup002"
	src := f.svc.PackagePath(pkgID)
	dst := filepath.Join(f.svc.Dir(), dupName+".tar.gz")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读源包：%v", err)
	}
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		t.Fatalf("写副本：%v", err)
	}

	adopt := NewBackupAdoptService(f.svc, f.repo)
	svc := NewBackupOrphanCleanupService(f.svc, adopt, BackupOrphanCleanupOptions{Grace: 0})
	res, err := svc.Run()
	if err != nil {
		t.Fatalf("Run：%v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("重复副本不应被回收，实际删除 %d 个（%s）", res.Deleted, res.Summary())
	}
	if _, statErr := os.Stat(dst); statErr != nil {
		t.Fatalf("重复副本必须保留：%v", statErr)
	}
}

// TestOrphanCleanupReclaimsPreRestoreBackups 是「回滚备份只增不减」的回归守卫。
//
// # 曾经的 bug
//
// 每次恢复都会在数据目录下留一份 pre-restore-<时间戳>/ 作为回滚退路，但**从不清理**。
// 实测测试站积了 61 个目录、96MB；按 20GB 数据算，每恢复一次就多 20GB，
// 两次就能写满 40GB 的盘——而这恰恰发生在"用户已因磁盘紧张来恢复"的时刻。
//
// # 本用例守住
//
// 只保留最近 keepRecentPreRestore 个，更早的被回收；且不多删（保留那几个必须还在）。
func TestOrphanCleanupReclaimsPreRestoreBackups(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	base := f.generateRealPackage(t, "占位")

	// 造 6 个 pre-restore 目录，mtime 依次递增（越晚越新）。
	dataDir := filepath.Dir(f.svc.Dir())
	names := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("pre-restore-2026010%d-000000", i+1)
		dir := filepath.Join(dataDir, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("建目录 %s：%v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "jianartifact.db"), []byte("snapshot"), 0o600); err != nil {
			t.Fatalf("写快照 %s：%v", dir, err)
		}
		// 用 Chtimes 明确设定 mtime，避免同秒内创建导致排序不确定。
		ts := time.Now().Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(dir, ts, ts); err != nil {
			t.Fatalf("设置 mtime %s：%v", dir, err)
		}
		names = append(names, name)
	}
	_ = base

	adopt := NewBackupAdoptService(f.svc, f.repo)
	svc := NewBackupOrphanCleanupService(f.svc, adopt, BackupOrphanCleanupOptions{Grace: time.Hour})
	res, err := svc.Run()
	if err != nil {
		t.Fatalf("Run：%v", err)
	}

	if res.PreRestoreKept != keepRecentPreRestore {
		t.Fatalf("应保留 %d 个，实得 %d", keepRecentPreRestore, res.PreRestoreKept)
	}
	if res.PreRestoreDeleted != 6-keepRecentPreRestore {
		t.Fatalf("应回收 %d 个，实得 %d", 6-keepRecentPreRestore, res.PreRestoreDeleted)
	}

	// 最近 3 个（下标 3,4,5）必须还在；更早的必须已删。
	for i, name := range names {
		_, statErr := os.Stat(filepath.Join(dataDir, name))
		if i >= 6-keepRecentPreRestore {
			if statErr != nil {
				t.Fatalf("最近的 %s 必须保留：%v", name, statErr)
			}
			continue
		}
		if !os.IsNotExist(statErr) {
			t.Fatalf("较早的 %s 应已回收", name)
		}
	}

	// 摘要里要体现这件事，否则运维不知道磁盘为何变小。
	if !strings.Contains(res.Summary(), "pre-restore") {
		t.Fatalf("摘要应体现 pre-restore 回收：%s", res.Summary())
	}
}

// TestOrphanCleanupKeepsAllWhenFewPreRestore 覆盖"不足 N 个时不删"：
// 不能把仅有的几个回滚退路也删掉。
func TestOrphanCleanupKeepsAllWhenFewPreRestore(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	f.generateRealPackage(t, "占位")

	dataDir := filepath.Dir(f.svc.Dir())
	for i := 0; i < 2; i++ {
		dir := filepath.Join(dataDir, fmt.Sprintf("pre-restore-2026020%d-000000", i+1))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("建目录：%v", err)
		}
	}

	adopt := NewBackupAdoptService(f.svc, f.repo)
	svc := NewBackupOrphanCleanupService(f.svc, adopt, BackupOrphanCleanupOptions{Grace: time.Hour})
	res, err := svc.Run()
	if err != nil {
		t.Fatalf("Run：%v", err)
	}
	if res.PreRestoreDeleted != 0 {
		t.Fatalf("不足 %d 个时不应删除，实得删除 %d", keepRecentPreRestore, res.PreRestoreDeleted)
	}
	if res.PreRestoreKept != 2 {
		t.Fatalf("应保留 2 个，实得 %d", res.PreRestoreKept)
	}
}
