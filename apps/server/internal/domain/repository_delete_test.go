package domain_test

import (
	"bytes"
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

	if _, err := repoSvc.Create("to-delete", "raw", "hosted", "private", repository.RepositoryConfig{}); err != nil {
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
	if _, err := repoSvc.Create("to-delete", "raw", "hosted", "private", repository.RepositoryConfig{}); err != nil {
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
