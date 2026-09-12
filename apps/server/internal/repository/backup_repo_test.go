package repository

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

func openBackupTestRepo(t *testing.T) *BackupPackageRepo {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "backup.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	return NewBackupPackageRepo(db)
}

func sampleBackup(packageID string) BackupPackage {
	return BackupPackage{
		PackageID:       packageID,
		Mode:            BackupModeHot,
		Status:          BackupStatusQueued,
		Label:           "上线前",
		CountsJSON:      `{"assets":3}`,
		NodeID:          "np-1",
		AppVersion:      "0.7.1",
		DBSchemaVersion: 34,
	}
}

func TestBackupPackageRepoLifecycle(t *testing.T) {
	repo := openBackupTestRepo(t)

	if err := repo.Create(sampleBackup("bk-1")); err != nil {
		t.Fatalf("Create：%v", err)
	}
	got, err := repo.Get("bk-1")
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	if got.Mode != BackupModeHot || got.Status != BackupStatusQueued || got.Label != "上线前" {
		t.Fatalf("登记字段未持久化：%+v", got)
	}
	if got.CreatedAt == "" {
		t.Fatal("Create 应自动填充 created_at")
	}
	if got.FinishedAt != nil {
		t.Fatalf("未完成的包 finished_at 应为空：%+v", got.FinishedAt)
	}
	if got.IsIncremental() {
		t.Fatal("无基线的包不应是增量包")
	}

	if err := repo.UpdateProgress("bk-1", BackupStatusPacking, 4096, `{"assets":5}`); err != nil {
		t.Fatalf("UpdateProgress：%v", err)
	}
	if err := repo.Finish("bk-1", true, 8192, `{"assets":7}`, ""); err != nil {
		t.Fatalf("Finish：%v", err)
	}
	got, err = repo.Get("bk-1")
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	if got.Status != BackupStatusDone || got.SizeBytes != 8192 || got.CountsJSON != `{"assets":7}` {
		t.Fatalf("收尾未持久化：%+v", got)
	}
	if got.FinishedAt == nil || *got.FinishedAt == "" {
		t.Fatal("完成的包应有 finished_at")
	}
}

func TestBackupPackageRepoFinishFailureKeepsSummary(t *testing.T) {
	repo := openBackupTestRepo(t)
	if err := repo.Create(sampleBackup("bk-fail")); err != nil {
		t.Fatalf("Create：%v", err)
	}
	if err := repo.Finish("bk-fail", false, 0, "{}", "blob 读取失败"); err != nil {
		t.Fatalf("Finish：%v", err)
	}
	got, err := repo.Get("bk-fail")
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	if got.Status != BackupStatusFailed || got.ErrorSummary != "blob 读取失败" {
		t.Fatalf("失败信息未持久化：%+v", got)
	}
}

func TestBackupPackageRepoGetNotFound(t *testing.T) {
	repo := openBackupTestRepo(t)
	if _, err := repo.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的包应返回 ErrNotFound，实际 %v", err)
	}
}

func TestBackupPackageRepoListAndCount(t *testing.T) {
	repo := openBackupTestRepo(t)
	for _, id := range []string{"bk-a", "bk-b", "bk-c"} {
		if err := repo.Create(sampleBackup(id)); err != nil {
			t.Fatalf("Create %s：%v", id, err)
		}
	}
	n, err := repo.Count()
	if err != nil || n != 3 {
		t.Fatalf("Count = %d err=%v，期望 3", n, err)
	}
	items, err := repo.List(2, 0)
	if err != nil || len(items) != 2 {
		t.Fatalf("List 分页错误：len=%d err=%v", len(items), err)
	}
	rest, err := repo.List(2, 2)
	if err != nil || len(rest) != 1 {
		t.Fatalf("List 第二页错误：len=%d err=%v", len(rest), err)
	}
}

func TestBackupPackageRepoDelete(t *testing.T) {
	repo := openBackupTestRepo(t)
	if err := repo.Create(sampleBackup("bk-del")); err != nil {
		t.Fatalf("Create：%v", err)
	}
	if err := repo.Delete("bk-del"); err != nil {
		t.Fatalf("Delete：%v", err)
	}
	if _, err := repo.Get("bk-del"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应查不到，实际 %v", err)
	}
}

// TestBackupPackageRepoHasDerived 覆盖增量包对基线的依赖判定，
// 这是"删除基线前必须确认无派生包"的依据。
func TestBackupPackageRepoHasDerived(t *testing.T) {
	repo := openBackupTestRepo(t)
	base := sampleBackup("bk-base")
	if err := repo.Create(base); err != nil {
		t.Fatalf("Create base：%v", err)
	}
	derived, err := repo.HasDerived("bk-base")
	if err != nil {
		t.Fatalf("HasDerived：%v", err)
	}
	if derived {
		t.Fatal("尚无派生包时 HasDerived 应为 false")
	}

	delta := sampleBackup("bk-delta")
	delta.Mode = BackupModeFrozen
	delta.BasePackageID = "bk-base"
	if err := repo.Create(delta); err != nil {
		t.Fatalf("Create delta：%v", err)
	}
	if got, err := repo.Get("bk-delta"); err != nil || !got.IsIncremental() || got.BasePackageID != "bk-base" {
		t.Fatalf("增量包基线未持久化：%+v err=%v", got, err)
	}
	derived, err = repo.HasDerived("bk-base")
	if err != nil {
		t.Fatalf("HasDerived：%v", err)
	}
	if !derived {
		t.Fatal("存在派生包时 HasDerived 应为 true")
	}
}

// TestBackupPackageRepoMarkInterrupted 覆盖服务重启后遗留生成中包的收尾，
// 与复制孤儿轮次清理同理：不能让列表里留下永久"生成中"的幻影。
func TestBackupPackageRepoMarkInterrupted(t *testing.T) {
	repo := openBackupTestRepo(t)
	states := map[string]string{
		"bk-q": BackupStatusQueued,
		"bk-s": BackupStatusSnapshotting,
		"bk-p": BackupStatusPacking,
		"bk-d": BackupStatusDone,
		"bk-f": BackupStatusFailed,
	}
	for id, st := range states {
		p := sampleBackup(id)
		p.Status = st
		if err := repo.Create(p); err != nil {
			t.Fatalf("Create %s：%v", id, err)
		}
	}

	n, err := repo.MarkInterrupted("服务重启，轮次被中断")
	if err != nil {
		t.Fatalf("MarkInterrupted：%v", err)
	}
	if n != 3 {
		t.Fatalf("标记行数 = %d，期望 3（只应影响 queued/snapshotting/packing）", n)
	}
	for id, want := range map[string]string{
		"bk-q": BackupStatusFailed,
		"bk-s": BackupStatusFailed,
		"bk-p": BackupStatusFailed,
		"bk-d": BackupStatusDone,
		"bk-f": BackupStatusFailed,
	} {
		got, err := repo.Get(id)
		if err != nil {
			t.Fatalf("Get %s：%v", id, err)
		}
		if got.Status != want {
			t.Fatalf("%s 状态 = %s，期望 %s", id, got.Status, want)
		}
	}
	// 终态包不应被二次标记覆盖摘要。
	done, err := repo.Get("bk-d")
	if err != nil {
		t.Fatalf("Get bk-d：%v", err)
	}
	if done.ErrorSummary != "" {
		t.Fatalf("已完成包不应被写入中断摘要：%q", done.ErrorSummary)
	}
}
