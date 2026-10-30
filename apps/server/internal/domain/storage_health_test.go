package domain_test

import (
	"errors"
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

// failingBlobScanner 模拟「磁盘扫描本身失败」：用于验证单项失败不整份失败。
type failingBlobScanner struct{}

func (failingBlobScanner) WalkActive(func(string) error) error {
	return errors.New("模拟扫描失败")
}

// TestStorageHealthPartialFailureStillReturnsOtherItems 钉住「单项统计失败不整份失败」。
//
// 为什么这条重要：体检是运维判断「要不要清理」的唯一依据。某个目录因为权限、挂载或
// IO 错误读不到时，如果整份报告变成 500，运维得到的信息量是零——连那些**读得到**的
// 可回收数据都看不到。所以正确行为是「能算的都算，把失败原因附在 partialError 里」。
//
// 变异验证：把 Snapshot 里 keep(err) 之后改成「err != nil 就 return」，本用例必须转红。
func TestStorageHealthPartialFailureStillReturnsOtherItems(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	// 造一个「读得到」的可回收项：空的分片目录。
	if err := os.MkdirAll(filepath.Join(dataDir, "blobs", "cc"), 0o755); err != nil {
		t.Fatalf("建空目录：%v", err)
	}

	// 引用集合读取失败 + blob 扫描失败，两者都是「单项失败」。
	svc := domain.NewStorageHealthService(
		dataDir,
		failingBlobScanner{},
		func() (map[string]struct{}, error) { return nil, errors.New("模拟引用集合读取失败") },
	)

	health, err := svc.Snapshot()
	if err == nil {
		t.Fatal("应如实返回失败原因（供 handler 写 partialError），实际 nil")
	}
	// 关键：其余项仍应被统计到——空目录数必须还是 1，不能因为别的项失败就归零。
	if health.EmptyDirs != 1 {
		t.Fatalf("读得到的项仍应统计：EmptyDirs = %d，期望 1", health.EmptyDirs)
	}
	// 失败的项留空而不是给出错值（不给运维假数据）。
	if health.OrphanBlobs != 0 || health.OrphanBlobBytes != 0 {
		t.Fatalf("扫描失败的项应留空，实际 OrphanBlobs=%d OrphanBlobBytes=%d",
			health.OrphanBlobs, health.OrphanBlobBytes)
	}
}
