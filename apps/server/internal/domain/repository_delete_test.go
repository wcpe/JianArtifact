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
	t.Parallel()
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

func TestDeleteRepositoryNonHostedAndOverLimit(t *testing.T) {
	t.Parallel()
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

	// proxy 与 group 都能删除：删仓是管理员对仓库本身的动作，与「代理仓不承载写入」
	// 分属两件事。此前一律拒绝，导致误建的代理仓再也清不掉、界面点删除必失败。
	// group 仓库按规格必须至少有一个同格式成员，先建一个 hosted 成员供其引用。
	if _, err := repoSvc.Create("non-hosted-member", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("创建 group 成员：%v", err)
	}
	for _, repoType := range []string{"proxy", "group"} {
		name := "non-hosted-" + repoType
		// group 是成员聚合，不接受 remoteUrl、但必须有成员；proxy 需要上游地址。
		cfg := repository.RepositoryConfig{}
		if repoType == "proxy" {
			cfg.RemoteURL = "https://example.invalid"
		} else {
			cfg.Members = []string{"non-hosted-member"}
		}
		if _, err := repoSvc.Create(name, "raw", repoType, "private", "", cfg); err != nil {
			t.Fatalf("创建 %s：%v", repoType, err)
		}
		// 给 proxy 造一条缓存制品，验证删仓时连带清理。
		if repoType == "proxy" {
			repo, err := repoRepo.GetByName(name)
			if err != nil {
				t.Fatalf("读取 %s：%v", repoType, err)
			}
			// 用真实 blob 写入再建 asset 行：删除链路会校验 blob 哈希格式，
			// 手造假哈希会在删仓时被判「非法的 blob 哈希」而与本用例无关。
			hash, sha1sum, md5sum, size, err := blobs.Put(bytes.NewReader([]byte("cached")))
			if err != nil {
				t.Fatalf("写入 %s 缓存 blob：%v", repoType, err)
			}
			if err := assetRepo.Upsert(repo.ID, "cached.bin", hash, size, "application/octet-stream", sha1sum, md5sum); err != nil {
				t.Fatalf("造 %s 缓存制品：%v", repoType, err)
			}
		}
		if err := repoSvc.Delete(name); err != nil {
			t.Fatalf("删除 %s 仓库应成功，实际：%v", repoType, err)
		}
		if _, err := repoSvc.Get(name); err == nil {
			t.Fatalf("%s 仓库应已删除", repoType)
		}
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
	t.Parallel()
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
