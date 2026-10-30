package domain_test

import (
	"bytes"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// TestSearchMavenPackageVersionsByPackageName 验证核心诉求：
// 输入包名（group:artifact）能一次列出该包下所有版本，而不是逐目录点进去找。
func TestSearchMavenPackageVersionsByPackageName(t *testing.T) {
	t.Parallel()
	svc, _, _ := newMavenTrashFixture(t, "pkg-search")
	publish := func(dir, file string) {
		if _, err := svc.Put("pkg-search", dir+"/"+file, bytes.NewBufferString("x"), "application/java-archive"); err != nil {
			t.Fatalf("发布 %s：%v", dir, err)
		}
	}
	publish("com/tbao/tbaoolib/1.0.0", "tbaoolib-1.0.0.jar")
	publish("com/tbao/tbaoolib/2.0.0", "tbaoolib-2.0.0.jar")
	publish("com/tbao/tbaoolib/2.0.0", "tbaoolib-2.0.0.pom")
	// 同 group 的另一个包，不能被带进来。
	publish("com/tbao/otherlib/1.0.0", "otherlib-1.0.0.jar")

	result, err := svc.SearchMavenPackageVersions("pkg-search", domain.MavenPackageQuery{
		GroupID: "com.tbao", ArtifactID: "tbaoolib",
	})
	if err != nil {
		t.Fatalf("搜索：%v", err)
	}
	if len(result.Versions) != 2 {
		t.Fatalf("应命中 2 个版本，得 %d（%+v）", len(result.Versions), result.Versions)
	}
	// 关键断言：别的包绝不能混进来。
	for _, v := range result.Versions {
		if v.Coordinate.ArtifactID != "tbaoolib" {
			t.Fatalf("不应命中其它包：%+v", v.Coordinate)
		}
		if v.Coordinate.GroupID != "com.tbao" {
			t.Fatalf("groupId 解析错误：%q", v.Coordinate.GroupID)
		}
	}
	// 2.0.0 有两个文件，字节应累计。
	for _, v := range result.Versions {
		if v.Coordinate.Version == "2.0.0" && v.FileCount != 2 {
			t.Fatalf("2.0.0 应有 2 个文件，得 %d", v.FileCount)
		}
	}
}

// TestSearchMavenPackageWildcards 验证通配符：`*` 只匹配坐标，不会退化成"扫全仓删除"。
func TestSearchMavenPackageWildcards(t *testing.T) {
	t.Parallel()
	svc, _, _ := newMavenTrashFixture(t, "pkg-wildcard")
	// 目录 → artifactId 一一列出，避免从路径反推时把版本号误当 artifact。
	fixtures := []struct{ dir, artifact string }{
		{"com/tbao/liba/1.0.0", "liba"},
		{"com/tbao/libb/1.0.0", "libb"},
		{"com/tbao/libb/2.0.0", "libb"},
		{"org/other/libc/1.0.0", "libc"},
	}
	for _, f := range fixtures {
		path := f.dir + "/" + f.artifact + ".jar"
		if _, err := svc.Put("pkg-wildcard", path, bytes.NewBufferString("x"), "application/java-archive"); err != nil {
			t.Fatalf("发布 %s：%v", path, err)
		}
	}

	// artifactId 通配
	res, err := svc.SearchMavenPackageVersions("pkg-wildcard", domain.MavenPackageQuery{
		GroupID: "com.tbao", ArtifactID: "lib*",
	})
	if err != nil {
		t.Fatalf("搜索：%v", err)
	}
	if len(res.Versions) != 3 {
		t.Fatalf("lib* 应命中 3 个版本（liba 1 + libb 2），得 %d", len(res.Versions))
	}

	// groupId 通配
	res, err = svc.SearchMavenPackageVersions("pkg-wildcard", domain.MavenPackageQuery{
		GroupID: "com.*", ArtifactID: "*",
	})
	if err != nil {
		t.Fatalf("搜索：%v", err)
	}
	if len(res.Versions) != 3 {
		t.Fatalf("com.* 应命中 3 个版本，得 %d", len(res.Versions))
	}
	// org.other 的包不能被 com.* 带上。
	for _, v := range res.Versions {
		if v.Coordinate.ArtifactID == "libc" {
			t.Fatal("com.* 不应命中 org.other:libc")
		}
	}

	// version 通配
	res, err = svc.SearchMavenPackageVersions("pkg-wildcard", domain.MavenPackageQuery{
		GroupID: "com.tbao", ArtifactID: "libb", Version: "2.*",
	})
	if err != nil {
		t.Fatalf("搜索：%v", err)
	}
	if len(res.Versions) != 1 || res.Versions[0].Coordinate.Version != "2.0.0" {
		t.Fatalf("2.* 应只命中 2.0.0，得 %+v", res.Versions)
	}
}

// splitPath 返回路径最后一段（artifactID），供测试拼文件名。
func splitPath(dir string) string {
	last := ""
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' {
			return dir[i+1:]
		}
	}
	return last
}

// TestSearchMavenSnapshotOnlyFlagsSuspect 验证污染检测：字面 SNAPSHOT 且无时间戳构建
// 应被标记为 Suspect——这正是 Nexus 事故里的形态。
func TestSearchMavenSnapshotOnlyFlagsSuspect(t *testing.T) {
	t.Parallel()
	svc, _, _ := newMavenTrashFixture(t, "pkg-snapshot")
	// 幽灵：只有字面 -SNAPSHOT 文件，没有任何时间戳构建。
	if _, err := svc.Put("pkg-snapshot", "com/x/ghost/1.17-SNAPSHOT/ghost-1.17-SNAPSHOT.jar",
		bytes.NewBufferString("old"), "application/java-archive"); err != nil {
		t.Fatalf("发布幽灵：%v", err)
	}
	// 正规：时间戳构建。
	if _, err := svc.Put("pkg-snapshot", "com/x/real/1.17-SNAPSHOT/real-1.17-20260317.215705-3.jar",
		bytes.NewBufferString("new"), "application/java-archive"); err != nil {
		t.Fatalf("发布正规：%v", err)
	}
	// 无关：release 版本。
	if _, err := svc.Put("pkg-snapshot", "com/x/plain/1.0.0/plain-1.0.0.jar",
		bytes.NewBufferString("r"), "application/java-archive"); err != nil {
		t.Fatalf("发布 release：%v", err)
	}

	res, err := svc.SearchMavenPackageVersions("pkg-snapshot", domain.MavenPackageQuery{
		GroupID: "com.x", ArtifactID: "*", SnapshotOnly: true,
	})
	if err != nil {
		t.Fatalf("搜索：%v", err)
	}
	if len(res.Versions) != 2 {
		t.Fatalf("应命中 2 个含 SNAPSHOT 的版本，得 %d", len(res.Versions))
	}
	var ghost, real *domain.MavenPackageVersion
	for i := range res.Versions {
		switch res.Versions[i].Coordinate.ArtifactID {
		case "ghost":
			ghost = &res.Versions[i]
		case "real":
			real = &res.Versions[i]
		}
	}
	if ghost == nil || real == nil {
		t.Fatalf("应同时命中 ghost 与 real：%+v", res.Versions)
	}
	if !ghost.Suspect {
		t.Fatal("字面 SNAPSHOT 且无时间戳构建应标记为 Suspect")
	}
	if real.Suspect {
		t.Fatal("含时间戳构建的正规快照不应标记为 Suspect")
	}
	// 排序契约：疑似问题置顶。
	if !res.Versions[0].Suspect {
		t.Fatal("Suspect 版本应排在最前")
	}
}

// TestSearchMavenPackageSizeAndTimeFilters 验证体积与时间筛选。
func TestSearchMavenPackageSizeAndTimeFilters(t *testing.T) {
	t.Parallel()
	svc, _, _ := newMavenTrashFixture(t, "pkg-filter")
	if _, err := svc.Put("pkg-filter", "com/f/small/1.0.0/small-1.0.0.jar",
		bytes.NewBufferString("s"), "application/java-archive"); err != nil {
		t.Fatalf("发布：%v", err)
	}
	big := bytes.Repeat([]byte("x"), 2048)
	if _, err := svc.Put("pkg-filter", "com/f/big/1.0.0/big-1.0.0.jar",
		bytes.NewReader(big), "application/java-archive"); err != nil {
		t.Fatalf("发布：%v", err)
	}

	// 只要大于 1000 字节的
	res, err := svc.SearchMavenPackageVersions("pkg-filter", domain.MavenPackageQuery{
		GroupID: "com.f", ArtifactID: "*", MinBytes: 1000,
	})
	if err != nil {
		t.Fatalf("搜索：%v", err)
	}
	if len(res.Versions) != 1 || res.Versions[0].Coordinate.ArtifactID != "big" {
		t.Fatalf("MinBytes 应只命中 big，得 %+v", res.Versions)
	}

	// 体积区间上界
	res, err = svc.SearchMavenPackageVersions("pkg-filter", domain.MavenPackageQuery{
		GroupID: "com.f", ArtifactID: "*", MaxBytes: 1000,
	})
	if err != nil {
		t.Fatalf("搜索：%v", err)
	}
	if len(res.Versions) != 1 || res.Versions[0].Coordinate.ArtifactID != "small" {
		t.Fatalf("MaxBytes 应只命中 small，得 %+v", res.Versions)
	}

	// 未来的时间界 → 全部命中（都用 "早于"）
	res, err = svc.SearchMavenPackageVersions("pkg-filter", domain.MavenPackageQuery{
		GroupID: "com.f", ArtifactID: "*", UpdatedBefore: "2099-01-01",
	})
	if err != nil {
		t.Fatalf("搜索：%v", err)
	}
	if len(res.Versions) != 2 {
		t.Fatalf("未来时间界应命中全部 2 个，得 %d", len(res.Versions))
	}

	// 过去的时间界 → 一个都不命中
	res, err = svc.SearchMavenPackageVersions("pkg-filter", domain.MavenPackageQuery{
		GroupID: "com.f", ArtifactID: "*", UpdatedBefore: "2000-01-01",
	})
	if err != nil {
		t.Fatalf("搜索：%v", err)
	}
	if len(res.Versions) != 0 {
		t.Fatalf("过去时间界不应命中任何版本，得 %d", len(res.Versions))
	}
}

// TestSearchMavenPackageRejectsEscape 验证坐标校验：含 `..` 或路径分隔符必须被拒，
// 否则前缀匹配会逃逸到别的目录。
func TestSearchMavenPackageRejectsEscape(t *testing.T) {
	t.Parallel()
	svc, _, _ := newMavenTrashFixture(t, "pkg-escape")
	cases := []domain.MavenPackageQuery{
		{GroupID: "com/../etc", ArtifactID: "demo"},
		{GroupID: "com.tbao", ArtifactID: "../../etc"},
		{GroupID: "", ArtifactID: ""},
		{GroupID: "com.x", ArtifactID: "demo", MinBytes: 100, MaxBytes: 10},
		{GroupID: "com.x", ArtifactID: "demo", UpdatedBefore: "not-a-date"},
	}
	for _, q := range cases {
		if _, err := svc.SearchMavenPackageVersions("pkg-escape", q); err == nil {
			t.Fatalf("条件 %+v 应被拒绝", q)
		}
	}
}

// TestTrashMavenVersionsBatchPartialSuccess 验证批量清理：成功与失败并存时如实报告。
func TestTrashMavenVersionsBatchPartialSuccess(t *testing.T) {
	t.Parallel()
	svc, trash, _ := newMavenTrashFixture(t, "pkg-batch")
	for _, v := range []string{"1.0.0", "2.0.0"} {
		path := "com/b/lib/" + v + "/lib-" + v + ".jar"
		if _, err := svc.Put("pkg-batch", path, bytes.NewBufferString("x"), "application/java-archive"); err != nil {
			t.Fatalf("发布：%v", err)
		}
	}
	result, err := svc.TrashMavenVersionsBatch("pkg-batch", domain.MavenBatchTrashRequest{
		Versions: []domain.MavenVersionCoordinate{
			{GroupID: "com.b", ArtifactID: "lib", Version: "1.0.0"},
			// 不存在的版本：应进入 Failures 而不是让整批失败。
			{GroupID: "com.b", ArtifactID: "lib", Version: "9.9.9"},
			{GroupID: "com.b", ArtifactID: "lib", Version: "2.0.0"},
		},
	}, "tester")
	if err != nil {
		t.Fatalf("部分成功的批量不应整体报错：%v", err)
	}
	if result.TrashedVersions != 2 {
		t.Fatalf("应成功清理 2 个版本，得 %d", result.TrashedVersions)
	}
	if len(result.Failures) != 1 {
		t.Fatalf("应有 1 条失败明细，得 %d", len(result.Failures))
	}
	// 两个成功版本的文件都应进回收站。
	entries, total, err := trash.List("staged", 10, 0)
	if err != nil {
		t.Fatalf("读取回收站：%v", err)
	}
	if total != 2 || len(entries) != 2 {
		t.Fatalf("回收站应有 2 条，得 %d", total)
	}
}

// TestTrashMavenVersionsBatchAllFailedReturnsError 验证全部失败时不能返回"成功 0 个"。
func TestTrashMavenVersionsBatchAllFailedReturnsError(t *testing.T) {
	t.Parallel()
	svc, _, _ := newMavenTrashFixture(t, "pkg-batch-fail")
	_, err := svc.TrashMavenVersionsBatch("pkg-batch-fail", domain.MavenBatchTrashRequest{
		Versions: []domain.MavenVersionCoordinate{
			{GroupID: "com.none", ArtifactID: "nothing", Version: "1.0.0"},
		},
	}, "tester")
	if err == nil {
		t.Fatal("全部失败时必须返回错误，而不是一个显示'清理完成 0 个'的成功结果")
	}
}

// TestTrashMavenVersionsBatchRejectsEmpty 验证空请求被拒。
func TestTrashMavenVersionsBatchRejectsEmpty(t *testing.T) {
	t.Parallel()
	svc, _, _ := newMavenTrashFixture(t, "pkg-batch-empty")
	if _, err := svc.TrashMavenVersionsBatch("pkg-batch-empty", domain.MavenBatchTrashRequest{}, "tester"); err == nil {
		t.Fatal("空版本列表应被拒绝")
	}
}
