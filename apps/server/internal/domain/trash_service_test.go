package domain_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newTrashEnv 建一个带一条回收站条目的环境：返回服务与原始/回收站路径。
func newTrashEnv(t *testing.T, db *persistence.DB, content string) (svc *domain.TrashService,
	cleanup *domain.CleanupService, taskRepo *repository.CleanTaskRepo, original, trashPath string) {
	t.Helper()
	dataDir := t.TempDir()
	taskRepo = repository.NewCleanTaskRepo(db)
	svc = domain.NewTrashService(dataDir, taskRepo)
	cleanup = domain.NewCleanupService(dataDir, taskRepo)

	// 造一个"中断上传会话"并把它移入回收站，得到真实条目。
	session := filepath.Join(dataDir, "backup-uploads", "up-1")
	writeFile(t, filepath.Join(session, "chunk-0"), content)
	cleanup.SetBackupUploadExpiredSource(func() ([]string, error) { return []string{"up-1"}, nil })
	if _, err := cleanup.Execute(domain.ExecuteOptions{Kind: domain.CleanupKindStaleUpload}); err != nil {
		t.Fatalf("执行清理：%v", err)
	}
	entries, _, err := taskRepo.ListTrash(repository.TrashStaged, 10, 0)
	if err != nil {
		t.Fatalf("列回收站：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("回收站条目数 = %d，期望 1", len(entries))
	}
	return svc, cleanup, taskRepo, entries[0].OriginalPath, entries[0].TrashPath
}

// TestTrashRestoreBringsContentBack 钉住「恢复把内容放回原位置」。
func TestTrashRestoreBringsContentBack(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, _, taskRepo, original, _ := newTrashEnv(t, db, "chunk-payload")

	entries, _, _ := taskRepo.ListTrash(repository.TrashStaged, 10, 0)
	result, err := svc.Restore([]int64{entries[0].ID})
	if err != nil {
		t.Fatalf("恢复：%v", err)
	}
	if result.Restored != 1 {
		t.Fatalf("恢复数 = %d，期望 1", result.Restored)
	}
	got, err := os.ReadFile(filepath.Join(original, "chunk-0"))
	if err != nil {
		t.Fatalf("读恢复后的文件：%v", err)
	}
	if string(got) != "chunk-payload" {
		t.Fatalf("内容 = %q，期望 chunk-payload", string(got))
	}
	// 恢复后不再出现在 staged 列表里。
	remaining, total, _ := taskRepo.ListTrash(repository.TrashStaged, 10, 0)
	if total != 0 || len(remaining) != 0 {
		t.Fatalf("恢复后 staged 应为空，实际 %d 条", total)
	}
}

// TestTrashRestoreNeverOverwrites 钉住「目标已存在 → 跳过，绝不覆盖」。
//
// 这是本批最重要的一条：恢复的语义是"把东西拿回来"，若它顺手覆盖了目标位置，
// 就等于以恢复之名执行了一次删除，且回收站里的副本状态还会被标成已恢复——
// 两处都没了，用户无从察觉。
func TestTrashRestoreNeverOverwrites(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, _, taskRepo, original, _ := newTrashEnv(t, db, "trash-copy")

	// 在目标位置放一个新内容（模拟"清理后又被重新写入"）。
	writeFile(t, filepath.Join(original, "chunk-0"), "newer-content")

	entries, _, _ := taskRepo.ListTrash(repository.TrashStaged, 10, 0)
	result, err := svc.Restore([]int64{entries[0].ID})
	if err != nil {
		t.Fatalf("恢复：%v", err)
	}
	if result.Restored != 0 {
		t.Fatalf("目标已存在时必须跳过，实际恢复 %d 条", result.Restored)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("应报 1 条冲突，实际 %d 条", len(result.Conflicts))
	}
	// 目标内容必须保持"新写的那份"。
	got, err := os.ReadFile(filepath.Join(original, "chunk-0"))
	if err != nil {
		t.Fatalf("读目标文件：%v", err)
	}
	if string(got) != "newer-content" {
		t.Fatalf("目标内容被覆盖了，实际 %q", string(got))
	}
	// 回收站里的副本必须还在（未被标记为已恢复）。
	staged, total, _ := taskRepo.ListTrash(repository.TrashStaged, 10, 0)
	if total != 1 || len(staged) != 1 {
		t.Fatalf("冲突后条目应仍留在回收站，实际 %d 条", total)
	}
}

// TestTrashPurgeOnlyRemovesTrashCopy 钉住「彻底删除只删回收站副本，不碰原位置」。
func TestTrashPurgeOnlyRemovesTrashCopy(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, _, taskRepo, original, trashPath := newTrashEnv(t, db, "payload")

	// 原位置此时不存在（已被移走）。额外在**原位置**造一份同名内容，
	// 用来证明 purge 不会"顺手清理原位置"。
	writeFile(t, filepath.Join(original, "chunk-0"), "should-survive-purge")

	entries, _, _ := taskRepo.ListTrash(repository.TrashStaged, 10, 0)
	result, err := svc.Purge([]int64{entries[0].ID})
	if err != nil {
		t.Fatalf("彻底删除：%v", err)
	}
	if result.Purged != 1 {
		t.Fatalf("彻底删除数 = %d，期望 1", result.Purged)
	}
	if _, statErr := os.Stat(trashPath); !os.IsNotExist(statErr) {
		t.Fatalf("回收站副本应已删除，实际 err=%v", statErr)
	}
	got, err := os.ReadFile(filepath.Join(original, "chunk-0"))
	if err != nil {
		t.Fatalf("原位置内容不得被 purge 动过：%v", err)
	}
	if string(got) != "should-survive-purge" {
		t.Fatalf("原位置内容被改动了，实际 %q", string(got))
	}
}

// TestTrashPurgeAllEmptiesStaged 钉住「清空回收站」不留点不动的僵尸条目。
func TestTrashPurgeAllEmptiesStaged(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, _, taskRepo, _, _ := newTrashEnv(t, db, "payload")

	result, err := svc.PurgeAll()
	if err != nil {
		t.Fatalf("清空回收站：%v", err)
	}
	if result.Purged != 1 {
		t.Fatalf("清空条数 = %d，期望 1", result.Purged)
	}
	remaining, total, _ := taskRepo.ListTrash(repository.TrashStaged, 10, 0)
	if total != 0 || len(remaining) != 0 {
		t.Fatalf("清空后不应残留 staged 条目，实际 %d 条", total)
	}
}

// TestTrashRetentionZeroMeansNoAutoCleanup 钉住「保留期 0 = 不自动清理」。
//
// 把 0 解释成"立刻全删"会让一次配置笔误清空整个回收站；"不清理"最坏只是多占磁盘。
func TestTrashRetentionZeroMeansNoAutoCleanup(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, _, taskRepo, _, _ := newTrashEnv(t, db, "payload")

	result, err := svc.ReconcileExpired(0)
	if err != nil {
		t.Fatalf("按保留期回收：%v", err)
	}
	if result.Purged != 0 {
		t.Fatalf("保留期 0 时不应删除任何条目，实际 %d 条", result.Purged)
	}
	_, total, _ := taskRepo.ListTrash(repository.TrashStaged, 10, 0)
	if total != 1 {
		t.Fatalf("条目应仍在回收站，实际 %d 条", total)
	}
}

// TestTrashCapacityGuardBlocksCleanup 钉住容量护栏：回收站超上限时**拒绝**新清理，而不是淘汰。
func TestTrashCapacityGuardBlocksCleanup(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	dataDir := t.TempDir()
	taskRepo := repository.NewCleanTaskRepo(db)
	cleanup := domain.NewCleanupService(dataDir, taskRepo)
	cleanup.SetTrashCapacity(1) // 极小上限：只要有 1 字节就把路堵上

	// 先制造一批可回收内容并执行一次，让回收站有占用。
	writeFile(t, filepath.Join(dataDir, "backup-uploads", "up-1", "chunk"), "12345")
	cleanup.SetBackupUploadExpiredSource(func() ([]string, error) { return []string{"up-1"}, nil })
	if _, err := cleanup.Execute(domain.ExecuteOptions{Kind: domain.CleanupKindStaleUpload}); err != nil {
		t.Fatalf("首次清理：%v", err)
	}

	// 再造一批，此时应被容量护栏拒绝。
	writeFile(t, filepath.Join(dataDir, "backup-uploads", "up-2", "chunk"), "67890")
	cleanup.SetBackupUploadExpiredSource(func() ([]string, error) { return []string{"up-2"}, nil })
	if _, err := cleanup.Execute(domain.ExecuteOptions{Kind: domain.CleanupKindStaleUpload}); err == nil {
		t.Fatal("回收站超上限时应拒绝新清理")
	}
	// 被拒绝的第二批必须原样留在原地（不能被"部分收进去"）。
	if _, statErr := os.Stat(filepath.Join(dataDir, "backup-uploads", "up-2", "chunk")); statErr != nil {
		t.Fatalf("被拒绝的清理不得动到文件：%v", statErr)
	}
}

// TestTrashExpiredRetentionPurgesOld 钉住保留期回收：只有超过保留期的条目被删。
func TestTrashExpiredRetentionPurgesOld(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, _, taskRepo, _, _ := newTrashEnv(t, db, "payload")

	// 把条目改成 30 天前。
	if _, err := db.Exec(`UPDATE trash_entry SET created_at = datetime('now', '-30 days')`); err != nil {
		t.Fatalf("回填时间：%v", err)
	}
	result, err := svc.ReconcileExpired(7 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("按保留期回收：%v", err)
	}
	if result.Purged != 1 {
		t.Fatalf("超过 7 天的条目应被回收，实际 %d 条", result.Purged)
	}
	_, total, _ := taskRepo.ListTrash(repository.TrashStaged, 10, 0)
	if total != 0 {
		t.Fatalf("回收后不应残留 staged 条目，实际 %d 条", total)
	}
}
