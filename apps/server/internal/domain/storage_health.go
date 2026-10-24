package domain

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// StorageHealth 是「存储体检」的一次只读快照。
//
// 它只回答「现在有哪些可回收的东西、各占多少」，**不做任何清理**：仓库删除、blob 回收
// 都是不可逆的运维决策，自动清理会把一次误判变成数据丢失。清理动作仍由既有作业
// （blob-gc / storage-cleanup / backup-orphan-cleanup）在明确间隔下执行。
type StorageHealth struct {
	// OrphanBlobs 是磁盘上存在、但没有任何 asset 引用的 blob 文件数。
	// 它们只由 blob-gc 回收；持续为正通常意味着写入中途失败或旁路写入了 blob。
	OrphanBlobs int `json:"orphanBlobs"`
	// OrphanBlobBytes 是上述孤儿 blob 的字节数合计。
	OrphanBlobBytes int64 `json:"orphanBlobBytes"`
	// QuarantinedBlobs / QuarantinedBytes 是隔离区内待回收的 blob（删除或覆盖后的中转态）。
	QuarantinedBlobs int   `json:"quarantinedBlobs"`
	QuarantinedBytes int64 `json:"quarantinedBytes"`
	// EmptyDirs 是活动 blob 目录下没有任何文件的目录数（哈希分片目录被清空后残留）。
	EmptyDirs int `json:"emptyDirs"`
	// PreRestoreDirs / PreRestoreBytes 是恢复前回滚备份目录的数量与占用，
	// 由 backup-orphan-cleanup 按「保留最近 N 个」回收。
	PreRestoreDirs  int   `json:"preRestoreDirs"`
	PreRestoreBytes int64 `json:"preRestoreBytes"`
	// BackupUploadsBytes 是分片上传暂存占用（中断或放弃的上传会留在那里）。
	BackupUploadsBytes int64 `json:"backupUploadsBytes"`
	// BackupImportsBytes 是导入暂存占用。
	BackupImportsBytes int64 `json:"backupImportsBytes"`
	// BackupDrillBytes 是恢复演练的临时目录占用。
	BackupDrillBytes int64 `json:"backupDrillBytes"`
	// TotalBytes 是数据目录总占用，用于判断上面各项是否值得清理。
	TotalBytes int64 `json:"totalBytes"`
}

// dataDirScanner 抽象「列出磁盘上实际存在的 blob 哈希」，便于测试注入而不碰真实目录。
type dataDirScanner interface {
	WalkActive(fn func(hash string) error) error
}

// StorageHealthService 汇总存储体检快照。
//
// 判定口径刻意保守：只报「确定可回收」的类别（孤儿 blob、隔离区、空目录、暂存目录），
// 不猜「看起来没用」的文件——把业务数据误报成垃圾，比漏报危险得多。
type StorageHealthService struct {
	dataDir string
	blobs   dataDirScanner
	// referenced 返回当前被 asset 引用的全部 blob 哈希；为 nil 时跳过孤儿判定。
	referenced func() (map[string]struct{}, error)
}

// NewStorageHealthService 构造体检服务。blobs 可为 nil（仅统计目录占用）。
func NewStorageHealthService(dataDir string, blobs dataDirScanner, referenced func() (map[string]struct{}, error)) *StorageHealthService {
	return &StorageHealthService{dataDir: dataDir, blobs: blobs, referenced: referenced}
}

// Snapshot 生成一次快照。单项统计失败不阻断其余项：体检的价值在于「尽量看清全貌」，
// 一个目录读不了不应让整份报告变成不可用（但会把该项留空并在 Err 中说明）。
func (s *StorageHealthService) Snapshot() (StorageHealth, error) {
	var health StorageHealth
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	blobRoot := filepath.Join(s.dataDir, "blobs")
	// 孤儿 blob：磁盘有、asset 没有。
	if s.blobs != nil && s.referenced != nil {
		used, err := s.referenced()
		keep(err)
		if err == nil {
			keep(s.blobs.WalkActive(func(hash string) error {
				if _, ok := used[hash]; ok {
					return nil
				}
				health.OrphanBlobs++
				if info, statErr := os.Stat(s.blobPath(blobRoot, hash)); statErr == nil {
					health.OrphanBlobBytes += info.Size()
				}
				return nil
			}))
		}
	}

	// 隔离区：删除/覆盖后的中转态，由 storage-cleanup 回收。
	qFiles, qBytes := dirUsage(filepath.Join(blobRoot, "quarantine"))
	health.QuarantinedBlobs = qFiles
	health.QuarantinedBytes = qBytes

	// 空目录：只统计活动 blob 目录下的分片目录（隔离区与临时目录不在此口径）。
	health.EmptyDirs = countEmptyDirs(blobRoot)

	health.PreRestoreDirs, health.PreRestoreBytes = preRestoreUsage(s.dataDir)
	_, health.BackupUploadsBytes = dirUsage(filepath.Join(s.dataDir, "backup-uploads"))
	_, health.BackupImportsBytes = dirUsage(filepath.Join(s.dataDir, "backup-imports"))
	_, health.BackupDrillBytes = dirUsage(filepath.Join(s.dataDir, "backup-drill"))
	_, health.TotalBytes = dirUsage(s.dataDir)

	return health, firstErr
}

// blobPath 把内容寻址哈希还原成落盘路径。
//
// 布局与 blobstore 一致：前两位作为分片目录，其余作为文件名（sha256 十六进制）。
// 这里只用于取大小，读不到就按 0 计，不影响孤儿计数。
func (s *StorageHealthService) blobPath(root, hash string) string {
	if len(hash) < 3 {
		return filepath.Join(root, hash)
	}
	return filepath.Join(root, hash[:2], hash[2:])
}

// dirUsage 返回目录下普通文件数与字节数合计（递归，符号链接不计，读不到按 0）。
func dirUsage(dir string) (int, int64) {
	var count int
	var size int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if info, statErr := d.Info(); statErr == nil {
			count++
			size += info.Size()
		}
		return nil
	})
	return count, size
}

// countEmptyDirs 统计 root 下（跳过隔离区与临时目录）不含任何文件的目录数。
//
// 只数到叶子：上一级目录即使「下面只有空目录」也不算——它自身仍有子项，
// 一并报出来只会让条数虚高，运维无法用它判断该删哪些。
func countEmptyDirs(root string) int {
	var empty int
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "quarantine" || name == "tmp") {
				return filepath.SkipDir
			}
			return nil
		}
		return nil
	})
	// 上面只做遍历过滤；空目录判定用逐层读取（WalkDir 不便于回溯子项数）。
	_ = empty
	return emptyDirsRecursive(root)
}

// emptyDirsRecursive 统计不含文件的叶子目录数。
func emptyDirsRecursive(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var empty int
	for _, entry := range entries {
		if !entry.IsDir() {
			return 0 // 本目录含文件，且调用方只关心叶子
		}
		name := entry.Name()
		if name == "quarantine" || name == "tmp" {
			continue
		}
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "quarantine" || name == "tmp" {
			continue
		}
		sub := filepath.Join(dir, name)
		if hasFile(sub) {
			continue
		}
		if isLeafEmpty(sub) {
			empty++
			continue
		}
		empty += emptyDirsRecursive(sub)
	}
	return empty
}

// hasFile 判断目录子树内是否含任意普通文件。
func hasFile(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		found = true
		return filepath.SkipAll
	})
	return found
}

// isLeafEmpty 判断目录本身为空（没有任何子项）。
func isLeafEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

// preRestoreUsage 统计 pre-restore-* 目录的数量与总占用。
func preRestoreUsage(dataDir string) (int, int64) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return 0, 0
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "pre-restore-") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	var size int64
	for _, name := range names {
		_, bytes := dirUsage(filepath.Join(dataDir, name))
		size += bytes
	}
	return len(names), size
}
