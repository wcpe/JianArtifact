package domain

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newDrillFixture 建一个带演练服务的夹具（复用 backupFixture 的真实库 + blobstore）。
func newDrillFixture(t *testing.T) (*backupFixture, *BackupDrillService) {
	t.Helper()
	f := newBackupFixture(t)
	drillSvc := NewBackupDrillService(f.svc, repository.NewBackupVerificationRepo(f.db), f.dataDir)
	return f, drillSvc
}

// checkByName 取出指定名的检查项结论。
func checkByName(t *testing.T, result DrillResult, name string) DrillCheck {
	t.Helper()
	for _, c := range result.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("报告里缺少检查项 %s：%+v", name, result.Checks)
	return DrillCheck{}
}

// stagingEmpty 报告演练临时目录根下是否已无残留。
func stagingEmpty(t *testing.T, svc *BackupDrillService) bool {
	t.Helper()
	entries, err := os.ReadDir(svc.StagingRoot())
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		t.Fatalf("读取演练临时目录：%v", err)
	}
	return len(entries) == 0
}

// TestBackupDrillShallowSucceeds 覆盖浅校验成功路径：四项检查全通过、报告落库、临时目录清理。
func TestBackupDrillShallowSucceeds(t *testing.T) {
	t.Parallel()
	f, drillSvc := newDrillFixture(t)
	f.seedAssets(t, "alpha", "beta", "gamma")

	rec, err := f.svc.Generate(t.Context(), CreateBackupOptions{Mode: archive.ModeHot, Origin: repository.BackupOriginAuto})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}

	result, err := drillSvc.Drill(t.Context(), rec.PackageID, DrillBackupOptions{Level: repository.VerificationLevelShallow})
	if err != nil {
		t.Fatalf("Drill：%v", err)
	}
	if !result.OK {
		t.Fatalf("浅校验应通过，实际结论=%+v 摘要=%s", result.Checks, result.ErrorSummary)
	}
	if result.ErrorSummary != "" {
		t.Fatalf("成功时不应有失败摘要：%s", result.ErrorSummary)
	}
	if result.Level != repository.VerificationLevelShallow {
		t.Fatalf("级别 = %q，期望 shallow", result.Level)
	}
	for _, name := range []string{"integrity", "extract", "sqlite", "blobs"} {
		if c := checkByName(t, result, name); !c.OK {
			t.Fatalf("检查项 %s 应通过，实际：%s", name, c.Detail)
		}
	}
	// 报告必须落库（这是"可查历史"的前提），且字段与返回值一致。
	if result.Verification.ID == 0 {
		t.Fatal("报告应已落库并回填自增 id")
	}
	items, total, err := drillSvc.ListVerifications(rec.PackageID, 10, 0)
	if err != nil {
		t.Fatalf("ListVerifications：%v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("历史报告数 = %d（列表 %d），期望 1", total, len(items))
	}
	if !items[0].OK || items[0].Level != repository.VerificationLevelShallow {
		t.Fatalf("落库报告内容不符：%+v", items[0])
	}
	if items[0].ChecksJSON == "" || items[0].ChecksJSON == "{}" {
		t.Fatalf("落库报告应带逐项结论，实际 %q", items[0].ChecksJSON)
	}
	if !stagingEmpty(t, drillSvc) {
		t.Fatal("演练结束后临时目录必须被清理")
	}
}

// TestBackupDrillDeepSucceeds 覆盖深校验成功路径（逐 blob 比对内容摘要）。
func TestBackupDrillDeepSucceeds(t *testing.T) {
	t.Parallel()
	f, drillSvc := newDrillFixture(t)
	f.seedAssets(t, "one", "two")

	rec, err := f.svc.Generate(t.Context(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	result, err := drillSvc.Drill(t.Context(), rec.PackageID, DrillBackupOptions{Level: repository.VerificationLevelDeep})
	if err != nil {
		t.Fatalf("Drill：%v", err)
	}
	if !result.OK {
		t.Fatalf("深校验应通过，实际：%+v", result.Checks)
	}
	if result.Level != repository.VerificationLevelDeep {
		t.Fatalf("级别 = %q，期望 deep", result.Level)
	}
	if c := checkByName(t, result, "integrity"); !c.OK {
		t.Fatalf("完整性检查应通过：%s", c.Detail)
	}
	if !stagingEmpty(t, drillSvc) {
		t.Fatal("演练结束后临时目录必须被清理")
	}
}

// TestBackupDrillSucceedsOnPackageWithoutBlobs 覆盖"空实例的包"（零 blob）。
//
// 这是实机冒烟发现的缺陷回归：Reader.Extract 只为真实存在的 blob 条目建目录，
// 因此零 blob 的包解包后根本没有 blobs/ 子目录。若把"目录不存在"当遍历错误，
// 一个没有任何资产的实例生成的合法备份包会永远演练失败——
// 而它恰恰是最该通过的一种包（空库能恢复，才有资格谈非空库）。
func TestBackupDrillSucceedsOnPackageWithoutBlobs(t *testing.T) {
	t.Parallel()
	f, drillSvc := newDrillFixture(t)
	// 刻意不 seedAssets：库是刚迁移完的空实例。

	rec, err := f.svc.Generate(t.Context(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	result, err := drillSvc.Drill(t.Context(), rec.PackageID, DrillBackupOptions{Level: repository.VerificationLevelDeep})
	if err != nil {
		t.Fatalf("Drill：%v", err)
	}
	if !result.OK {
		t.Fatalf("零 blob 的包应演练通过，实际：%+v", result.Checks)
	}
	if c := checkByName(t, result, "blobs"); !c.OK {
		t.Fatalf("零 blob 不应被判为失败：%s", c.Detail)
	}
	if !stagingEmpty(t, drillSvc) {
		t.Fatal("演练结束后临时目录必须被清理")
	}
}

// TestBackupDrillReportsCorruptedPackage 覆盖损坏包：报告必须如实标失败，
// 且仍然落库（失败报告正是演练最该留下的东西），临时目录仍被清理。
func TestBackupDrillReportsCorruptedPackage(t *testing.T) {
	t.Parallel()
	f, drillSvc := newDrillFixture(t)
	f.seedAssets(t, "payload")

	rec, err := f.svc.Generate(t.Context(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}

	// 人为损坏包体：截断到一半（gzip 流因此不完整）。
	path := f.svc.PackagePath(rec.PackageID)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 包体：%v", err)
	}
	if err := os.Truncate(path, info.Size()/2); err != nil {
		t.Fatalf("截断包体：%v", err)
	}

	result, err := drillSvc.Drill(t.Context(), rec.PackageID, DrillBackupOptions{Level: repository.VerificationLevelShallow})
	if err != nil {
		t.Fatalf("演练本身应正常完成（失败结论不是执行错误）：%v", err)
	}
	if result.OK {
		t.Fatal("损坏包不得判为通过")
	}
	if result.ErrorSummary == "" {
		t.Fatal("失败报告必须写明原因，不得粉饰")
	}
	if c := checkByName(t, result, "integrity"); c.OK {
		t.Fatalf("损坏包的完整性检查不得通过：%s", c.Detail)
	}
	// 报告仍要落库（失败也要留痕）。
	if result.Verification.ID == 0 || result.Verification.OK {
		t.Fatalf("失败报告应落库且标记失败：%+v", result.Verification)
	}
	if result.Verification.ErrorSummary == "" {
		t.Fatal("落库报告应带失败原因")
	}
	if !stagingEmpty(t, drillSvc) {
		t.Fatal("演练失败后临时目录同样必须被清理")
	}
}

// TestBackupDrillRejectsIncompletePackage 覆盖前置校验：包未完成时不做演练、不写报告。
func TestBackupDrillRejectsIncompletePackage(t *testing.T) {
	t.Parallel()
	f, drillSvc := newDrillFixture(t)
	if err := f.repo.Create(repository.BackupPackage{
		PackageID: "bk-packing", Mode: repository.BackupModeHot, Status: repository.BackupStatusPacking,
	}); err != nil {
		t.Fatalf("登记生成中包：%v", err)
	}
	if _, err := drillSvc.Drill(t.Context(), "bk-packing", DrillBackupOptions{}); err == nil {
		t.Fatal("生成中的包不应可演练")
	}
	if _, err := drillSvc.Drill(t.Context(), "bk-missing", DrillBackupOptions{}); err == nil {
		t.Fatal("不存在的包不应可演练")
	}
	// 两级前置失败都不该留下报告或临时目录。
	verifyRepo := repository.NewBackupVerificationRepo(f.db)
	if n, err := verifyRepo.CountByPackage("bk-packing"); err != nil || n != 0 {
		t.Fatalf("前置失败不应写报告：n=%d err=%v", n, err)
	}
	if !stagingEmpty(t, drillSvc) {
		t.Fatal("前置失败不应留下临时目录")
	}
}

// TestBackupDrillRejectsUnknownLevel 覆盖级别校验。
func TestBackupDrillRejectsUnknownLevel(t *testing.T) {
	t.Parallel()
	f, drillSvc := newDrillFixture(t)
	f.seedAssets(t, "payload")
	rec, err := f.svc.Generate(t.Context(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	if _, err := drillSvc.Drill(t.Context(), rec.PackageID, DrillBackupOptions{Level: "medium"}); err == nil {
		t.Fatal("未知级别应被拒绝")
	}
}

// TestBackupDrillNeverTouchesRestoreMarkers 是本条最关键的回归：
// 演练必须是只读路径，绝不写 restore.pending / restore-staging/——
// 那两个是"下次重启触发真替换"的副作用，被演练触发会污染节点。
//
// 用例对**成功与失败两条路径**都断言，因为失败路径最容易在清理逻辑里误加写操作。
func TestBackupDrillNeverTouchesRestoreMarkers(t *testing.T) {
	t.Parallel()
	f, drillSvc := newDrillFixture(t)
	f.seedAssets(t, "payload")

	restore := NewRestoreService(f.db, f.dataDir, f.dbPath, filepath.Join(f.dataDir, "blobs"))

	assertNoRestoreMarkers := func(t *testing.T, stage string) {
		t.Helper()
		if _, err := os.Stat(restore.PendingPath()); err == nil {
			t.Fatalf("%s：演练不得产生 restore.pending（%s）", stage, restore.PendingPath())
		} else if !os.IsNotExist(err) {
			t.Fatalf("%s：检查 restore.pending：%v", stage, err)
		}
		if _, err := os.Stat(restore.StagingDir()); err == nil {
			t.Fatalf("%s：演练不得创建 restore-staging/（%s）", stage, restore.StagingDir())
		} else if !os.IsNotExist(err) {
			t.Fatalf("%s：检查 restore-staging：%v", stage, err)
		}
		if pending, err := restore.MarkedPending(); err != nil || pending != nil {
			t.Fatalf("%s：不得存在待生效恢复标记：pending=%+v err=%v", stage, pending, err)
		}
	}

	// ① 成功路径。
	rec, err := f.svc.Generate(t.Context(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	result, err := drillSvc.Drill(t.Context(), rec.PackageID, DrillBackupOptions{Level: repository.VerificationLevelDeep})
	if err != nil {
		t.Fatalf("Drill：%v", err)
	}
	if !result.OK {
		t.Fatalf("演练应通过：%+v", result.Checks)
	}
	assertNoRestoreMarkers(t, "成功演练后")

	// ② 失败路径（包损坏）。
	if err := os.Truncate(f.svc.PackagePath(rec.PackageID), 8); err != nil {
		t.Fatalf("截断包体：%v", err)
	}
	if _, err := drillSvc.Drill(t.Context(), rec.PackageID, DrillBackupOptions{Level: repository.VerificationLevelShallow}); err != nil {
		t.Fatalf("演练本身应正常完成：%v", err)
	}
	assertNoRestoreMarkers(t, "失败演练后")

	// ③ 数据目录里除演练自己的临时目录根外，不得出现 restore 相关目录。
	entries, err := os.ReadDir(f.dataDir)
	if err != nil {
		t.Fatalf("读取数据目录：%v", err)
	}
	for _, e := range entries {
		switch e.Name() {
		case restoreStagingDirName, restorePendingFile, "pre-restore":
			t.Fatalf("演练后数据目录出现了恢复产物：%s", e.Name())
		}
	}
}

// TestBackupDrillStagingIsOutsideRestoreNamespace 覆盖隔离目录的位置：
// 演练临时目录必须与恢复暂存目录是两个不同的路径，不能复用同一个目录名。
func TestBackupDrillStagingIsOutsideRestoreNamespace(t *testing.T) {
	t.Parallel()
	f, drillSvc := newDrillFixture(t)
	restore := NewRestoreService(f.db, f.dataDir, f.dbPath, filepath.Join(f.dataDir, "blobs"))
	if drillSvc.StagingRoot() == restore.StagingDir() {
		t.Fatalf("演练临时目录不得与恢复暂存目录同路径：%s", drillSvc.StagingRoot())
	}
	if filepath.Base(drillSvc.StagingRoot()) != drillStagingDirName {
		t.Fatalf("演练临时目录名 = %q，期望 %q", filepath.Base(drillSvc.StagingRoot()), drillStagingDirName)
	}
}

// TestBackupDrillHistoryKeepsEveryRun 覆盖历史留痕：多次演练各自留一条报告，最近优先。
func TestBackupDrillHistoryKeepsEveryRun(t *testing.T) {
	t.Parallel()
	f, drillSvc := newDrillFixture(t)
	f.seedAssets(t, "payload")
	rec, err := f.svc.Generate(t.Context(), CreateBackupOptions{Mode: archive.ModeHot})
	if err != nil {
		t.Fatalf("Generate：%v", err)
	}
	for _, level := range []string{repository.VerificationLevelShallow, repository.VerificationLevelDeep} {
		if _, err := drillSvc.Drill(t.Context(), rec.PackageID, DrillBackupOptions{Level: level}); err != nil {
			t.Fatalf("Drill(%s)：%v", level, err)
		}
	}
	items, total, err := drillSvc.ListVerifications(rec.PackageID, 10, 0)
	if err != nil {
		t.Fatalf("ListVerifications：%v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("历史报告数 = %d（列表 %d），期望 2", total, len(items))
	}
	// 分页：只取 1 条时 total 仍是 2（总数与页大小无关）。
	page, total, err := drillSvc.ListVerifications(rec.PackageID, 1, 0)
	if err != nil {
		t.Fatalf("ListVerifications 分页：%v", err)
	}
	if len(page) != 1 || total != 2 {
		t.Fatalf("分页结果 = %d 条 / 总数 %d，期望 1 / 2", len(page), total)
	}
}

// TestBackupDrillListRejectsUnknownPackage 覆盖历史查询的前置校验：
// 不存在的包应报"包不存在"，而不是返回空列表（空列表会让前端以为"从没演练过"）。
func TestBackupDrillListRejectsUnknownPackage(t *testing.T) {
	t.Parallel()
	_, drillSvc := newDrillFixture(t)
	if _, _, err := drillSvc.ListVerifications("bk-nope", 10, 0); err == nil {
		t.Fatal("不存在的包应报错")
	}
}
