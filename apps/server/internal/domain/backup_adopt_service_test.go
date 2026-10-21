package domain

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newAdoptFixture 建一个带未管理包服务的夹具，并允许固定"当前时间"。
func newAdoptFixture(t *testing.T) (*backupFixture, *BackupAdoptService) {
	t.Helper()
	f := newBackupFixture(t)
	return f, NewBackupAdoptService(f.svc, f.repo)
}

// generateRealPackage 生成一个真实备份包，返回它的标识。
// 未管理包判定需要能读出 manifest 的合法归档，不能只造登记行。
func (f *backupFixture) generateRealPackage(t *testing.T, label string) string {
	t.Helper()
	f.seedAssets(t, "adopt-payload-"+label)
	rec, err := f.svc.Generate(t.Context(), CreateBackupOptions{Mode: archive.ModeHot, Label: label})
	if err != nil {
		t.Fatalf("生成备份包：%v", err)
	}
	return rec.PackageID
}

// forgetPackage 模拟"文件在、登记没了"的失步：只删登记行，保留磁盘文件。
// 这正是历史上孤儿包的成因（Delete 只走先删文件后删登记，正常路径不产生它）。
func (f *backupFixture) forgetPackage(t *testing.T, packageID string) {
	t.Helper()
	if err := f.repo.Delete(packageID); err != nil {
		t.Fatalf("删除登记 %s：%v", packageID, err)
	}
}

// TestAdoptScanFindsOnlyUnmanaged 覆盖差集口径：
// 已登记的包不出现，失步后的包出现，且被判定为合法可导入。
func TestAdoptScanFindsOnlyUnmanaged(t *testing.T) {
	t.Parallel()
	f, adopt := newAdoptFixture(t)

	managed := f.generateRealPackage(t, "仍在管理")
	orphan := f.generateRealPackage(t, "已被遗忘")
	f.forgetPackage(t, orphan)

	items, err := adopt.Scan()
	if err != nil {
		t.Fatalf("Scan：%v", err)
	}
	if len(items) != 1 {
		t.Fatalf("未管理包应有 1 个，实际 %d 个：%+v", len(items), items)
	}
	got := items[0]
	if got.FileID != orphan {
		t.Fatalf("未管理包应是 %s，实际 %s", orphan, got.FileID)
	}
	if got.FileID == managed {
		t.Fatalf("已登记的包不应出现在未管理列表里")
	}
	if !got.Valid {
		t.Fatalf("真实包应判定为合法，实际 reason=%q", got.Reason)
	}
	if got.PackageID != orphan {
		t.Fatalf("迁移后 packageId 应取自 manifest：期望 %s 实际 %s", orphan, got.PackageID)
	}
	if !got.HasSidecar {
		t.Fatalf("生成流程应同时写出侧车索引")
	}
	if got.SizeBytes <= 0 {
		t.Fatalf("包体字节数应大于 0，实际 %d", got.SizeBytes)
	}
}

// TestAdoptRebuildsRegistration 覆盖核心动作：导入后包出现在登记表里并可正常读取。
func TestAdoptRebuildsRegistration(t *testing.T) {
	t.Parallel()
	f, adopt := newAdoptFixture(t)

	orphan := f.generateRealPackage(t, "待导入")
	f.forgetPackage(t, orphan)

	res, err := adopt.Adopt([]string{orphan})
	if err != nil {
		t.Fatalf("Adopt：%v", err)
	}
	if res.Adopted != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("导入结果应为 1 成功 0 跳过 0 失败，实际 %+v（%s）", res, res.Summary())
	}

	rec, err := f.repo.Get(orphan)
	if err != nil {
		t.Fatalf("导入后应能读到登记：%v", err)
	}
	if rec.Status != repository.BackupStatusDone {
		t.Fatalf("登记状态应为 done，实际 %s", rec.Status)
	}
	// 来源必须是 manual：标成 auto 会让保留策略把这个包当成周期备份淘汰掉。
	if rec.Origin != repository.BackupOriginManual {
		t.Fatalf("导入包的来源应为 manual，实际 %s", rec.Origin)
	}
	// 导入后不再出现在未管理列表里。
	items, err := adopt.Scan()
	if err != nil {
		t.Fatalf("Scan：%v", err)
	}
	if len(items) != 0 {
		t.Fatalf("导入后未管理列表应为空，实际 %+v", items)
	}
}

// TestAdoptIsIdempotent 覆盖重复导入：第二次应计为跳过而不是报错或重复登记。
func TestAdoptIsIdempotent(t *testing.T) {
	t.Parallel()
	f, adopt := newAdoptFixture(t)

	orphan := f.generateRealPackage(t, "幂等")
	f.forgetPackage(t, orphan)

	if _, err := adopt.Adopt([]string{orphan}); err != nil {
		t.Fatalf("首次导入：%v", err)
	}
	res, err := adopt.Adopt([]string{orphan})
	if err != nil {
		t.Fatalf("重复导入不应报错：%v", err)
	}
	if res.Adopted != 0 || res.Skipped != 1 {
		t.Fatalf("重复导入应计为跳过，实际 %+v", res)
	}
	if res.Items[0].Reason == "" {
		t.Fatalf("跳过必须有原因说明")
	}
}

// TestAdoptRejectsInvalidAndTraversal 覆盖两类拒绝：
// 非本产品的归档不导入，且带路径分隔符的标识被直接拒绝（防越出备份目录）。
func TestAdoptRejectsInvalidAndTraversal(t *testing.T) {
	t.Parallel()
	f, adopt := newAdoptFixture(t)

	// 放一个不是备份包的 tar.gz（内容任意）。
	// 夹具未必生成过包，backups/ 可能尚不存在，先确保目录在。
	if err := os.MkdirAll(f.svc.Dir(), 0o750); err != nil {
		t.Fatalf("建备份目录：%v", err)
	}
	fakeID := "bk-20260101-000000-fake01"
	fakePath := filepath.Join(f.svc.Dir(), fakeID+".tar.gz")
	if err := os.WriteFile(fakePath, []byte("not a tarball"), 0o600); err != nil {
		t.Fatalf("写伪包：%v", err)
	}

	items, err := adopt.Scan()
	if err != nil {
		t.Fatalf("Scan：%v", err)
	}
	if len(items) != 1 || items[0].Valid {
		t.Fatalf("伪包应被列出且判定为非法，实际 %+v", items)
	}
	if items[0].Reason == "" {
		t.Fatalf("非法包必须给出原因")
	}

	res, err := adopt.Adopt([]string{fakeID, "../escape", ""})
	if err != nil {
		t.Fatalf("Adopt：%v", err)
	}
	if res.Adopted != 0 {
		t.Fatalf("非法输入不应导入任何包，实际 %+v", res)
	}
	if res.Skipped != 3 {
		t.Fatalf("三个非法输入都应计为跳过，实际 %+v", res)
	}
}

// TestAdoptBatchPartialFailure 覆盖批量语义：一个失败不影响其余成功。
func TestAdoptBatchPartialFailure(t *testing.T) {
	t.Parallel()
	f, adopt := newAdoptFixture(t)

	good1 := f.generateRealPackage(t, "好包一")
	good2 := f.generateRealPackage(t, "好包二")
	f.forgetPackage(t, good1)
	f.forgetPackage(t, good2)

	// 中间夹一个伪包。
	if err := os.MkdirAll(f.svc.Dir(), 0o750); err != nil {
		t.Fatalf("建备份目录：%v", err)
	}
	fakeID := "bk-20260101-000000-fake02"
	if err := os.WriteFile(filepath.Join(f.svc.Dir(), fakeID+".tar.gz"), []byte("bad"), 0o600); err != nil {
		t.Fatalf("写伪包：%v", err)
	}

	res, err := adopt.Adopt([]string{good1, fakeID, good2})
	if err != nil {
		t.Fatalf("Adopt：%v", err)
	}
	if res.Adopted != 2 || res.Skipped != 1 {
		t.Fatalf("应为 2 成功 1 跳过，实际 %+v（%s）", res, res.Summary())
	}
	if _, err := f.repo.Get(good1); err != nil {
		t.Fatalf("好包一应已登记：%v", err)
	}
	if _, err := f.repo.Get(good2); err != nil {
		t.Fatalf("好包二应已登记：%v", err)
	}
}

// TestOrphanCleanupRespectsGrace 覆盖宽限期：
// 刚落地（mtime 在宽限期内）的孤儿必须保留，过期孤儿才回收。
func TestOrphanCleanupRespectsGrace(t *testing.T) {
	t.Parallel()
	f, adopt := newAdoptFixture(t)

	fresh := f.generateRealPackage(t, "刚落地")
	old := f.generateRealPackage(t, "已过期")
	f.forgetPackage(t, fresh)
	f.forgetPackage(t, old)

	// 把 old 的 mtime 推到很久以前。
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(f.svc.PackagePath(old), past, past); err != nil {
		t.Fatalf("调整 mtime：%v", err)
	}

	svc := NewBackupOrphanCleanupService(f.svc, adopt, BackupOrphanCleanupOptions{Grace: time.Hour})
	res, err := svc.Run()
	if err != nil {
		t.Fatalf("Run：%v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("应回收 1 个过期孤儿，实际 %+v（%s）", res, res.Summary())
	}
	if res.Skipped != 1 {
		t.Fatalf("应保留 1 个宽限期内的孤儿，实际 %+v（%s）", res, res.Summary())
	}
	if _, err := os.Stat(f.svc.PackagePath(old)); !os.IsNotExist(err) {
		t.Fatalf("过期孤儿的包体应已删除")
	}
	if _, err := os.Stat(f.svc.PackagePath(fresh)); err != nil {
		t.Fatalf("宽限期内的包体必须保留：%v", err)
	}
}

// TestOrphanCleanupKeepsManagedAndInvalid 覆盖两条不删规则：
// 已登记的包绝不回收；无法识别的归档不动（可能是用户放错的其他文件）。
func TestOrphanCleanupKeepsManagedAndInvalid(t *testing.T) {
	t.Parallel()
	f, adopt := newAdoptFixture(t)

	managed := f.generateRealPackage(t, "在管理中")
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(f.svc.PackagePath(managed), past, past); err != nil {
		t.Fatalf("调整 mtime：%v", err)
	}

	if err := os.MkdirAll(f.svc.Dir(), 0o750); err != nil {
		t.Fatalf("建备份目录：%v", err)
	}
	fakeID := "bk-20260101-000000-fake03"
	fakePath := filepath.Join(f.svc.Dir(), fakeID+".tar.gz")
	if err := os.WriteFile(fakePath, []byte("unknown archive"), 0o600); err != nil {
		t.Fatalf("写伪包：%v", err)
	}
	if err := os.Chtimes(fakePath, past, past); err != nil {
		t.Fatalf("调整 mtime：%v", err)
	}

	svc := NewBackupOrphanCleanupService(f.svc, adopt, BackupOrphanCleanupOptions{Grace: time.Hour})
	res, err := svc.Run()
	if err != nil {
		t.Fatalf("Run：%v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("不应删除任何包，实际 %+v（%s）", res, res.Summary())
	}
	if res.Invalid != 1 {
		t.Fatalf("无法识别的归档应计入 Invalid，实际 %+v", res)
	}
	if _, err := os.Stat(f.svc.PackagePath(managed)); err != nil {
		t.Fatalf("已登记的包必须保留：%v", err)
	}
	if _, err := os.Stat(fakePath); err != nil {
		t.Fatalf("无法识别的归档必须保留：%v", err)
	}
}

// TestOrphanCleanupRemovesSidecar 覆盖侧车一并回收，不留 .index 残件。
func TestOrphanCleanupRemovesSidecar(t *testing.T) {
	t.Parallel()
	f, adopt := newAdoptFixture(t)

	orphan := f.generateRealPackage(t, "带侧车")
	f.forgetPackage(t, orphan)
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(f.svc.PackagePath(orphan), past, past); err != nil {
		t.Fatalf("调整 mtime：%v", err)
	}

	svc := NewBackupOrphanCleanupService(f.svc, adopt, BackupOrphanCleanupOptions{Grace: time.Hour})
	res, err := svc.Run()
	if err != nil {
		t.Fatalf("Run：%v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("应回收 1 个，实际 %+v", res)
	}
	if _, err := os.Stat(f.svc.SidecarPath(orphan)); !os.IsNotExist(err) {
		t.Fatalf("侧车索引应随包体一并删除，实际仍存在")
	}
}

// TestOrphanCleanupSkipsSnapshotTemp 覆盖命名空间护栏：
// 生成中的 .tar.gz.snap 临时文件由生成流程自清理，回收作业不得碰它。
func TestOrphanCleanupSkipsSnapshotTemp(t *testing.T) {
	t.Parallel()
	f, adopt := newAdoptFixture(t)

	orphan := f.generateRealPackage(t, "带快照残留")
	f.forgetPackage(t, orphan)
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(f.svc.PackagePath(orphan), past, past); err != nil {
		t.Fatalf("调整 mtime：%v", err)
	}
	snapPath := f.svc.PackagePath(orphan) + ".snap"
	if err := os.WriteFile(snapPath, []byte("snapshot"), 0o600); err != nil {
		t.Fatalf("写快照残留：%v", err)
	}

	svc := NewBackupOrphanCleanupService(f.svc, adopt, BackupOrphanCleanupOptions{Grace: time.Hour})
	if _, err := svc.Run(); err != nil {
		t.Fatalf("Run：%v", err)
	}
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("快照临时文件不应被回收作业删除：%v", err)
	}
}
