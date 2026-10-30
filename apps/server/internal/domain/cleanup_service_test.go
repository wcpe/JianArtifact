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

// TestCleanupQuarantineSkipsOperationsWithPendingRestore 钉住隔离区「宁可少收」这条安全边界。
//
// 隔离目录的一级子目录名是**资产变更操作的 operationID**。只要该操作仍有 staged 的隔离
// 记录，这份副本就是那次变更唯一的回滚依据，绝不能当垃圾收走。
//
// 变异验证方式：删掉 wiring 里的 SetPendingRestoreChecker 接线（或让判定恒假），本用例
// 必须转红——此时被引用的操作目录会被错误地列进清理候选。
func TestCleanupQuarantineSkipsOperationsWithPendingRestore(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, dataDir := newCleanupEnv(t, db)

	pendingOp := "01a16a23-0000-7000-8000-000000000001"
	idleOp := "01a16a23-0000-7000-8000-000000000002"
	writeFile(t, filepath.Join(dataDir, "blobs", "quarantine", pendingOp, "aabb"), "rollback-copy")
	writeFile(t, filepath.Join(dataDir, "blobs", "quarantine", idleOp, "ccdd"), "leftover")

	// 只有 pendingOp 存在 staged 隔离记录；idleOp 没有。
	mutations := repository.NewAssetMutationRepo(db)
	if err := mutations.Begin(pendingOp, nil); err != nil {
		t.Fatalf("建操作 intent：%v", err)
	}
	if err := mutations.MarkStaged(pendingOp, []repository.BlobQuarantine{{
		OperationID:    pendingOp,
		BlobHash:       "aabb",
		QuarantinePath: filepath.Join(dataDir, "blobs", "quarantine", pendingOp, "aabb"),
	}}); err != nil {
		t.Fatalf("写隔离记录：%v", err)
	}

	// 接线方式与生产一致：问 blob_quarantine 该操作是否仍有 staged 记录。
	svc.SetPendingRestoreChecker(func(operationID string) bool {
		pending, err := mutations.HasPendingQuarantine(operationID)
		return err != nil || pending
	})

	preview, err := svc.Preview(domain.CleanupKindQuarantine, 0)
	if err != nil {
		t.Fatalf("预览隔离区：%v", err)
	}

	if preview.ItemCount != 1 {
		t.Fatalf("候选数 = %d，期望 1（只应收 idleOp）；候选：%+v", preview.ItemCount, preview.Items)
	}
	if got := preview.Items[0].Path; got != "blobs/quarantine/"+idleOp {
		t.Fatalf("候选路径 = %q，期望只含未被引用的 %q", got, idleOp)
	}
	// 被引用的目录必须仍在磁盘上，且出现在 skipped 里。
	if _, err := os.Stat(filepath.Join(dataDir, "blobs", "quarantine", pendingOp, "aabb")); err != nil {
		t.Fatalf("被待生效操作引用的隔离副本不得被回收：%v", err)
	}
	if len(preview.Skipped) != 1 {
		t.Fatalf("跳过项数 = %d，期望 1（应如实报告被跳过的目录）", len(preview.Skipped))
	}
}

// TestAssetMutationHasPendingQuarantineTracksStatus 钉住判据本身的状态迁移。
//
// 判据必须只看 staged：一旦隔离副本被放回（restored）或进入待回收（pending_gc/deleted），
// 它已不再承担回滚职责，此时仍报「被引用」会让隔离区永远清不掉。
func TestAssetMutationHasPendingQuarantineTracksStatus(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	mutations := repository.NewAssetMutationRepo(db)

	const op = "01a16a23-0000-7000-8000-000000000003"
	if err := mutations.Begin(op, nil); err != nil {
		t.Fatalf("建操作 intent：%v", err)
	}
	if err := mutations.MarkStaged(op, []repository.BlobQuarantine{{
		OperationID: op, BlobHash: "aabb", QuarantinePath: "/tmp/x/aabb",
	}}); err != nil {
		t.Fatalf("写隔离记录：%v", err)
	}

	if pending, err := mutations.HasPendingQuarantine(op); err != nil || !pending {
		t.Fatalf("staged 状态应判为仍被引用：pending=%v err=%v", pending, err)
	}

	if err := mutations.MarkQuarantine(op, "aabb", "restored", ""); err != nil {
		t.Fatalf("标记已放回：%v", err)
	}
	if pending, err := mutations.HasPendingQuarantine(op); err != nil || pending {
		t.Fatalf("restored 后不应再判为被引用：pending=%v err=%v", pending, err)
	}
}

// TestCleanupFailureRollsBackMovedItems 钉住「失败不得留下不可见孤儿」这条一致性要求。
//
// 原实现是「先把所有项 rename 进回收站，循环结束才一次性写 DB 记录」：第 N 项失败时，
// 前面已落盘的文件没有任何 trash_entry，回收站列不出来、无法恢复，而 trash-gc 只扫
// staged 记录，也永远清不到——静默占盘且不可见。
//
// 现在改为逐项落库 + 失败回滚。本用例让**第二次**移动失败（第一次成功），
// 验证确实走了回滚路径：原目录全部回到原位、回收站不留任何条目、任务记为 failed。
func TestCleanupFailureRollsBackMovedItems(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, dataDir := newCleanupEnv(t, db)

	first := mkdir(t, filepath.Join(dataDir, "blobs", "0a", "ea"))
	second := mkdir(t, filepath.Join(dataDir, "blobs", "11", "bb"))

	// 第 2 次移动失败，模拟跨设备（EXDEV）或权限问题。
	calls := 0
	svc.SetRenameForTest(func(oldpath, newpath string) error {
		calls++
		if calls == 2 {
			return errors.New("模拟移动失败")
		}
		return os.Rename(oldpath, newpath)
	})

	task, err := svc.Execute(domain.ExecuteOptions{Kind: domain.CleanupKindEmptyDirs})
	if err != nil {
		t.Fatalf("执行清理：%v", err)
	}
	if task.Status != repository.CleanTaskFailed {
		t.Fatalf("任务状态 = %q，期望 failed", task.Status)
	}

	// 两项都必须回到原位：第一项虽然已移动成功，但整批失败后必须被搬回。
	for _, dir := range []string{first, second} {
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("失败回滚后 %s 应回到原位：%v", dir, statErr)
		}
	}
	// 回收站不留条目：否则会指向已被搬回原位的副本。
	entries, _, listErr := repository.NewCleanTaskRepo(db).ListTrash("", 100, 0)
	if listErr != nil {
		t.Fatalf("列回收站：%v", listErr)
	}
	if len(entries) != 0 {
		t.Fatalf("失败任务不得留下回收站条目，实际 %d 条：%+v", len(entries), entries)
	}
	// 回收站目录下不得残留文件。
	leftover := snapshotTree(t, filepath.Join(dataDir, domain.TrashDirName))
	if len(leftover) > 1 { // 只允许 trash 根自身
		t.Fatalf("回收站目录下残留了文件：%v", leftover)
	}
}
