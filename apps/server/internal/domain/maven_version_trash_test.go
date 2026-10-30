package domain_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newMavenTrashFixture 构造一个带资产回收站的 Maven hosted 仓库。
func newMavenTrashFixture(t *testing.T, repoName string) (*domain.AssetService, *repository.AssetTrashRepo, *blobstore.Store) {
	t.Helper()
	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	svc := domain.NewAssetService(repos, assets, blobs, nil)
	svc.SetChangeRecorder(domain.NoopChangeRecorder{})
	svc.SetNodeIdentity(domain.NewNodeIdentity(repository.NewSettingRepo(db)))
	trash := repository.NewAssetTrashRepo(db)
	svc.SetAssetTrashRepo(trash)
	if _, err := repos.Create(repoName, "maven", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	return svc, trash, blobs
}

// publishMavenVersionFiles 发布一组位于同一版本目录下的文件，返回展示名列表。
func publishMavenVersionFiles(t *testing.T, svc *domain.AssetService, repoName, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if _, err := svc.Put(repoName, dir+"/"+name, bytes.NewBufferString(body), "application/octet-stream"); err != nil {
			t.Fatalf("发布 %s：%v", name, err)
		}
	}
}

// TestMavenVersionPlanGroupsAllNamingAnchors 验证「不同命名锚点一次性找出来」：
// 版本目录下的 jar/pom/sources/javadoc/校验和/metadata 全部出现在同一份计划里，
// 并各自归入正确的类别。
func TestMavenVersionPlanGroupsAllNamingAnchors(t *testing.T) {
	t.Parallel()
	svc, _, _ := newMavenTrashFixture(t, "maven-plan")
	dir := "com/example/demo/1.17-SNAPSHOT"
	publishMavenVersionFiles(t, svc, "maven-plan", dir, map[string]string{
		"demo-1.17-20260317.215705-3.jar":         "jar",
		"demo-1.17-20260317.215705-3.pom":         "pom",
		"demo-1.17-20260317.215705-3-sources.jar": "sources",
		"demo-1.17-20260317.215705-3-javadoc.jar": "javadoc",
		"demo-1.17-20260317.215705-3.jar.sha1":    "sha1",
		"demo-1.17-20260317.215705-3.jar.md5":     "md5",
		"maven-metadata.xml":                      "<metadata/>",
		"maven-metadata.xml.sha1":                 "meta-sha1",
	})

	plan, err := svc.PlanMavenVersion("maven-plan", domain.MavenVersionCoordinate{
		GroupID: "com.example", ArtifactID: "demo", Version: "1.17-SNAPSHOT",
	})
	if err != nil {
		t.Fatalf("生成清理计划：%v", err)
	}
	// 8 个文件一个都不能漏：漏掉任何一个都会让 metadata 或校验和指向不存在的内容。
	if plan.TotalFiles != 8 {
		t.Fatalf("应展开 8 个文件，得 %d（%+v）", plan.TotalFiles, plan.Files)
	}
	if !plan.Exists {
		t.Fatal("Exists 应为 true")
	}

	// SNAPSHOT 版本必须给出历史构建聚合。
	if len(plan.SnapshotBuilds) != 1 {
		t.Fatalf("应有 1 个时间戳构建，得 %+v", plan.SnapshotBuilds)
	}
	if plan.SnapshotBuilds[0].BuildID != "20260317.215705-3" {
		t.Fatalf("构建号解析错误：%q", plan.SnapshotBuilds[0].BuildID)
	}
	if plan.SnapshotBuilds[0].FileCount != 6 {
		t.Fatalf("该构建应含 6 个文件（jar/pom/sources/javadoc/sha1/md5），得 %d",
			plan.SnapshotBuilds[0].FileCount)
	}

	// metadata 与其校验和必须归入 metadata 类，而不是被当成主档。
	metadataCount := 0
	for _, file := range plan.Files {
		if file.Category == domain.MavenFileMetadata {
			metadataCount++
		}
	}
	if metadataCount != 2 {
		t.Fatalf("metadata 类应含 2 个文件（xml + 其 sha1），得 %d", metadataCount)
	}
}

// TestMavenVersionDeleteKeepsBlobAndRestoresExactly 是本功能的核心断言：
// 删除后 blob 仍在、资产不可见、回收站有条目；恢复后字段与内容与删除前完全一致。
func TestMavenVersionDeleteKeepsBlobAndRestoresExactly(t *testing.T) {
	t.Parallel()
	svc, trash, blobs := newMavenTrashFixture(t, "maven-restore")
	dir := "com/example/demo/2.0-SNAPSHOT"
	const jarPath = "com/example/demo/2.0-SNAPSHOT/demo-2.0-20260401.120000-1.jar"
	jarBody := "the-jar-content"
	if _, err := svc.Put("maven-restore", jarPath, bytes.NewBufferString(jarBody), "application/java-archive"); err != nil {
		t.Fatalf("发布 jar：%v", err)
	}
	before, beforeRC, err := svc.Get("maven-restore", jarPath)
	if err != nil {
		t.Fatalf("读取待删资产：%v", err)
	}
	// 必须显式关闭读流：blob 是磁盘文件句柄，Windows 上未关闭会让后续删除/隔离失败。
	_ = beforeRC.Close()
	if !blobs.Exists(before.BlobHash) {
		t.Fatal("前置条件：blob 应存在")
	}
	_ = dir

	result, err := svc.DeleteMavenVersion("maven-restore", domain.MavenVersionCoordinate{
		GroupID: "com.example", ArtifactID: "demo", Version: "2.0-SNAPSHOT",
	}, nil, "tester")
	if err != nil {
		t.Fatalf("版本级软删除：%v", err)
	}
	if result.Trashed != 1 {
		t.Fatalf("应删除 1 个文件，得 %d", result.Trashed)
	}

	// 1) 资产已不可见。
	if _, leakRC, getErr := svc.Get("maven-restore", jarPath); !errors.Is(getErr, domain.ErrNotFound) {
		if leakRC != nil {
			_ = leakRC.Close()
		}
		t.Fatalf("删除后应读不到该资产，得 %v", getErr)
	}
	// 2) blob 必须保留——这是可恢复的唯一依据。
	if !blobs.Exists(before.BlobHash) {
		t.Fatal("软删除绝不能释放 blob，否则无法恢复")
	}
	// 3) 回收站里有条目。
	entries, total, err := trash.List(repository.AssetTrashStaged, 10, 0)
	if err != nil {
		t.Fatalf("读取回收站：%v", err)
	}
	if total != 1 || len(entries) != 1 {
		t.Fatalf("回收站应有 1 条，得 total=%d len=%d", total, len(entries))
	}
	if entries[0].Path != jarPath {
		t.Fatalf("回收站路径不符：%q", entries[0].Path)
	}
	// 校验和必须原样保存，否则恢复出来的制品校验和不一致。
	if entries[0].Sha1 != before.Sha1 || entries[0].Md5 != before.Md5 {
		t.Fatalf("回收站应保存完整校验和：got sha1=%q md5=%q want sha1=%q md5=%q",
			entries[0].Sha1, entries[0].Md5, before.Sha1, before.Md5)
	}

	// 4) 恢复后字段与内容与删除前完全一致。
	restoreResult, err := svc.RestoreAssetTrash([]int64{entries[0].ID})
	if err != nil {
		t.Fatalf("恢复：%v", err)
	}
	if restoreResult.Restored != 1 || len(restoreResult.Conflicts) != 0 {
		t.Fatalf("恢复结果异常：%+v", restoreResult)
	}
	restored, rc, err := svc.Get("maven-restore", jarPath)
	if err != nil {
		t.Fatalf("恢复后应能读到：%v", err)
	}
	defer func() { _ = rc.Close() }()
	if restored.BlobHash != before.BlobHash || restored.Size != before.Size {
		t.Fatalf("恢复后内容不符：got hash=%q size=%d want hash=%q size=%d",
			restored.BlobHash, restored.Size, before.BlobHash, before.Size)
	}
	if restored.Sha1 != before.Sha1 || restored.ContentType != before.ContentType {
		t.Fatalf("恢复后元数据不符：got sha1=%q type=%q", restored.Sha1, restored.ContentType)
	}
	// 时间戳必须回到原值：恢复不是"重新上传"。
	if restored.CreatedAt != before.CreatedAt || restored.UpdatedAt != before.UpdatedAt {
		t.Fatalf("恢复后时间戳应还原：got created=%q updated=%q want created=%q updated=%q",
			restored.CreatedAt, restored.UpdatedAt, before.CreatedAt, before.UpdatedAt)
	}
	if entry, err := trash.Get(entries[0].ID); err != nil {
		t.Fatalf("读取已恢复条目：%v", err)
	} else if entry.Status != repository.AssetTrashRestored {
		t.Fatalf("条目状态应为 restored，得 %q", entry.Status)
	}
}

// TestRestoreSkipsWhenTargetPathOccupied 验证恢复绝不覆盖：
// 目标路径被新内容占用时应保护性跳过并如实报告冲突。
func TestRestoreSkipsWhenTargetPathOccupied(t *testing.T) {
	t.Parallel()
	svc, trash, _ := newMavenTrashFixture(t, "maven-conflict")
	const path = "com/example/dup/1.0/dup-1.0.jar"
	if _, err := svc.Put("maven-conflict", path, bytes.NewBufferString("old"), "application/java-archive"); err != nil {
		t.Fatalf("发布：%v", err)
	}
	if _, err := svc.DeleteMavenVersion("maven-conflict", domain.MavenVersionCoordinate{
		GroupID: "com.example", ArtifactID: "dup", Version: "1.0",
	}, nil, "tester"); err != nil {
		t.Fatalf("删除：%v", err)
	}
	// 同路径重新发布一份不同内容。
	if _, err := svc.Put("maven-conflict", path, bytes.NewBufferString("new"), "application/java-archive"); err != nil {
		t.Fatalf("重新发布：%v", err)
	}
	entries, _, err := trash.List(repository.AssetTrashStaged, 10, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("回收站应有 1 条：%v len=%d", err, len(entries))
	}

	result, err := svc.RestoreAssetTrash([]int64{entries[0].ID})
	if err != nil {
		t.Fatalf("恢复：%v", err)
	}
	if result.Restored != 0 {
		t.Fatalf("冲突时不应恢复任何条目，得 %d", result.Restored)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("应报告 1 条冲突，得 %+v", result.Conflicts)
	}
	// 现有内容必须保持不变。
	_, rc, err := svc.Get("maven-conflict", path)
	if err != nil {
		t.Fatalf("原有资产应仍在：%v", err)
	}
	_ = rc.Close()
}

// TestPurgeFreesBlobOnlyAfterLeavingTrash 验证彻底清理才释放 blob。
func TestPurgeFreesBlobOnlyAfterLeavingTrash(t *testing.T) {
	t.Parallel()
	svc, trash, blobs := newMavenTrashFixture(t, "maven-purge")
	const path = "com/example/purge/1.0/purge-1.0.jar"
	if _, err := svc.Put("maven-purge", path, bytes.NewBufferString("bye"), "application/java-archive"); err != nil {
		t.Fatalf("发布：%v", err)
	}
	before, beforeRC, err := svc.Get("maven-purge", path)
	if err != nil {
		t.Fatalf("读取：%v", err)
	}
	// 必须显式关闭读流：blob 是磁盘文件句柄，Windows 上未关闭会让后续删除失败。
	_ = beforeRC.Close()
	if _, err := svc.DeleteMavenVersion("maven-purge", domain.MavenVersionCoordinate{
		GroupID: "com.example", ArtifactID: "purge", Version: "1.0",
	}, nil, "tester"); err != nil {
		t.Fatalf("删除：%v", err)
	}
	if !blobs.Exists(before.BlobHash) {
		t.Fatal("入回收站后 blob 仍应保留")
	}
	entries, _, err := trash.List(repository.AssetTrashStaged, 10, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("回收站应有 1 条：%v", err)
	}
	result, err := svc.PurgeAssetTrash([]int64{entries[0].ID})
	if err != nil {
		t.Fatalf("彻底清理：%v", err)
	}
	if result.Purged != 1 {
		t.Fatalf("应清理 1 条，得 %d", result.Purged)
	}
	if blobs.Exists(before.BlobHash) {
		t.Fatal("彻底清理后 blob 应被释放")
	}
}

// TestPurgeKeepsBlobStillReferencedElsewhere 验证共享 blob 不被误删：
// 同一内容同时存在于另一个仍在用的路径时，清理回收站不能毁掉那份数据。
func TestPurgeKeepsBlobStillReferencedElsewhere(t *testing.T) {
	t.Parallel()
	svc, trash, blobs := newMavenTrashFixture(t, "maven-shared")
	const shared = "identical-content"
	const trashedPath = "com/example/shared/1.0/shared-1.0.jar"
	const livePath = "com/example/shared/2.0/shared-2.0.jar"
	if _, err := svc.Put("maven-shared", trashedPath, bytes.NewBufferString(shared), "application/java-archive"); err != nil {
		t.Fatalf("发布待删：%v", err)
	}
	if _, err := svc.Put("maven-shared", livePath, bytes.NewBufferString(shared), "application/java-archive"); err != nil {
		t.Fatalf("发布在用：%v", err)
	}
	trashedAsset, trashedRC, err := svc.Get("maven-shared", trashedPath)
	if err != nil {
		t.Fatalf("读取待删：%v", err)
	}
	_ = trashedRC.Close()
	liveAsset, liveRC, err := svc.Get("maven-shared", livePath)
	if err != nil {
		t.Fatalf("读取在用：%v", err)
	}
	_ = liveRC.Close()
	if trashedAsset.BlobHash != liveAsset.BlobHash {
		t.Fatal("前置条件：两份内容应共享同一 blob")
	}
	if _, err := svc.DeleteMavenVersion("maven-shared", domain.MavenVersionCoordinate{
		GroupID: "com.example", ArtifactID: "shared", Version: "1.0",
	}, nil, "tester"); err != nil {
		t.Fatalf("删除：%v", err)
	}
	entries, _, err := trash.List(repository.AssetTrashStaged, 10, 0)
	if err != nil || len(entries) == 0 {
		t.Fatalf("回收站应有条目：%v", err)
	}
	if _, err := svc.PurgeAssetTrash([]int64{entries[0].ID}); err != nil {
		t.Fatalf("彻底清理：%v", err)
	}
	// 共享 blob 仍被 livePath 引用，绝不能删。
	if !blobs.Exists(trashedAsset.BlobHash) {
		t.Fatal("共享 blob 仍被在用资产引用，不得释放")
	}
	if _, liveCheck, err := svc.Get("maven-shared", livePath); err != nil {
		t.Fatalf("在用资产应仍可读：%v", err)
	} else {
		_ = liveCheck.Close()
	}
}

// TestDeleteMavenVersionRejectsPathOutsidePlan 验证子集校验：
// 前端若回传计划外的路径，整批拒绝而不是悄悄删掉别的东西。
func TestDeleteMavenVersionRejectsPathOutsidePlan(t *testing.T) {
	t.Parallel()
	svc, _, _ := newMavenTrashFixture(t, "maven-subset")
	if _, err := svc.Put("maven-subset", "com/example/sub/1.0/sub-1.0.jar", bytes.NewBufferString("x"), "application/java-archive"); err != nil {
		t.Fatalf("发布：%v", err)
	}
	_, err := svc.DeleteMavenVersion("maven-subset", domain.MavenVersionCoordinate{
		GroupID: "com.example", ArtifactID: "sub", Version: "1.0",
	}, []string{"com/example/sub/1.0/NOT-IN-PLAN.jar"}, "tester")
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("计划外路径应被拒绝，得 %v", err)
	}
	// 拒绝后原资产必须完好。
	if _, guardRC, getErr := svc.Get("maven-subset", "com/example/sub/1.0/sub-1.0.jar"); getErr != nil {
		t.Fatalf("被拒绝的删除不应影响资产：%v", getErr)
	} else if guardRC != nil {
		_ = guardRC.Close()
	}
}

// TestPlanRejectsCoordinateEscape 验证坐标校验：含 `..` 或路径分隔符的输入必须被拒绝，
// 否则 LIKE 前缀匹配会逃逸到别的目录。
func TestPlanRejectsCoordinateEscape(t *testing.T) {
	t.Parallel()
	svc, _, _ := newMavenTrashFixture(t, "maven-escape")
	cases := []domain.MavenVersionCoordinate{
		{GroupID: "com..example", ArtifactID: "demo", Version: "1.0"},
		{GroupID: "com/example", ArtifactID: "demo", Version: "1.0"},
		{GroupID: "com.example", ArtifactID: "../etc", Version: "1.0"},
		{GroupID: "com.example", ArtifactID: "demo", Version: ""},
	}
	for _, coord := range cases {
		if _, err := svc.PlanMavenVersion("maven-escape", coord); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("坐标 %+v 应被拒绝，得 %v", coord, err)
		}
	}
}

// TestPlanReportsNotExistForUnknownVersion 验证空版本返回 Exists=false 而不是报错。
func TestPlanReportsNotExistForUnknownVersion(t *testing.T) {
	t.Parallel()
	svc, _, _ := newMavenTrashFixture(t, "maven-empty")
	plan, err := svc.PlanMavenVersion("maven-empty", domain.MavenVersionCoordinate{
		GroupID: "com.example", ArtifactID: "nothing", Version: "9.9",
	})
	if err != nil {
		t.Fatalf("空版本不应报错：%v", err)
	}
	if plan.Exists || plan.TotalFiles != 0 {
		t.Fatalf("应为空计划，得 exists=%v files=%d", plan.Exists, plan.TotalFiles)
	}
}
