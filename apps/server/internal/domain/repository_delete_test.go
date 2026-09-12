package domain_test

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestDeleteRepositoryCascadesAssets 删除仓库后 asset 行须级联清除，便于真机复测腾出元数据。
func TestDeleteRepositoryCascadesAssets(t *testing.T) {
	db := newTestDB(t)
	repoRepo := repository.NewRepoRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	aclRepo := repository.NewAclRepo(db)
	blobs := blobstore.NewStore(filepath.Join(t.TempDir(), "blobs"))
	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, assetRepo, domain.NewSettingService(repository.NewSettingRepo(db)), repository.NewUserRepo(db))
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, nil)
	mutator, err := domain.NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建资产协调器：%v", err)
	}
	repoSvc.SetMutationCoordinator(mutator)
	assetSvc.SetMutationCoordinator(mutator)

	if _, err := repoSvc.Create("to-delete", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := assetSvc.Put("to-delete", "x.bin", bytes.NewReader([]byte("data")), ""); err != nil {
		t.Fatal(err)
	}
	// 确认资产存在后立即关闭流，避免 Windows 上 TempDir 清理占用
	_, rc, err := assetSvc.Get("to-delete", "x.bin")
	if err != nil {
		t.Fatalf("Put 后 Get：%v", err)
	}
	_ = rc.Close()

	if err := repoSvc.Delete("to-delete"); err != nil {
		t.Fatalf("Delete：%v", err)
	}
	if _, err := repoSvc.Get("to-delete"); err == nil {
		t.Fatal("仓库应已删除")
	}
	// 同名重建应成功且无旧资产
	if _, err := repoSvc.Create("to-delete", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("重建：%v", err)
	}
	if _, _, err := assetSvc.Get("to-delete", "x.bin"); err == nil {
		t.Fatal("删除后资产应不存在（级联）")
	}
	// 再删除干净
	if err := repoSvc.Delete("to-delete"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteRepositoryRejectsNonHostedAndOverLimitWithoutSideEffects(t *testing.T) {
	db := newTestDB(t)
	repoRepo := repository.NewRepoRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(filepath.Join(t.TempDir(), "blobs"))
	repoSvc := domain.NewRepositoryService(repoRepo, repository.NewAclRepo(db), assetRepo, domain.NewSettingService(repository.NewSettingRepo(db)), repository.NewUserRepo(db))
	mutator, err := domain.NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建资产协调器：%v", err)
	}
	repoSvc.SetMutationCoordinator(mutator)

	if _, err := repoSvc.Create("proxy", "raw", "proxy", "private", "", repository.RepositoryConfig{RemoteURL: "https://example.invalid"}); err != nil {
		t.Fatalf("创建 proxy：%v", err)
	}
	if err := repoSvc.Delete("proxy"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("删除非 hosted 仓库应拒绝，实际：%v", err)
	}
	if _, err := repoSvc.Get("proxy"); err != nil {
		t.Fatalf("非 hosted 拒绝后仓库不得消失：%v", err)
	}

	hosted, err := repoSvc.Create("too-many", "raw", "hosted", "private", "", repository.RepositoryConfig{})
	if err != nil {
		t.Fatalf("创建 hosted：%v", err)
	}
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for i := 0; i <= 500; i++ {
		if err := assetRepo.Upsert(hosted.ID, fmt.Sprintf("dir/asset-%03d", i), hash, 1, "application/octet-stream", "", ""); err != nil {
			t.Fatalf("预置资产 %d：%v", i, err)
		}
	}
	if err := repoSvc.Delete("too-many"); !errors.Is(err, domain.ErrOperationLimit) {
		t.Fatalf("超过 500 条必须拒绝，实际：%v", err)
	}
	if _, err := repoSvc.Get("too-many"); err != nil {
		t.Fatalf("超限拒绝后仓库不得消失：%v", err)
	}
	count, err := assetRepo.CountByRepo(hosted.ID, "")
	if err != nil || count != 501 {
		t.Fatalf("超限拒绝后资产不得变化：count=%d err=%v", count, err)
	}
}

func TestDeleteRepositoryRowFailureRestoresRepositoryAclMetadataAssetAndBlob(t *testing.T) {
	db := newTestDB(t)
	repoRepo := repository.NewRepoRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	aclRepo := repository.NewAclRepo(db)
	metadataRepo := repository.NewFormatMetadataRepo(db)
	userRepo := repository.NewUserRepo(db)
	blobs := blobstore.NewStore(filepath.Join(t.TempDir(), "blobs"))
	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, assetRepo, domain.NewSettingService(repository.NewSettingRepo(db)), userRepo)
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, nil)
	mutator, err := domain.NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建资产协调器：%v", err)
	}
	repoSvc.SetMutationCoordinator(mutator)
	assetSvc.SetMutationCoordinator(mutator)

	repo, err := repoSvc.Create("delete-failure", "raw", "hosted", "private", "", repository.RepositoryConfig{})
	if err != nil {
		t.Fatalf("创建仓库：%v", err)
	}
	if _, err := assetSvc.Put("delete-failure", "packages/demo-1.0.0.tar.gz", bytes.NewReader([]byte("payload")), ""); err != nil {
		t.Fatalf("写入资产：%v", err)
	}
	asset, err := assetRepo.GetByPath(repo.ID, "packages/demo-1.0.0.tar.gz")
	if err != nil {
		t.Fatalf("读取资产：%v", err)
	}
	if err := metadataRepo.Put(repository.FormatMetadata{
		RepositoryID: repo.ID, Format: "raw", NameNormalized: "demo", NameDisplay: "demo",
		Version: "1.0.0", VersionNormalized: "1.0.0", Filename: "demo-1.0.0.tar.gz",
		AssetPath: asset.Path, Sha256: asset.BlobHash, Size: asset.Size,
	}); err != nil {
		t.Fatalf("写入格式元数据：%v", err)
	}
	userID, err := userRepo.Create("delete-check", "hash", "user")
	if err != nil {
		t.Fatalf("创建 ACL 用户：%v", err)
	}
	if err := aclRepo.Replace(repo.ID, []repository.Acl{{SubjectID: userID, Action: "read"}}); err != nil {
		t.Fatalf("写入 ACL：%v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_repository_delete BEFORE DELETE ON repository BEGIN SELECT RAISE(ABORT, '注入仓库删除失败'); END`); err != nil {
		t.Fatalf("创建失败注入：%v", err)
	}

	if err := repoSvc.Delete("delete-failure"); err == nil {
		t.Fatal("仓库行删除失败必须返回错误")
	}
	if _, err := repoSvc.Get("delete-failure"); err != nil {
		t.Fatalf("仓库删除失败后必须保留：%v", err)
	}
	if _, err := assetRepo.GetByPath(repo.ID, asset.Path); err != nil {
		t.Fatalf("仓库删除失败后资产必须保留：%v", err)
	}
	if !blobs.Exists(asset.BlobHash) {
		t.Fatal("仓库删除失败后 blob 必须保留")
	}
	if _, err := metadataRepo.Get(repo.ID, "raw", "demo", "demo-1.0.0.tar.gz"); err != nil {
		t.Fatalf("仓库删除失败后格式元数据必须保留：%v", err)
	}
	acls, err := aclRepo.ListByRepo(repo.ID)
	if err != nil || len(acls) != 1 || acls[0].SubjectID != userID || acls[0].Action != "read" {
		t.Fatalf("仓库删除失败后 ACL 必须保留：%+v err=%v", acls, err)
	}
}
