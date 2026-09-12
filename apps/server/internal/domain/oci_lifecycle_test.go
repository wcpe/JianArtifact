package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestOCIStagedBlobTTLRemovesHiddenContent(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	if _, err := repos.Create("oci-ttl", "docker", "hosted", "private", ""); err != nil {
		t.Fatalf("创建 OCI 仓库：%v", err)
	}
	store := blobstore.NewStore(t.TempDir())
	assetSvc := NewAssetService(repos, assets, store, nil)
	oci := NewOCIService(assetSvc, NewRepositoryService(repos, repository.NewAclRepo(db), assets, NewSettingService(repository.NewSettingRepo(db)), repository.NewUserRepo(db)))
	oci.stagedTTL = 10 * time.Millisecond

	body := []byte("oci-staged-ttl")
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	asset, err := oci.PutBlob("oci-ttl", digest, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("暂存 OCI blob：%v", err)
	}
	if !store.Exists(asset.BlobHash) {
		t.Fatal("TTL 前暂存 blob 应存在")
	}
	deadline := time.Now().Add(time.Second)
	for store.Exists(asset.BlobHash) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if store.Exists(asset.BlobHash) {
		t.Fatal("过期未提交 OCI blob 必须物理回收")
	}
	oci.mu.Lock()
	pendingCount := len(oci.pending)
	oci.mu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("过期 OCI 暂存记录必须清理，实际=%d", pendingCount)
	}
}

func TestStartupCleanupRemovesOnlyUnreferencedBlobs(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	repoID, err := repos.Create("oci-cleanup", "docker", "hosted", "private", "")
	if err != nil {
		t.Fatalf("创建仓库：%v", err)
	}
	store := blobstore.NewStore(t.TempDir())
	assetSvc := NewAssetService(repos, assets, store, nil)
	referenced, sha1sum, md5sum, size, err := store.Put(bytes.NewReader([]byte("referenced")))
	if err != nil {
		t.Fatalf("写入被引用 blob：%v", err)
	}
	if err := assets.Upsert(repoID, "oci/blobs/referenced", referenced, size, "application/octet-stream", sha1sum, md5sum); err != nil {
		t.Fatalf("写入引用：%v", err)
	}
	orphan, _, _, _, err := store.Put(bytes.NewReader([]byte("orphan")))
	if err != nil {
		t.Fatalf("写入孤立 blob：%v", err)
	}
	removed, err := assetSvc.CleanupUnreferencedBlobs()
	if err != nil {
		t.Fatalf("启动清理未引用 blob：%v", err)
	}
	if removed != 1 || !store.Exists(referenced) || store.Exists(orphan) {
		t.Fatalf("启动清理结果 removed=%d referenced=%t orphan=%t", removed, store.Exists(referenced), store.Exists(orphan))
	}
}
