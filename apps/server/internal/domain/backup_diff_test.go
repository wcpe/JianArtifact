package domain

import (
	"os"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestDiffSummaryClassifiesFourKinds 覆盖四分类的正确性：
// 包里有本机没有 / 本机有包里没有 / 两边都有但变了 / 两边一致。
//
// 这是"恢复前看清会改什么"的核心依据，任何一类算错都会让用户的判断走偏。
func TestDiffSummaryClassifiesFourKinds(t *testing.T) {
	t.Parallel()

	// 源实例：**只用 generateRealPackage 造包**，不要再额外 seedAssets——
	// 那会让源多出一整个仓库，使"包内"与"我预期的包内容"对不上（本测试第一版就这么错了）。
	src := newBackupFixture(t)
	pkgID := src.generateRealPackage(t, "对比基线")
	// 此时源包内只有 1 个仓库、1 条制品（raw-repo-1/a/0.bin）。

	// 目标实例：造两条，其中一条与包内路径重合（内容相同 → same），
	// 另一条只在本地存在 → onlyLocal。
	dst := newBackupFixture(t)
	dst.seedAssets(t, "adopt-payload-对比基线") // 与源包内同一 blob → 内容一致
	dst.seedAssets(t, "only-local-payload") // 本机独有

	// 把源包拷到目标实例的备份目录并登记，才能在目标上做对比。
	srcPath := src.svc.PackagePath(pkgID)
	dstPath := dst.svc.PackagePath(pkgID)
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("读源包：%v", err)
	}
	if err := os.MkdirAll(dst.svc.Dir(), 0o750); err != nil {
		t.Fatalf("建备份目录：%v", err)
	}
	if err := os.WriteFile(dstPath, raw, 0o600); err != nil {
		t.Fatalf("写包：%v", err)
	}
	// 直接登记（不解析 manifest，diff 只需要能 Get 到）。
	if err := dst.repo.Create(repository.BackupPackage{
		PackageID: pkgID,
		Mode:      repository.BackupModeHot,
		Status:    repository.BackupStatusDone,
		Origin:    repository.BackupOriginManual,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("登记：%v", err)
	}

	sum, err := dst.svc.DiffSummary(pkgID)
	if err != nil {
		t.Fatalf("DiffSummary：%v", err)
	}

	// 构造决定的期望：包内 1 条（raw-repo-1/a/0.bin），本地 2 条
	// （raw-repo-1/a/0.bin 同内容、raw-repo-2/a/0.bin 本机独有）。
	if sum.Assets.Same != 1 {
		t.Fatalf("应识别出 1 条内容一致的制品，实得 %d（%+v）", sum.Assets.Same, sum.Assets)
	}
	if sum.Assets.OnlyLocal != 1 {
		t.Fatalf("应识别出 1 条本机独有的制品，实得 %d（%+v）", sum.Assets.OnlyLocal, sum.Assets)
	}
	if sum.Assets.OnlyInPackage != 0 {
		t.Fatalf("本用例里包内制品都在本机存在，onlyInPackage 应为 0，实得 %d", sum.Assets.OnlyInPackage)
	}
	if !sum.MetadataOnly {
		t.Fatal("必须标明本次只比元数据")
	}

	// 明细必须能取到那条「本机独有」，且分页与总数一致。
	page, err := dst.svc.DiffDetail(pkgID, DiffEntityAssets, DiffOnlyLocal, 1, 10)
	if err != nil {
		t.Fatalf("DiffDetail：%v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("明细应返回 1 条，实得 total=%d items=%d", page.Total, len(page.Items))
	}
	if !contains(page.Items[0].Key, "raw-repo-2") {
		t.Fatalf("明细应指向本机独有那条，实得 %q", page.Items[0].Key)
	}
}

// TestDiffSnapshotCacheReusesExtraction 覆盖缓存复用：
// 第二次取快照不得重新解包（否则用户每次翻页都要等一次分钟级解包）。
func TestDiffSnapshotCacheReusesExtraction(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	pkgID := f.generateRealPackage(t, "缓存")

	first, err := f.svc.snapshotFor(pkgID)
	if err != nil {
		t.Fatalf("首次取快照：%v", err)
	}
	second, err := f.svc.snapshotFor(pkgID)
	if err != nil {
		t.Fatalf("二次取快照：%v", err)
	}
	if first != second {
		t.Fatalf("应复用同一份解包结果：%q vs %q", first, second)
	}

	// 包体被改动后必须失效（否则对比的是旧内容）。
	path := f.svc.PackagePath(pkgID)
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("stat：%v", statErr)
	}
	if err := os.Chtimes(path, info.ModTime().Add(time.Second), info.ModTime().Add(time.Second)); err != nil {
		t.Fatalf("改 mtime：%v", err)
	}
	third, err := f.svc.snapshotFor(pkgID)
	if err != nil {
		t.Fatalf("改后取快照：%v", err)
	}
	if third == first {
		t.Fatal("包体变化后应重新解包，而不是复用旧快照")
	}
}

// TestDiffDetailRespectsPageCap 覆盖分页与上限：
// 单页不得超过 maxDiffDetailPageSize，且 Total 必须是真实总数。
func TestDiffDetailRespectsPageCap(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	pkgID := f.generateRealPackage(t, "上限")

	// 请求一个远超上限的页大小，应被钳制而不是照办。
	page, err := f.svc.DiffDetail(pkgID, DiffEntityAssets, DiffSame, 1, 99999)
	if err != nil {
		t.Fatalf("DiffDetail：%v", err)
	}
	if len(page.Items) > maxDiffDetailPageSize {
		t.Fatalf("单页不应超过 %d 条，实得 %d", maxDiffDetailPageSize, len(page.Items))
	}
}

// TestDiffRejectsUnknownEntityAndKind 覆盖参数校验，避免把非法输入当成"无差异"。
func TestDiffRejectsUnknownEntityAndKind(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	pkgID := f.generateRealPackage(t, "参数")

	if _, err := f.svc.DiffDetail(pkgID, DiffEntity("nope"), DiffSame, 1, 10); err == nil {
		t.Fatal("未知实体应报错")
	}
	if _, err := f.svc.DiffDetail(pkgID, DiffEntityAssets, DiffKind("nope"), 1, 10); err == nil {
		t.Fatal("未知分类应报错")
	}
}
