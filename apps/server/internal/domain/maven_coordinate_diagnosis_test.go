package domain_test

import (
	"bytes"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newMavenServiceFixture 构造不带预设仓库的 Maven 服务，供诊断用例自行建仓。
func newMavenServiceFixture(t *testing.T, db *persistence.DB) (*domain.AssetService, *repository.RepoRepo) {
	t.Helper()
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	svc := domain.NewAssetService(repos, assets, blobs, nil)
	svc.SetChangeRecorder(domain.NoopChangeRecorder{})
	svc.SetNodeIdentity(domain.NewNodeIdentity(repository.NewSettingRepo(db)))
	svc.SetAssetTrashRepo(repository.NewAssetTrashRepo(db))
	return svc, repos
}

// TestDiagnoseDetectsDistinctContentAcrossRepos 验证核心场景：
// 同一坐标在两个仓库里内容不同 → 报告必须指出这是遮蔽风险，而不是"重复冗余"。
//
// 这正是线上事故的形态：releases 与 snapshots 都有 1.17-SNAPSHOT，
// 内容差十几 KB，聚合仓按成员顺序返回了旧的那份。
func TestDiagnoseDetectsDistinctContentAcrossRepos(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, repos := newMavenServiceFixture(t, db)
	for _, name := range []string{"releases-ish", "snapshots-ish"} {
		if _, err := repos.Create(name, "maven", "hosted", "private", ""); err != nil {
			t.Fatalf("建仓库 %s：%v", name, err)
		}
	}
	const dir = "com/example/demo/1.17-SNAPSHOT"
	// releases 里是旧的幽灵副本（字面 SNAPSHOT），snapshots 里是时间戳构建。
	if _, err := svc.Put("releases-ish", dir+"/demo-1.17-SNAPSHOT.jar",
		bytes.NewBufferString("old-ghost"), "application/java-archive"); err != nil {
		t.Fatalf("发布幽灵副本：%v", err)
	}
	if _, err := svc.Put("snapshots-ish", dir+"/demo-1.17-20260317.215705-3.jar",
		bytes.NewBufferString("new-correct-content"), "application/java-archive"); err != nil {
		t.Fatalf("发布正确副本：%v", err)
	}

	report, err := svc.DiagnoseMavenCoordinate(domain.MavenVersionCoordinate{
		GroupID: "com.example", ArtifactID: "demo", Version: "1.17-SNAPSHOT",
	})
	if err != nil {
		t.Fatalf("诊断：%v", err)
	}
	if len(report.Sources) != 2 {
		t.Fatalf("应发现 2 个来源，得 %d（%+v）", len(report.Sources), report.Sources)
	}
	// 内容不同 → 不是纯冗余，必须报遮蔽风险。
	if report.Identical {
		t.Fatal("内容不同时 Identical 必须为 false")
	}
	if report.DistinctFingerprints != 2 {
		t.Fatalf("应有 2 个不同指纹，得 %d", report.DistinctFingerprints)
	}
	if report.Warning == "" {
		t.Fatal("存在真遮蔽风险时必须给出警告文案")
	}

	// 字面 SNAPSHOT 必须被标记出来：它没有时间戳，是不可复现的覆盖写。
	var literalMarked, buildMarked bool
	for _, source := range report.Sources {
		if source.Repository == "releases-ish" && source.HasLiteralSnapshot {
			literalMarked = true
		}
		if source.Repository == "snapshots-ish" && len(source.SnapshotBuilds) == 1 &&
			source.SnapshotBuilds[0] == "20260317.215705-3" {
			buildMarked = true
		}
	}
	if !literalMarked {
		t.Fatal("releases 里的字面 -SNAPSHOT 必须被标记")
	}
	if !buildMarked {
		t.Fatalf("snapshots 里应识别出时间戳构建，得 %+v", report.Sources)
	}
}

// TestDiagnoseReportsIdenticalWhenSameContent 验证纯冗余不会被误报成风险。
func TestDiagnoseReportsIdenticalWhenSameContent(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, repos := newMavenServiceFixture(t, db)
	for _, name := range []string{"mirror-a", "mirror-b"} {
		if _, err := repos.Create(name, "maven", "hosted", "private", ""); err != nil {
			t.Fatalf("建仓库 %s：%v", name, err)
		}
	}
	const path = "com/example/same/1.0/same-1.0.jar"
	// 同一路径、同一内容 → 指纹必须一致。
	if _, err := svc.Put("mirror-a", path, bytes.NewBufferString("identical"), "application/java-archive"); err != nil {
		t.Fatalf("发布 a：%v", err)
	}
	if _, err := svc.Put("mirror-b", path, bytes.NewBufferString("identical"), "application/java-archive"); err != nil {
		t.Fatalf("发布 b：%v", err)
	}
	report, err := svc.DiagnoseMavenCoordinate(domain.MavenVersionCoordinate{
		GroupID: "com.example", ArtifactID: "same", Version: "1.0",
	})
	if err != nil {
		t.Fatalf("诊断：%v", err)
	}
	if len(report.Sources) != 2 {
		t.Fatalf("应发现 2 个来源，得 %d", len(report.Sources))
	}
	if !report.Identical || report.DistinctFingerprints != 1 {
		t.Fatalf("内容相同时应为纯冗余：identical=%v distinct=%d", report.Identical, report.DistinctFingerprints)
	}
	// 指纹相同也要能区分（供界面展示），不能是空串。
	if report.Sources[0].Fingerprint == "" {
		t.Fatal("指纹不应为空")
	}
	if report.Sources[0].Fingerprint != report.Sources[1].Fingerprint {
		t.Fatal("相同内容必须得到相同指纹")
	}
}

// TestDiagnoseIgnoresProxyAndGroupRepos 验证只统计 hosted：
// proxy/group 的"存在"取决于上游或成员，算进来会产生"同一份内容被数三次"的假重复。
func TestDiagnoseIgnoresProxyAndGroupRepos(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, repos := newMavenServiceFixture(t, db)
	if _, err := repos.Create("only-hosted", "maven", "hosted", "private", ""); err != nil {
		t.Fatalf("建 hosted：%v", err)
	}
	const path = "com/example/proxy/1.0/proxy-1.0.jar"
	if _, err := svc.Put("only-hosted", path, bytes.NewBufferString("x"), "application/java-archive"); err != nil {
		t.Fatalf("发布：%v", err)
	}
	report, err := svc.DiagnoseMavenCoordinate(domain.MavenVersionCoordinate{
		GroupID: "com.example", ArtifactID: "proxy", Version: "1.0",
	})
	if err != nil {
		t.Fatalf("诊断：%v", err)
	}
	if len(report.Sources) != 1 {
		t.Fatalf("只应统计 1 个 hosted 来源，得 %d（%+v）", len(report.Sources), report.Sources)
	}
	if !report.Identical {
		t.Fatal("单一来源应视为一致")
	}
}

// TestDiagnoseRejectsInvalidCoordinate 验证非法坐标被拒。
func TestDiagnoseRejectsInvalidCoordinate(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc, _ := newMavenServiceFixture(t, db)
	_, err := svc.DiagnoseMavenCoordinate(domain.MavenVersionCoordinate{
		GroupID: "com/../etc", ArtifactID: "demo", Version: "1.0",
	})
	if err == nil {
		t.Fatal("含路径分隔符的坐标必须被拒绝")
	}
}
