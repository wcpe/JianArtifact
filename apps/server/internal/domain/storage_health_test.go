package domain_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// fakeBlobScanner 用固定哈希集合模拟磁盘上的活动 blob。
type fakeBlobScanner struct {
	hashes []string
}

func (f fakeBlobScanner) WalkActive(fn func(string) error) error {
	for _, hash := range f.hashes {
		if err := fn(hash); err != nil {
			return err
		}
	}
	return nil
}

// TestStorageHealthReportsOnlyCollectableGarbage 钉住存储体检的判定口径：
// 只报「确定可回收」的类别，且**绝不清理**任何东西。
//
// 为什么只读是硬要求：仓库删除与 blob 回收不可逆，把一次误判（比如把在用的 blob
// 当成孤儿）变成自动删除，就是数据丢失。体检只负责让人看清现状。
func TestStorageHealthReportsOnlyCollectableGarbage(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	blobRoot := filepath.Join(dataDir, "blobs")

	// 两个 blob：一个被 asset 引用（在用），一个没有引用（孤儿）。
	usedHash := "aa11" + "bbccddeeff00112233445566778899aabbccddeeff0011223344556677889900"
	orphanHash := "ff22" + "bbccddeeff00112233445566778899aabbccddeeff0011223344556677889900"
	for _, hash := range []string{usedHash, orphanHash} {
		dir := filepath.Join(blobRoot, hash[:2])
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("建分片目录：%v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, hash[2:]), []byte("payload"), 0o644); err != nil {
			t.Fatalf("写 blob：%v", err)
		}
	}
	// 一个空的分片目录（清理后残留）。
	if err := os.MkdirAll(filepath.Join(blobRoot, "cc"), 0o755); err != nil {
		t.Fatalf("建空目录：%v", err)
	}
	// 隔离区一个文件。
	qDir := filepath.Join(blobRoot, "quarantine", "op-1")
	if err := os.MkdirAll(qDir, 0o755); err != nil {
		t.Fatalf("建隔离区：%v", err)
	}
	if err := os.WriteFile(filepath.Join(qDir, "deadbeef"), []byte("xx"), 0o644); err != nil {
		t.Fatalf("写隔离区文件：%v", err)
	}
	// pre-restore 两个目录。
	for _, name := range []string{"pre-restore-20260101-000000", "pre-restore-20260102-000000"} {
		dir := filepath.Join(dataDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("建 %s：%v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "jianartifact.db"), []byte("db"), 0o644); err != nil {
			t.Fatalf("写 %s 内容：%v", name, err)
		}
	}

	used := map[string]struct{}{usedHash: {}}
	svc := domain.NewStorageHealthService(dataDir, fakeBlobScanner{hashes: []string{usedHash, orphanHash}},
		func() (map[string]struct{}, error) { return used, nil })

	health, err := svc.Snapshot()
	if err != nil {
		t.Fatalf("体检：%v", err)
	}

	if health.OrphanBlobs != 1 {
		t.Fatalf("孤儿 blob 数 = %d，期望 1（只有未被引用那个）", health.OrphanBlobs)
	}
	if health.OrphanBlobBytes != int64(len("payload")) {
		t.Fatalf("孤儿字节 = %d，期望 %d", health.OrphanBlobBytes, len("payload"))
	}
	if health.QuarantinedBlobs != 1 {
		t.Fatalf("隔离区文件数 = %d，期望 1", health.QuarantinedBlobs)
	}
	if health.EmptyDirs != 1 {
		t.Fatalf("空目录数 = %d，期望 1（cc）", health.EmptyDirs)
	}
	if health.PreRestoreDirs != 2 {
		t.Fatalf("pre-restore 目录数 = %d，期望 2", health.PreRestoreDirs)
	}
	if health.PreRestoreBytes != 4 {
		t.Fatalf("pre-restore 占用 = %d，期望 4", health.PreRestoreBytes)
	}

	// **最关键的不变量**：体检不得删除任何东西。
	for _, hash := range []string{usedHash, orphanHash} {
		path := filepath.Join(blobRoot, hash[:2], hash[2:])
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("体检不得删除 blob（%s 已不存在）：%v", hash, statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(qDir, "deadbeef")); statErr != nil {
		t.Fatalf("体检不得清理隔离区：%v", statErr)
	}
	for _, name := range []string{"pre-restore-20260101-000000", "pre-restore-20260102-000000"} {
		if _, statErr := os.Stat(filepath.Join(dataDir, name)); statErr != nil {
			t.Fatalf("体检不得删除 %s：%v", name, statErr)
		}
	}
}
