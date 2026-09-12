package domain_test

import (
	"bytes"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestCleanupEmptyMavenArtifactsIsAtomicOnPhysicalFailure(t *testing.T) {
	db := newTestDB(t)
	repoRepo := repository.NewRepoRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	repoSvc := domain.NewRepositoryService(repoRepo, repository.NewAclRepo(db), assetRepo, domain.NewSettingService(repository.NewSettingRepo(db)), repository.NewUserRepo(db))
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, nil)
	mutator, err := domain.NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建资产协调器：%v", err)
	}
	repoSvc.SetMutationCoordinator(mutator)
	assetSvc.SetMutationCoordinator(mutator)
	if _, err := repoSvc.Create("maven", "maven", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("创建 Maven 仓库：%v", err)
	}
	if _, err := assetSvc.Put("maven", "com/example/good/1.0/good-1.0.pom", bytes.NewReader([]byte("good")), "application/xml"); err != nil {
		t.Fatalf("写有效 POM：%v", err)
	}
	repo, err := repoSvc.Get("maven")
	if err != nil {
		t.Fatalf("读取 Maven 仓库：%v", err)
	}
	if err := assetRepo.Upsert(repo.ID, "com/example/missing/1.0/missing-1.0.pom", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 1, "application/xml", "", ""); err != nil {
		t.Fatalf("写缺失 blob 的 POM：%v", err)
	}

	if _, err := repoSvc.CleanupEmptyMavenArtifacts("maven"); err == nil {
		t.Fatal("任一 blob 无法隔离时 Maven 清理必须整批失败")
	}
	for _, path := range []string{
		"com/example/good/1.0/good-1.0.pom",
		"com/example/missing/1.0/missing-1.0.pom",
	} {
		if _, err := assetRepo.GetByPath(repo.ID, path); err != nil {
			t.Fatalf("清理失败后资产不得部分删除 path=%s err=%v", path, err)
		}
	}
}
