package blobstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRemoveEmptyQuarantineDirsOnFreshStore 全新建的 blob 根目录下没有隔离区与临时目录，
// 清理必须是幂等的空操作（否则首次部署的运行就会因目录不存在而失败）。
func TestRemoveEmptyQuarantineDirsOnFreshStore(t *testing.T) {
	store := NewStore(t.TempDir())
	removed, err := store.RemoveEmptyQuarantineDirs(time.Now())
	if err != nil {
		t.Fatalf("清理全新 blob 根的隔离区：%v", err)
	}
	if removed != 0 {
		t.Fatalf("全新 blob 根不应删除任何目录，实际 %d", removed)
	}
	tempsRemoved, tempsSkipped, err := store.CleanupOCIUploadTempsBefore(time.Now())
	if err != nil {
		t.Fatalf("清理全新 blob 根的上传暂存目录：%v", err)
	}
	if tempsRemoved != 0 || tempsSkipped != 0 {
		t.Fatalf("全新 blob 根不应删除任何临时文件，实际 removed=%d skipped=%d", tempsRemoved, tempsSkipped)
	}
}

// TestRemoveEmptyQuarantineDirsSkipsNonEmptyAndIgnoresNonDirEntries 只删空目录：
// 非空目录与普通文件都必须保留。
func TestRemoveEmptyQuarantineDirsSkipsNonEmptyAndIgnoresNonDirEntries(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	quarantineRoot := filepath.Join(root, quarantineDirName)
	empty := filepath.Join(quarantineRoot, "op-empty")
	occupied := filepath.Join(quarantineRoot, "op-occupied")
	for _, dir := range []string{empty, occupied} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("创建隔离目录：%v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(occupied, "rollback.tar"), []byte("snapshot"), 0o600); err != nil {
		t.Fatalf("写入回滚快照：%v", err)
	}
	stray := filepath.Join(quarantineRoot, "stray-file")
	if err := os.WriteFile(stray, []byte("stray"), 0o600); err != nil {
		t.Fatalf("写入游离文件：%v", err)
	}

	removed, err := store.RemoveEmptyQuarantineDirs(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("清理隔离区空目录：%v", err)
	}
	if removed != 1 {
		t.Fatalf("应只删除 1 个空目录，实际 %d", removed)
	}
	if _, err := os.Stat(empty); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("空隔离目录应被删除：%v", err)
	}
	for _, path := range []string{occupied, filepath.Join(occupied, "rollback.tar"), stray} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("不得删除 %s：%v", path, err)
		}
	}
}

// TestCleanupOCIUploadTempsBeforeOnlyTouchesThatDirectory 清理只作用于 tmp/oci-upload：
// 同级的普通 blob 中间文件与活动 blob 都不受影响。
func TestCleanupOCIUploadTempsBeforeOnlyTouchesThatDirectory(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	uploadDir := filepath.Join(root, tmpDirName, ociUploadTmpDirName)
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatalf("创建上传暂存目录：%v", err)
	}
	old := time.Now().Add(-72 * time.Hour)
	expired := filepath.Join(uploadDir, "upload-1")
	fresh := filepath.Join(uploadDir, "upload-2")
	blobIntermediate := filepath.Join(root, tmpDirName, "blob-1")
	restoreIntermediate := filepath.Join(root, tmpDirName, "restore-1")
	for path, modTime := range map[string]time.Time{
		expired:             old,
		fresh:               time.Now(),
		blobIntermediate:    old,
		restoreIntermediate: old,
	} {
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatalf("写入 %s：%v", path, err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("回填时间 %s：%v", path, err)
		}
	}
	hash := strings.Repeat("ab", 32)
	active := store.pathFor(hash)
	if err := os.MkdirAll(filepath.Dir(active), 0o750); err != nil {
		t.Fatalf("创建活动分片目录：%v", err)
	}
	if err := os.WriteFile(active, []byte("active"), 0o600); err != nil {
		t.Fatalf("写入活动 blob：%v", err)
	}

	removed, skipped, err := store.CleanupOCIUploadTempsBefore(time.Now().Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("清理过期上传临时文件：%v", err)
	}
	if removed != 1 || skipped != 0 {
		t.Fatalf("应删除 1 个过期上传临时文件且无跳过，实际 removed=%d skipped=%d", removed, skipped)
	}
	if _, err := os.Stat(expired); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("过期上传临时文件应被删除：%v", err)
	}
	for _, path := range []string{fresh, blobIntermediate, restoreIntermediate, active} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("不得删除 %s：%v", path, err)
		}
	}
}
