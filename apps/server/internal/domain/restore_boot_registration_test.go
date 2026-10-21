package domain

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// TestApplyPendingRestoreKeepsBackupRegistrations 是「恢复把备份包一起覆盖掉」的回归守卫。
//
// # 曾经的 bug
//
// backup_package 登记表与被恢复的数据库是同一个文件，而备份包体（<id>.tar.gz）在数据目录下、
// **不在**快照里。恢复一份旧快照会让登记表退回旧内容，于是恢复之后"磁盘上明明有包、
// 列表里却看不见"——用户看到的就是"备份包全丢了"。而 restore 恰恰是最需要备份兜底的时刻。
//
// 实测证据：测试站恢复后 API 登记只剩 3 个，磁盘上却有 42 个包体。
//
// # 本用例守住什么
//
// 恢复完成后，凡"包体存在且 manifest 合法"的包都必须出现在登记表里，
// 且 outcome.ReRegistered 如实报告补回的条数。
func TestApplyPendingRestoreKeepsBackupRegistrations(t *testing.T) {
	t.Parallel()

	source := newRestoreTargetFixture(t)
	source.seedAssetsForRestore(t, "payload-a", "payload-b")
	pkgPath := source.generateRestorePackage(t)

	target := newRestoreTargetFixture(t)
	// 目标实例先放几个"本地已有的备份包"，模拟"恢复前攒下的备份"。
	// 它们只存在于目标磁盘上，不在源快照里——正是会被覆盖掉的那批。
	preExisting := target.seedLocalBackupPackages(t, 3)

	// 导入并应用到目标。
	if _, err := target.svc.Stage(t.Context(), RestoreRequest{
		SourcePath: pkgPath,
		Origin:     "upload",
		Operator:   "tester",
		Overwrite:  true,
	}); err != nil {
		t.Fatalf("Stage：%v", err)
	}
	_ = target.db.Close()
	applied, outcome, err := ApplyPendingRestore(target.dir, target.dbPath, filepath.Join(target.dir, "blobs"))
	if err != nil {
		t.Fatalf("ApplyPendingRestore：%v", err)
	}
	if !applied {
		t.Fatal("应已应用恢复")
	}

	// 核心断言：恢复前就在磁盘上的那些包，恢复后必须仍在列表里。
	reopened, err := persistence.Open(target.dbPath)
	if err != nil {
		t.Fatalf("重开恢复后的数据库：%v", err)
	}
	defer func() { _ = reopened.Close() }()

	for _, id := range preExisting {
		var count int
		if err := reopened.Get(&count, `SELECT COUNT(*) FROM backup_package WHERE package_id=?`, id); err != nil {
			t.Fatalf("查询登记 %s：%v", id, err)
		}
		if count != 1 {
			t.Fatalf("恢复后备份登记 %s 应仍存在，实际 count=%d（这正是「备份包全丢了」的 bug）", id, count)
		}
	}
	if outcome.ReRegistered != len(preExisting) {
		t.Fatalf("ReRegistered = %d，期望 %d", outcome.ReRegistered, len(preExisting))
	}
	// 摘要里要能看出补回了多少（运维靠它判断是否发生了覆盖）。
	if !contains(outcome.Summary(), "补回备份登记") {
		t.Fatalf("恢复摘要应体现补回数量：%s", outcome.Summary())
	}
}

// TestApplyPendingRestoreIdempotentOnReRegister 覆盖幂等：
// 源快照里本来就登记过的包不应被重复插入或改动。
func TestApplyPendingRestoreIdempotentOnReRegister(t *testing.T) {
	t.Parallel()

	source := newRestoreTargetFixture(t)
	source.seedAssetsForRestore(t, "payload-x")
	pkgPath := source.generateRestorePackage(t)

	target := newRestoreTargetFixture(t)
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
	// 没有本地包时不该补任何登记。
	if outcome.ReRegistered != 0 {
		t.Fatalf("无本地包时 ReRegistered 应为 0，实得 %d", outcome.ReRegistered)
	}
}

// TestApplyPendingRestoreSkipsBrokenPackages 覆盖身份校验：
// 磁盘上无法识别的归档不应被补成登记（它本来就没法用）。
func TestApplyPendingRestoreSkipsBrokenPackages(t *testing.T) {
	t.Parallel()

	source := newRestoreTargetFixture(t)
	source.seedAssetsForRestore(t, "payload-y")
	pkgPath := source.generateRestorePackage(t)

	target := newRestoreTargetFixture(t)
	target.seedLocalBackupPackages(t, 1)
	// 再放一个损坏的归档。
	backupDir := filepath.Join(target.dir, "backups")
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatalf("建备份目录：%v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "bk-20260101-000000-broken.tar.gz"),
		[]byte("not a tarball"), 0o600); err != nil {
		t.Fatalf("写损坏包：%v", err)
	}

	if _, err := target.svc.Stage(t.Context(), RestoreRequest{
		SourcePath: pkgPath, Origin: "upload", Operator: "tester", Overwrite: true,
	}); err != nil {
		t.Fatalf("Stage：%v", err)
	}
	_ = target.db.Close()
	_, outcome, err := ApplyPendingRestore(target.dir, target.dbPath, filepath.Join(target.dir, "blobs"))
	if err != nil {
		t.Fatalf("ApplyPendingRestore：%v", err)
	}
	if outcome.ReRegistered != 1 {
		t.Fatalf("只应补回 1 个合法包，实得 %d", outcome.ReRegistered)
	}
}

// —— 测试助手 ——

// seedAssetsForRestore 在夹具里写入若干资产并落盘 blob。
func (f *restoreTarget) seedAssetsForRestore(t *testing.T, contents ...string) {
	t.Helper()
	res, err := f.db.Exec(`INSERT INTO repository (name, format, type) VALUES ('restore-repo', 'raw','hosted')`)
	if err != nil {
		t.Fatalf("插入仓库：%v", err)
	}
	repoID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取仓库 id：%v", err)
	}
	for i, body := range contents {
		hash, _, _, _, err := f.store.Put(strings.NewReader(body))
		if err != nil {
			t.Fatalf("写入 blob：%v", err)
		}
		if _, err := f.db.Exec(`INSERT INTO asset
			(repository_id, path, blob_hash, size, content_type)
			VALUES (?, ?, ?, ?, 'text/plain')`,
			repoID, "pkg/item-"+string(rune('a'+i)), hash, int64(len(body))); err != nil {
			t.Fatalf("插入资产：%v", err)
		}
	}
}

// generateRestorePackage 基于当前夹具生成一份可恢复的备份包，返回归档路径。
func (f *restoreTarget) generateRestorePackage(t *testing.T) string {
	t.Helper()
	snap := filepath.Join(f.dir, "gen.snap")
	if err := persistence.SnapshotFile(f.dbPath, snap); err != nil {
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
		PackageID:       "bk-source-" + filepath.Base(f.dir),
		Mode:            archive.ModeHot,
		CreatedAt:       time.Now(),
		AppVersion:      "0.11.0",
		DBSchemaVersion: currentSchemaVersion(t, f.db),
	}
	out := filepath.Join(f.dir, "source.tar.gz")
	if _, err := archive.WriteBundle(out, m, snap, archive.NewBlobIndex(refs), f.store, nil); err != nil {
		t.Fatalf("WriteBundle：%v", err)
	}
	return out
}

// seedLocalBackupPackages 在夹具的 backups/ 下造 n 个**只存在于磁盘**的合法备份包，
// 返回它们的标识。它们不写登记行——模拟"恢复后会消失在列表里"的那批。
func (f *restoreTarget) seedLocalBackupPackages(t *testing.T, n int) []string {
	t.Helper()
	backupDir := filepath.Join(f.dir, "backups")
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatalf("建备份目录：%v", err)
	}
	snap := filepath.Join(f.dir, "local.snap")
	if err := persistence.SnapshotFile(f.dbPath, snap); err != nil {
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
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := "bk-local-" + string(rune('A'+i))
		m := archive.Manifest{
			SchemaVersion:   archive.SchemaVersion,
			Kind:            archive.Kind,
			PackageID:       id,
			Mode:            archive.ModeHot,
			CreatedAt:       time.Now().Add(-time.Duration(i) * time.Hour),
			AppVersion:      "0.11.0",
			DBSchemaVersion: currentSchemaVersion(t, f.db),
		}
		if _, err := archive.WriteBundle(filepath.Join(backupDir, id+".tar.gz"), m, snap,
			archive.NewBlobIndex(refs), f.store, nil); err != nil {
			t.Fatalf("WriteBundle(%s)：%v", id, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// currentSchemaVersion 读取夹具数据库的当前 schema 版本（写进 manifest 供兼容判定）。
func currentSchemaVersion(t *testing.T, db *persistence.DB) int {
	t.Helper()
	raw, err := db.CurrentVersion()
	if err != nil {
		t.Fatalf("读 schema 版本：%v", err)
	}
	n, convErr := strconv.Atoi(raw)
	if convErr != nil {
		t.Fatalf("schema 版本不是数字：%q", raw)
	}
	return n
}

// contains 是字符串包含的薄封装（避免为一条断言引入额外依赖）。
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
