package domain

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func newLifecycleAssetService(t *testing.T) (*AssetService, *repository.AssetRepo, *blobstore.Store, *AssetMutationCoordinator, int64) {
	t.Helper()
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(filepath.Join(t.TempDir(), "blobs"))
	repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("创建仓库：%v", err)
	}
	mutator, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建资产协调器：%v", err)
	}
	service := NewAssetService(repos, assets, blobs, nil)
	service.SetMutationCoordinator(mutator)
	return service, assets, blobs, mutator, repoID
}

func TestAssetServiceConcurrentPutSamePathKeepsOneReferencedBlob(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payloads [][]byte
	}{
		{name: "同哈希", payloads: [][]byte{[]byte("same"), []byte("same"), []byte("same")}},
		{name: "异哈希", payloads: [][]byte{[]byte("one"), []byte("two"), []byte("three")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, assets, blobs, _, repoID := newLifecycleAssetService(t)
			start := make(chan struct{})
			errs := make(chan error, len(tc.payloads))
			var wg sync.WaitGroup
			for _, payload := range tc.payloads {
				payload := payload
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := service.Put("raw", "same.bin", bytes.NewReader(payload), "application/octet-stream")
					errs <- err
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			// 信封路径是乐观并发：并发同路径发布只允许一个成功，其余必须报
			// 「资产已被其他操作修改」冲突（原子性语义，客户端重试即可）。
			successes := 0
			for err := range errs {
				if err == nil {
					successes++
					continue
				}
				if !strings.Contains(err.Error(), "资产已被其他操作修改") {
					t.Fatalf("并发发布只允许同路径冲突错误，实际：%v", err)
				}
			}
			if successes == 0 {
				t.Fatal("并发同路径发布至少应有一个成功")
			}
			asset, err := assets.GetByPath(repoID, "same.bin")
			if err != nil {
				t.Fatalf("读取最终资产：%v", err)
			}
			if !blobs.Exists(asset.BlobHash) {
				t.Fatal("最终资产引用的 blob 必须存在")
			}
			count := 0
			if err := blobs.WalkActive(func(string) error {
				count++
				return nil
			}); err != nil {
				t.Fatalf("枚举活动 blob：%v", err)
			}
			if count != 1 {
				t.Fatalf("同一路径并发覆盖后只能保留一个活动 blob，实际 %d", count)
			}
		})
	}
}

func TestAssetServicePublishKeepsBlobWhenGCInterleaves(t *testing.T) {
	service, assets, blobs, _, repoID := newLifecycleAssetService(t)
	payload := []byte("publish-gc-race")
	hash := stringHash(sha256.Sum256(payload))
	asset, err := service.StageBlob(bytes.NewReader(payload), "application/octet-stream")
	if err != nil {
		t.Fatalf("暂存 blob：%v", err)
	}
	asset.Path = "race.bin"
	removed, err := service.CleanupUnreferencedBlobs()
	if err != nil {
		t.Fatalf("并发 GC 失败：%v", err)
	}
	if removed != 0 {
		t.Fatalf("仍在发布生命周期的 blob 不得被 GC，实际清理 %d 个", removed)
	}
	if _, err := service.PublishAssets("raw", []*repository.Asset{asset}); err != nil {
		t.Fatalf("发布失败：%v", err)
	}
	if _, err := assets.GetByPath(repoID, "race.bin"); err != nil {
		t.Fatalf("发布元数据不存在：%v", err)
	}
	if !blobs.Exists(hash) {
		t.Fatal("发布成功的资产不得引用被并发 GC 删除的 blob")
	}
}

func TestAssetServiceWriteFailureDoesNotLeaveUnreferencedBlob(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, *AssetService, *repository.AssetRepo, int64)
		trigger string
		payload []byte
	}{
		{
			name:    "插入失败",
			prepare: func(_ *testing.T, _ *AssetService, _ *repository.AssetRepo, _ int64) {},
			trigger: `CREATE TRIGGER reject_asset_insert BEFORE INSERT ON asset BEGIN SELECT RAISE(ABORT, '注入资产插入失败'); END`,
			payload: []byte("insert-failure"),
		},
		{
			name: "更新失败",
			prepare: func(t *testing.T, service *AssetService, _ *repository.AssetRepo, _ int64) {
				if _, err := service.Put("raw", "same.bin", bytes.NewReader([]byte("old")), "application/octet-stream"); err != nil {
					t.Fatalf("预置旧资产：%v", err)
				}
			},
			trigger: `CREATE TRIGGER reject_asset_update BEFORE UPDATE ON asset BEGIN SELECT RAISE(ABORT, '注入资产更新失败'); END`,
			payload: []byte("update-failure"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, assets, blobs, _, repoID := newLifecycleAssetService(t)
			tc.prepare(t, service, assets, repoID)
			if _, err := assets.DB().Exec(tc.trigger); err != nil {
				t.Fatalf("创建失败注入：%v", err)
			}
			if _, err := service.Put("raw", "same.bin", bytes.NewReader(tc.payload), "application/octet-stream"); err == nil {
				t.Fatal("资产写入失败必须返回错误")
			}
			hash := sha256.Sum256(tc.payload)
			if blobs.Exists(stringHash(hash)) {
				t.Fatal("资产写入失败不得留下无引用 blob")
			}
		})
	}
}

func TestAssetServiceFailedCleanupReportsErrorAndCanRetry(t *testing.T) {
	service, _, blobs, mutator, _ := newLifecycleAssetService(t)
	cleanupErr := errors.New("注入物理清理失败")
	mutator.removeUnreferenced = func(string) error { return cleanupErr }
	if _, err := mutator.mutations.DB().Exec(`CREATE TRIGGER reject_asset_insert BEFORE INSERT ON asset BEGIN SELECT RAISE(ABORT, '注入资产提交失败'); END`); err != nil {
		t.Fatalf("创建失败注入：%v", err)
	}
	payload := []byte("retry-cleanup")
	if _, err := service.Put("raw", "retry.bin", bytes.NewReader(payload), "application/octet-stream"); !errors.Is(err, cleanupErr) {
		t.Fatalf("物理清理失败必须返回给调用方，实际：%v", err)
	}
	hash := sha256.Sum256(payload)
	if !blobs.Exists(stringHash(hash)) {
		t.Fatal("模拟清理失败后 blob 应保留，供恢复流程处理")
	}
	mutator.removeUnreferenced = blobs.Remove
	if _, err := service.CleanupUnreferencedBlobs(); err != nil {
		t.Fatalf("重试清理未引用 blob：%v", err)
	}
	if blobs.Exists(stringHash(hash)) {
		t.Fatal("重试清理后不得保留无引用 blob")
	}
}

func TestAssetServiceCommitHookFailureDoesNotLeaveUnreferencedBlob(t *testing.T) {
	service, _, blobs, _, _ := newLifecycleAssetService(t)
	payload := []byte("commit-hook-failure")
	hookErr := errors.New("注入提交回调失败")
	_, err := service.PutWithCommitHook("raw", "hook.bin", bytes.NewReader(payload), "application/octet-stream", func(*repository.Asset) repository.MutationCompletionHook {
		return func(*sqlx.Tx) error { return hookErr }
	})
	if !errors.Is(err, hookErr) {
		t.Fatalf("提交回调失败必须返回原始错误，实际：%v", err)
	}
	hash := sha256.Sum256(payload)
	if blobs.Exists(stringHash(hash)) {
		t.Fatal("提交回调失败不得留下无引用 blob")
	}
}

func stringHash(sum [sha256.Size]byte) string {
	return fmt.Sprintf("%x", sum)
}
