package domain

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestNewOperationIDUsesUUIDv7(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	first := newOperationID()
	if !pattern.MatchString(first) {
		t.Fatalf("operationId 必须为 UUIDv7，实际 %q", first)
	}
	if second := newOperationID(); second == first {
		t.Fatalf("连续 operationId 不得重复：%q", first)
	}
}

func mutationTestDB(t *testing.T) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "mutation.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return db
}

func TestAssetMutationSharedReferenceAndZeroReference(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	c, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建协调器：%v", err)
	}
	hash, sha1sum, md5sum, size, err := blobs.Put(bytes.NewReader([]byte("shared")))
	if err != nil {
		t.Fatalf("写 blob：%v", err)
	}
	for _, path := range []string{"a", "b"} {
		if err := assets.Upsert(repoID, path, hash, size, "text/plain", sha1sum, md5sum); err != nil {
			t.Fatalf("写资产 %s：%v", path, err)
		}
	}
	if err := c.Delete(repoID, "a"); err != nil {
		t.Fatalf("删除共享引用：%v", err)
	}
	if !blobs.Exists(hash) {
		t.Fatal("仍有引用时 blob 不应回收")
	}
	if err := c.Delete(repoID, "b"); err != nil {
		t.Fatalf("删除最后引用：%v", err)
	}
	if blobs.Exists(hash) {
		t.Fatal("引用归零后活动 blob 应回收")
	}
}

func TestAssetMutationBatchReclaimsLastSharedBlob(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	c, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建协调器：%v", err)
	}
	hash, sha1sum, md5sum, size, err := blobs.Put(bytes.NewReader([]byte("batch")))
	if err != nil {
		t.Fatalf("写 blob：%v", err)
	}
	items := make([]repository.AssetMutationItem, 0, 2)
	for i, path := range []string{"a", "b"} {
		before := &repository.Asset{RepositoryID: repoID, Path: path, BlobHash: hash, Size: size, ContentType: "text/plain", Sha1: sha1sum, Md5: md5sum}
		if err := assets.Upsert(repoID, path, hash, size, "text/plain", sha1sum, md5sum); err != nil {
			t.Fatalf("写资产：%v", err)
		}
		items = append(items, repository.AssetMutationItem{Ordinal: i, RepositoryID: repoID, Path: path, Before: before})
	}
	if err := c.Apply(items); err != nil {
		t.Fatalf("批量删除：%v", err)
	}
	if blobs.Exists(hash) {
		t.Fatal("批量删除最后引用后 blob 应回收")
	}
}

func TestAssetMutationStageFailureLeavesMetadata(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := assets.Upsert(repoID, "missing", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 1, "", "", ""); err != nil {
		t.Fatalf("写测试资产：%v", err)
	}
	c, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建协调器：%v", err)
	}
	if err := c.Delete(repoID, "missing"); err == nil {
		t.Fatal("隔离缺失 blob 应失败")
	}
	if _, err := assets.GetByPath(repoID, "missing"); err != nil {
		t.Fatalf("隔离失败后元数据应保留：%v", err)
	}
}

// TestAssetMutationFinalizeFailureRollsBackMetadataAndBlob 确保最终物理回收失败时，
// 删除不会留下“元数据已删但字节仍在隔离区”的半完成状态。
func TestAssetMutationFinalizeFailureRollsBackMetadataAndBlob(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	hash, sha1sum, md5sum, size, err := blobs.Put(bytes.NewReader([]byte("finalize failure")))
	if err != nil {
		t.Fatalf("写 blob：%v", err)
	}
	if err := assets.Upsert(repoID, "a.bin", hash, size, "application/octet-stream", sha1sum, md5sum); err != nil {
		t.Fatalf("写资产：%v", err)
	}
	c, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建协调器：%v", err)
	}
	c.finalize = func(blobstore.QuarantineEntry) error { return errors.New("注入最终回收失败") }

	if err := c.Delete(repoID, "a.bin"); err == nil {
		t.Fatal("最终物理回收失败必须返回错误")
	}
	if _, err := assets.GetByPath(repoID, "a.bin"); err != nil {
		t.Fatalf("最终回收失败后资产元数据必须恢复：%v", err)
	}
	if !blobs.Exists(hash) {
		t.Fatal("最终回收失败后 blob 必须恢复到活动存储")
	}
}

// TestAssetMutationFinalizeFailureRestoresWholeBatch 确保批内已有 blob 被物理删除后，
// 后续回收失败仍能把整批字节和元数据一起恢复。
func TestAssetMutationFinalizeFailureRestoresWholeBatch(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	c, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建协调器：%v", err)
	}
	items := make([]repository.AssetMutationItem, 0, 2)
	for _, path := range []string{"a.bin", "b.bin"} {
		hash, sha1sum, md5sum, size, err := blobs.Put(bytes.NewReader([]byte(path)))
		if err != nil {
			t.Fatalf("写 blob %s：%v", path, err)
		}
		asset := repository.Asset{RepositoryID: repoID, Path: path, BlobHash: hash, Size: size, ContentType: "application/octet-stream", Sha1: sha1sum, Md5: md5sum}
		if err := assets.Upsert(repoID, path, hash, size, asset.ContentType, sha1sum, md5sum); err != nil {
			t.Fatalf("写资产 %s：%v", path, err)
		}
		items = append(items, repository.AssetMutationItem{RepositoryID: repoID, Path: path, Before: &asset})
	}
	called := 0
	c.finalize = func(entry blobstore.QuarantineEntry) error {
		called++
		if called == 2 {
			return errors.New("注入第二个 blob 回收失败")
		}
		return blobs.Finalize(entry)
	}
	if err := c.Apply(items); err == nil {
		t.Fatal("批内任一最终回收失败必须整批失败")
	}
	for _, item := range items {
		if _, err := assets.GetByPath(repoID, item.Path); err != nil {
			t.Fatalf("整批失败后资产必须恢复 path=%s err=%v", item.Path, err)
		}
		if !blobs.Exists(item.Before.BlobHash) {
			t.Fatalf("整批失败后 blob 必须恢复 hash=%s", item.Before.BlobHash)
		}
	}
}

func TestAssetMutationFinalSnapshotFailureKeepsCommittedStateForCleanupRetry(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	hash, sha1sum, md5sum, size, err := blobs.Put(bytes.NewReader([]byte("snapshot failure")))
	if err != nil {
		t.Fatalf("写 blob：%v", err)
	}
	if err := assets.Upsert(repoID, "a.bin", hash, size, "application/octet-stream", sha1sum, md5sum); err != nil {
		t.Fatalf("写资产：%v", err)
	}
	c, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建协调器：%v", err)
	}
	c.finalizeSnapshot = func(blobstore.RollbackSnapshot) error { return errors.New("注入快照删除失败") }
	if err := c.Delete(repoID, "a.bin"); err != nil {
		t.Fatalf("完成事务后的快照清理失败不得回滚业务结果：%v", err)
	}
	if _, err := assets.GetByPath(repoID, "a.bin"); err == nil {
		t.Fatal("快照清理失败后已提交删除不得恢复")
	}
	if blobs.Exists(hash) {
		t.Fatal("快照清理失败后归零 blob 不得回到活动存储")
	}
	c.finalizeSnapshot = blobs.FinalizeRollbackSnapshot
	if err := c.Recover(); err != nil {
		t.Fatalf("启动恢复应只重试删除冗余快照：%v", err)
	}
	if _, err := assets.GetByPath(repoID, "a.bin"); err == nil || blobs.Exists(hash) {
		t.Fatal("清理重试不得补偿回滚已提交删除")
	}
}

func TestAssetMutationRecoverStagedBlob(t *testing.T) {
	db := mutationTestDB(t)
	blobs := blobstore.NewStore(t.TempDir())
	c, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建协调器：%v", err)
	}
	hash, _, _, _, err := blobs.Put(bytes.NewReader([]byte("crash")))
	if err != nil {
		t.Fatalf("写 blob：%v", err)
	}
	entry, err := blobs.Stage(hash, "op-crash")
	if err != nil {
		t.Fatalf("隔离 blob：%v", err)
	}
	if err := c.mutations.Begin("op-crash", nil); err != nil {
		t.Fatalf("写 intent：%v", err)
	}
	if err := c.mutations.MarkStaged("op-crash", []repository.BlobQuarantine{{OperationID: entry.OperationID, BlobHash: entry.Hash, QuarantinePath: entry.Path}}); err != nil {
		t.Fatalf("写 staged：%v", err)
	}
	c2, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("重启恢复：%v", err)
	}
	if c2 == nil || !blobs.Exists(hash) {
		t.Fatal("恢复后活动 blob 应存在")
	}
	if _, err := c2.mutations.Get("op-crash"); err != nil && !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("读取恢复 intent：%v", err)
	}
}

// TestAssetServiceConstructionDefersMutationRecovery 确保角色尚未完成装配时，
// AssetService 不会擅自恢复或回收本地遗留 intent。
func TestAssetServiceConstructionDefersMutationRecovery(t *testing.T) {
	for _, status := range []string{"staged", "pending_gc"} {
		t.Run(status, func(t *testing.T) {
			db := mutationTestDB(t)
			repos := repository.NewRepoRepo(db)
			assets := repository.NewAssetRepo(db)
			blobs := blobstore.NewStore(t.TempDir())
			repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
			if err != nil {
				t.Fatalf("创建仓库：%v", err)
			}
			hash, sha1sum, md5sum, size, err := blobs.Put(bytes.NewReader([]byte("历史 intent")))
			if err != nil {
				t.Fatalf("写入 blob：%v", err)
			}
			if err := assets.Upsert(repoID, "legacy.bin", hash, size, "application/octet-stream", sha1sum, md5sum); err != nil {
				t.Fatalf("预置资产：%v", err)
			}
			operationID := "op-legacy-" + status
			entry, err := blobs.Stage(hash, operationID)
			if err != nil {
				t.Fatalf("隔离历史 blob：%v", err)
			}
			mutations := repository.NewAssetMutationRepo(db)
			item := repository.AssetMutationItem{RepositoryID: repoID, Path: "legacy.bin", Before: &repository.Asset{RepositoryID: repoID, Path: "legacy.bin", BlobHash: hash, Size: size, ContentType: "application/octet-stream", Sha1: sha1sum, Md5: md5sum}}
			if err := mutations.Begin(operationID, []repository.AssetMutationItem{item}); err != nil {
				t.Fatalf("写入历史 intent：%v", err)
			}
			if err := mutations.MarkStaged(operationID, []repository.BlobQuarantine{{OperationID: operationID, BlobHash: hash, QuarantinePath: entry.Path}}); err != nil {
				t.Fatalf("标记历史隔离：%v", err)
			}
			if status == "pending_gc" {
				if err := mutations.Complete(operationID, []repository.AssetMutationItem{item}); err != nil {
					t.Fatalf("完成历史 intent：%v", err)
				}
				if err := mutations.MarkQuarantine(operationID, hash, status, ""); err != nil {
					t.Fatalf("标记待回收：%v", err)
				}
			}

			_ = NewAssetService(repos, assets, blobs, nil)

			if blobs.Exists(hash) {
				t.Fatal("构造服务不得恢复 staged 历史 blob")
			}
			if _, err := os.Stat(entry.Path); err != nil {
				t.Fatalf("构造服务不得回收隔离 blob：%v", err)
			}
			mutationStatus := status
			if status == "pending_gc" {
				mutationStatus = "completed"
			}
			mutation, err := mutations.Get(operationID)
			if err != nil || mutation.Status != mutationStatus {
				t.Fatalf("历史 intent 状态被改变：mutation=%+v err=%v", mutation, err)
			}
			entries, err := mutations.ListQuarantine(operationID)
			if err != nil || len(entries) != 1 || entries[0].Status != status {
				t.Fatalf("历史隔离记录被改变：entries=%+v err=%v", entries, err)
			}
			asset, err := assets.GetByPath(repoID, "legacy.bin")
			if status == "staged" && (err != nil || asset.BlobHash != hash) {
				t.Fatalf("staged 历史资产元数据被改变：asset=%+v err=%v", asset, err)
			}
			if status == "pending_gc" && !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("pending_gc 历史资产元数据被改变：asset=%+v err=%v", asset, err)
			}
		})
	}
}

// TestAssetServiceExplicitMutationRecovery 确保主节点和禁用复制节点可在装配完成后
// 显式恢复遗留 intent，保留原有启动恢复语义。
func TestAssetServiceExplicitMutationRecovery(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("创建仓库：%v", err)
	}
	hash, sha1sum, md5sum, size, err := blobs.Put(bytes.NewReader([]byte("待恢复内容")))
	if err != nil {
		t.Fatalf("写入 blob：%v", err)
	}
	if err := assets.Upsert(repoID, "recover.bin", hash, size, "application/octet-stream", sha1sum, md5sum); err != nil {
		t.Fatalf("预置资产：%v", err)
	}
	const operationID = "op-explicit-recover"
	entry, err := blobs.Stage(hash, operationID)
	if err != nil {
		t.Fatalf("隔离 blob：%v", err)
	}
	mutations := repository.NewAssetMutationRepo(db)
	item := repository.AssetMutationItem{RepositoryID: repoID, Path: "recover.bin", Before: &repository.Asset{RepositoryID: repoID, Path: "recover.bin", BlobHash: hash, Size: size, ContentType: "application/octet-stream", Sha1: sha1sum, Md5: md5sum}}
	if err := mutations.Begin(operationID, []repository.AssetMutationItem{item}); err != nil {
		t.Fatalf("写入 intent：%v", err)
	}
	if err := mutations.MarkStaged(operationID, []repository.BlobQuarantine{{OperationID: operationID, BlobHash: hash, QuarantinePath: entry.Path}}); err != nil {
		t.Fatalf("标记隔离：%v", err)
	}

	svc := NewAssetService(repos, assets, blobs, nil)
	if err := svc.RecoverMutationIntents(); err != nil {
		t.Fatalf("显式恢复 intent：%v", err)
	}
	if !blobs.Exists(hash) {
		t.Fatal("显式恢复后活动 blob 应存在")
	}
	if _, err := os.Stat(entry.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("显式恢复后隔离 blob 应移除：%v", err)
	}
	mutation, err := mutations.Get(operationID)
	if err != nil || mutation.Status != "rolled_back" {
		t.Fatalf("显式恢复后 intent 状态错误：mutation=%+v err=%v", mutation, err)
	}
}

// TestStandbyApplyOperationDoesNotRecoverLegacyMutation 确保备用节点首次接收
// operation 时，只应用当前复制批次，不能恢复或回收本地遗留 intent。
