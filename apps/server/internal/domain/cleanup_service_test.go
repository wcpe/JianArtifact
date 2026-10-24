package domain_test

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newCleanupEnv 建一个数据目录骨架与清理服务。
func newCleanupEnv(t *testing.T, db *persistence.DB) (*domain.CleanupService, string) {
	t.Helper()
	dataDir := t.TempDir()
	return domain.NewCleanupService(dataDir, repository.NewCleanTaskRepo(db)), dataDir
}

// mkdir 建目录（含父级），返回绝对路径。
func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("建目录 %s：%v", path, err)
	}
	return path
}

// writeFile 写文件（含父级）。
func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("建父目录 %s：%v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("写文件 %s：%v", path, err)
	}
	return path
}

// TestCleanupPreviewIsReadOnly 钉住「预览绝不删东西」这条硬不变量。
//
// 这是本批最关键的一条：预览是用户用来判断"要不要清理"的唯一依据，如果它自己就动了磁盘，
// 那么"先预览看看"这个动作本身就是一次未经确认的删除。
func TestCleanupPreviewIsReadOnly(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, dataDir := newCleanupEnv(t, db)

	emptyDir := mkdir(t, filepath.Join(dataDir, "blobs", "0a", "ea"))
	keepFile := writeFile(t, filepath.Join(dataDir, "blobs", "11", "bb", "content"), "payload")

	before := snapshotTree(t, dataDir)
	preview, err := svc.Preview(domain.CleanupKindEmptyDirs, 0)
	if err != nil {
		t.Fatalf("预览：%v", err)
	}
	after := snapshotTree(t, dataDir)

	if !equalStringSlices(before, after) {
		t.Fatalf("预览不得改动磁盘\n前：%v\n后：%v", before, after)
	}
	if preview.ItemCount != 1 {
		t.Fatalf("空目录数 = %d，期望 1", preview.ItemCount)
	}
	if preview.Items[0].Path != "blobs/0a/ea" {
		t.Fatalf("候选路径 = %q，期望 blobs/0a/ea（对外只暴露相对路径）", preview.Items[0].Path)
	}
	// 空目录不应被误算进"含文件的叶子目录"。
	if _, err := os.Stat(emptyDir); err != nil {
		t.Fatalf("空目录应仍存在：%v", err)
	}
	if _, err := os.Stat(keepFile); err != nil {
		t.Fatalf("业务文件应仍存在：%v", err)
	}
}

// TestCleanupMovesToTrashAndIsRestorable 钉住「清理 = 移入回收站」而非物理删除。
//
// 变异验证方式：把 moveToTrash 里的 os.Rename 换成 os.RemoveAll，本用例必须转红
// （原路径消失但回收站里什么都没有，restore 无从恢复）。
func TestCleanupMovesToTrashAndIsRestorable(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, dataDir := newCleanupEnv(t, db)

	session := mkdir(t, filepath.Join(dataDir, "backup-uploads", "up-1", "chunks"))
	writeFile(t, filepath.Join(session, "0"), "chunk-0")
	svc.SetBackupUploadExpiredSource(func() ([]string, error) { return []string{"up-1"}, nil })

	task, err := svc.Execute(domain.ExecuteOptions{Kind: domain.CleanupKindStaleUpload})
	if err != nil {
		t.Fatalf("执行清理：%v", err)
	}
	if task.Status != repository.CleanTaskSucceeded {
		t.Fatalf("任务状态 = %q，期望 succeeded", task.Status)
	}
	if task.ProcessedItems != 1 {
		t.Fatalf("处理项数 = %d，期望 1", task.ProcessedItems)
	}
	// 原路径必须消失，且内容已转移到回收站。
	if _, statErr := os.Stat(session); !os.IsNotExist(statErr) {
		t.Fatalf("原会话目录应已移走，实际 err=%v", statErr)
	}
	entries, _, err := repository.NewCleanTaskRepo(db).ListTrash("", 10, 0)
	if err != nil {
		t.Fatalf("列回收站：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("回收站条目数 = %d，期望 1", len(entries))
	}
	if _, statErr := os.Stat(entries[0].TrashPath); statErr != nil {
		t.Fatalf("回收站副本应存在：%v", statErr)
	}
	// 恢复：把内容放回原路径后必须与原内容一致。
	if err := os.Rename(entries[0].TrashPath, entries[0].OriginalPath); err != nil {
		t.Fatalf("恢复：%v", err)
	}
	got, err := os.ReadFile(filepath.Join(entries[0].OriginalPath, "chunks", "0"))
	if err != nil {
		t.Fatalf("读恢复后的分片：%v", err)
	}
	if string(got) != "chunk-0" {
		t.Fatalf("恢复后内容 = %q，期望 chunk-0", string(got))
	}
}

// TestCleanupFiltersOutNonCandidates 钉住「回传项不在候选集内 → 整批拒绝」。
//
// 部分执行会让用户以为预览清单被完整执行，而漏掉的项毫无提示——因此宁可整批失败。
func TestCleanupFiltersOutNonCandidates(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, dataDir := newCleanupEnv(t, db)

	writeFile(t, filepath.Join(dataDir, "blobs", "aa", "bb", "keep"), "x")

	_, err := svc.Execute(domain.ExecuteOptions{
		Kind:  domain.CleanupKindEmptyDirs,
		Items: []string{"blobs/aa/bb/keep"}, // 含文件的目录，不在候选集
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("不在候选集的路径应整批拒绝，实际：%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "blobs", "aa", "bb", "keep")); statErr != nil {
		t.Fatalf("拒绝后业务文件不得被动过：%v", statErr)
	}
}

// TestCleanupPreRestoreKeepsNewest 钉住「pre-restore 永远保留最新一个」。
//
// 即使 keepLast=0 也必须留一个：回滚备份是恢复失败时唯一的退路。
func TestCleanupPreRestoreKeepsNewest(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, dataDir := newCleanupEnv(t, db)

	for _, name := range []string{"pre-restore-20260101-000000", "pre-restore-20260102-000000", "pre-restore-20260103-000000"} {
		writeFile(t, filepath.Join(dataDir, name, "jianartifact.db"), name)
	}

	preview, err := svc.Preview(domain.CleanupKindPreRestore, 0)
	if err != nil {
		t.Fatalf("预览：%v", err)
	}
	// 3 个目录、保底留 1 个 → 候选 2 个，且必须是最旧的两个。
	if preview.ItemCount != 2 {
		t.Fatalf("候选数 = %d，期望 2", preview.ItemCount)
	}
	got := []string{preview.Items[0].Path, preview.Items[1].Path}
	sort.Strings(got)
	want := []string{"pre-restore-20260101-000000", "pre-restore-20260102-000000"}
	if !equalStringSlices(got, want) {
		t.Fatalf("候选 = %v，期望 %v（最旧两个）", got, want)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "pre-restore-20260103-000000")); statErr != nil {
		t.Fatalf("最新一个必须保留：%v", statErr)
	}
}

// TestCleanupRejectsPathEscape 钉住白名单逃逸防护：绝对路径与 `..` 一律拒绝。
func TestCleanupRejectsPathEscape(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, dataDir := newCleanupEnv(t, db)

	outside := writeFile(t, filepath.Join(dataDir, "outside.txt"), "must-survive")

	for _, bad := range []string{"../outside.txt", "blobs/../../outside.txt", outside} {
		if _, err := svc.Execute(domain.ExecuteOptions{
			Kind:  domain.CleanupKindEmptyDirs,
			Items: []string{bad},
		}); err == nil {
			t.Fatalf("路径 %q 应被拒绝", bad)
		}
	}
	if _, statErr := os.Stat(outside); statErr != nil {
		t.Fatalf("白名单外文件不得被动过：%v", statErr)
	}
}

// TestCleanupSkipsSymlink 钉住「不跟随符号链接」：指向根外的软链必须被跳过。
func TestCleanupSkipsSymlink(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, dataDir := newCleanupEnv(t, db)

	outsideDir := t.TempDir()
	writeFile(t, filepath.Join(outsideDir, "precious"), "keep-me")
	linkPath := filepath.Join(dataDir, "blobs", "aa", "bb")
	mkdir(t, filepath.Dir(linkPath))
	if err := os.Symlink(outsideDir, linkPath); err != nil {
		t.Skipf("当前平台不支持符号链接：%v", err)
	}

	if _, err := svc.Execute(domain.ExecuteOptions{
		Kind:  domain.CleanupKindEmptyDirs,
		Items: []string{"blobs/aa/bb"},
	}); err == nil {
		t.Fatal("符号链接应被拒绝")
	}
	if _, statErr := os.Stat(filepath.Join(outsideDir, "precious")); statErr != nil {
		t.Fatalf("根外文件不得被动过：%v", statErr)
	}
}

// snapshotTree 记录目录树里所有路径（排序后），用于断言"磁盘未被改动"。
func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatalf("遍历 %s：%v", root, err)
	}
	sort.Strings(paths)
	return paths
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
