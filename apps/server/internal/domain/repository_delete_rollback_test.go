package domain

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestDeleteRepositoryFinalSnapshotCleanupFailureKeepsCommittedCascadeDelete(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	acls := repository.NewAclRepo(db)
	metadata := repository.NewFormatMetadataRepo(db)
	users := repository.NewUserRepo(db)
	blobs := blobstore.NewStore(filepath.Join(t.TempDir(), "blobs"))
	repoSvc := NewRepositoryService(repos, acls, assets, NewSettingService(repository.NewSettingRepo(db)), users)
	repoSvc.SetFormatMetadataRepo(metadata)
	assetSvc := NewAssetService(repos, assets, blobs, nil)
	mutator, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建资产协调器：%v", err)
	}
	mutator.finalizeSnapshot = func(blobstore.RollbackSnapshot) error { return errors.New("注入最终回收失败") }
	repoSvc.SetMutationCoordinator(mutator)
	assetSvc.SetMutationCoordinator(mutator)

	repo, err := repoSvc.Create("rollback-repo", "raw", "hosted", "private", "", repository.RepositoryConfig{})
	if err != nil {
		t.Fatalf("创建仓库：%v", err)
	}
	if _, err := assetSvc.Put("rollback-repo", "demo.bin", bytes.NewReader([]byte("payload")), ""); err != nil {
		t.Fatalf("写入资产：%v", err)
	}
	asset, err := assets.GetByPath(repo.ID, "demo.bin")
	if err != nil {
		t.Fatalf("读取资产：%v", err)
	}
	if err := metadata.Put(repository.FormatMetadata{
		RepositoryID: repo.ID, Format: "raw", NameNormalized: "demo", NameDisplay: "demo",
		Version: "1.0.0", VersionNormalized: "1.0.0", Filename: "demo.bin", AssetPath: asset.Path,
		Sha256: asset.BlobHash, Size: asset.Size,
	}); err != nil {
		t.Fatalf("写入格式元数据：%v", err)
	}
	userID, err := users.Create("rollback-user", "hash", "user")
	if err != nil {
		t.Fatalf("创建 ACL 用户：%v", err)
	}
	if err := acls.Replace(repo.ID, []repository.Acl{{SubjectID: userID, Action: "read"}}); err != nil {
		t.Fatalf("写入 ACL：%v", err)
	}

	if err := repoSvc.Delete("rollback-repo"); err != nil {
		t.Fatalf("完成事务后的快照清理失败不得回滚仓库删除：%v", err)
	}
	if _, err := repoSvc.Get("rollback-repo"); err == nil {
		t.Fatal("快照清理失败后已提交仓库删除不得恢复")
	}
	if _, err := assets.GetByPath(repo.ID, asset.Path); err == nil {
		t.Fatal("快照清理失败后已提交资产删除不得恢复")
	}
	if blobs.Exists(asset.BlobHash) {
		t.Fatal("快照清理失败后归零 blob 不得回到活动存储")
	}
	if _, err := metadata.Get(repo.ID, "raw", "demo", "demo.bin"); err == nil {
		t.Fatal("快照清理失败后格式元数据删除不得恢复")
	}
	entries, err := acls.ListByRepo(repo.ID)
	if err != nil || len(entries) != 0 {
		t.Fatalf("快照清理失败后 ACL 删除不得恢复：%+v err=%v", entries, err)
	}
	_ = userID
}
