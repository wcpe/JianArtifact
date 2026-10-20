package domain

import (
	"os"
	"testing"
)

// TestBackupPreviewReadsPackageContent 覆盖预览的核心承诺：
// 能从包里读出仓库、账号与制品样例——这正是"恢复前先看清里面有什么"的依据。
func TestBackupPreviewReadsPackageContent(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	// 造一个有内容的实例：两个仓库 + 两个账号 + 几条制品。
	f.seedAssets(t, "preview-a", "preview-b", "preview-c")
	pkgID := f.generateRealPackage(t, "待预览")

	pv, err := f.svc.Preview(pkgID)
	if err != nil {
		t.Fatalf("Preview：%v", err)
	}

	// 仓库：至少有一个，且带 format/type。
	if len(pv.Repositories) == 0 {
		t.Fatal("应能读出包内仓库")
	}
	for _, r := range pv.Repositories {
		if r.Name == "" {
			t.Fatal("仓库名不应为空")
		}
	}

	// 账号：应能读出，且**不含**任何口令字段（结构体本身就没有该字段）。
	if len(pv.Users) == 0 {
		t.Fatal("应能读出包内账号")
	}
	for _, u := range pv.Users {
		if u.Username == "" {
			t.Fatal("用户名不应为空")
		}
	}

	// 制品：总数与样例都要有。
	if pv.TotalAssets == 0 {
		t.Fatal("应能读出制品总数")
	}
	if len(pv.SampleAssets) == 0 {
		t.Fatal("应能读出制品样例")
	}
	if pv.BlobBytes <= 0 {
		t.Fatal("应能读出 blob 总字节数")
	}
	// 样例不超过上限（防止大实例把响应撑爆）。
	if len(pv.SampleAssets) > maxPreviewAssets {
		t.Fatalf("样例条数不应超过 %d，实得 %d", maxPreviewAssets, len(pv.SampleAssets))
	}
}

// TestBackupPreviewMissingPackage 覆盖不存在的包：应报"找不到"，而不是 panic 或空结果。
func TestBackupPreviewMissingPackage(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	if _, err := f.svc.Preview("bk-does-not-exist"); err == nil {
		t.Fatal("不存在的包应报错")
	}
}

// TestBackupPreviewIsReadOnly 覆盖只读承诺：
// 预览不得改动包体、不得在备份目录留下任何文件。
func TestBackupPreviewIsReadOnly(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	f.seedAssets(t, "ro-payload")
	pkgID := f.generateRealPackage(t, "只读校验")

	pkgPath := f.svc.PackagePath(pkgID)
	before, err := os.Stat(pkgPath)
	if err != nil {
		t.Fatalf("读取包体信息：%v", err)
	}
	entriesBefore, _ := os.ReadDir(f.svc.Dir())

	if _, err := f.svc.Preview(pkgID); err != nil {
		t.Fatalf("Preview：%v", err)
	}

	after, err := os.Stat(pkgPath)
	if err != nil {
		t.Fatalf("预览后包体应仍存在：%v", err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("预览不应改动包体")
	}
	entriesAfter, _ := os.ReadDir(f.svc.Dir())
	if len(entriesBefore) != len(entriesAfter) {
		t.Fatalf("预览不应在备份目录留下文件：前 %d 项、后 %d 项",
			len(entriesBefore), len(entriesAfter))
	}
}
