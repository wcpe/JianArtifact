package domain

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// preRestoreDirPrefix 是覆盖前的自动备份目录前缀；pre-restore-<UTC时间戳>/ 作为回滚退路。
const preRestoreDirPrefix = "pre-restore-"

// RestoreOutcome 是启动期应用一次待生效恢复的结果，供启动日志打印。
type RestoreOutcome struct {
	PackageID     string
	AssetCount    int
	BlobMerged    int
	BlobSkipped   int
	PreRestoreDir string // 覆盖前自动备份目录；无既有 db 时为空
}

// Summary 返回可读的一行摘要（含包标识、制品数、blob 合并/跳过数、pre-restore 备份目录）。
func (o RestoreOutcome) Summary() string {
	backup := o.PreRestoreDir
	if backup == "" {
		backup = "（无既有数据库，未生成回滚备份）"
	}
	return fmt.Sprintf("包 %s：制品 %d 件，blob 合并 %d / 跳过 %d，回滚备份 %s",
		o.PackageID, o.AssetCount, o.BlobMerged, o.BlobSkipped, backup)
}

// ApplyPendingRestore 在 persistence.Open 之前调用：若存在待生效标记，则先备份当前数据库，
// 再原子替换为暂存数据库，最后清理标记与暂存目录。返回 applied 标识是否执行了替换。
//
// 关键约束：本函数必须在打开数据库连接之前运行——替换 db 文件时不能有任何打开的连接，
// 否则 SQLite 会持有文件锁导致替换失败或静默损坏。
//
// blob 在 Stage 阶段已按内容寻址合并进目标 blobstore（可加、幂等），启动阶段不需要再处理
// blob；本函数只负责数据库文件的原子替换与回滚备份。
func ApplyPendingRestore(dataDir, dbPath, blobDir string) (applied bool, outcome RestoreOutcome, err error) {
	pending, err := readPendingFile(dataDir)
	if err != nil {
		return false, RestoreOutcome{}, fmt.Errorf("读取待生效标记：%w", err)
	}
	// readPendingFile 已把"标记文件不存在"归一化为 (nil, nil)，
	// 因此这里 nil 即"本节点没有待生效恢复"——必须在解引用前判断。
	if pending == nil {
		return false, RestoreOutcome{}, nil
	}

	stagingDB := filepath.Join(dataDir, pending.StagedDBRelativePath)
	if _, statErr := os.Stat(stagingDB); statErr != nil {
		// 暂存数据库缺失：标记与暂存目录一并清理，视为无待生效恢复，避免阻塞启动。
		_ = os.Remove(pendingPath(dataDir))
		_ = os.RemoveAll(stagingDir(dataDir))
		return false, RestoreOutcome{}, nil
	}

	// 1. 覆盖前自动备份当前数据库（回滚退路，必须保留）。
	ts := time.Now().UTC().Format("20060102-150405")
	preDir := filepath.Join(dataDir, preRestoreDirPrefix+ts)
	var preRestoreDir string
	if _, statErr := os.Stat(dbPath); statErr == nil {
		if err := os.MkdirAll(preDir, 0o750); err != nil {
			return false, RestoreOutcome{}, fmt.Errorf("创建回滚备份目录：%w", err)
		}
		if err := copyRegularFile(dbPath, filepath.Join(preDir, filepath.Base(dbPath))); err != nil {
			return false, RestoreOutcome{}, fmt.Errorf("备份当前数据库：%w", err)
		}
		preRestoreDir = preDir
	} else if !os.IsNotExist(statErr) {
		return false, RestoreOutcome{}, fmt.Errorf("检查当前数据库：%w", statErr)
	}

	// 2. 把暂存 db 复制到 <dbPath>.restore-tmp，再原子 rename 覆盖（同文件系统原子替换）。
	tmpDB := dbPath + ".restore-tmp"
	if err := copyRegularFile(stagingDB, tmpDB); err != nil {
		if preRestoreDir != "" {
			_ = os.RemoveAll(preDir)
		}
		return false, RestoreOutcome{}, fmt.Errorf("复制暂存数据库：%w", err)
	}
	if err := os.Rename(tmpDB, dbPath); err != nil {
		_ = os.Remove(tmpDB)
		if preRestoreDir != "" {
			_ = os.RemoveAll(preDir)
		}
		return false, RestoreOutcome{}, fmt.Errorf("替换数据库：%w", err)
	}

	// 3. 删除可能残留的 WAL / SHM（恢复的是已 checkpoint 的快照，不应带旧日志）。
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")

	// 4. 删除标记与暂存目录。
	if err := os.Remove(pendingPath(dataDir)); err != nil {
		return false, RestoreOutcome{}, fmt.Errorf("移除待生效标记：%w", err)
	}
	_ = os.RemoveAll(stagingDir(dataDir))

	return true, RestoreOutcome{
		PackageID:     pending.PackageID,
		AssetCount:    pending.Counts.Assets,
		BlobMerged:    pending.BlobMerged,
		BlobSkipped:   pending.BlobSkipped,
		PreRestoreDir: preRestoreDir,
	}, nil
}

// copyRegularFile 把 src 完整复制为 dst（不保留原 mtime，仅内容一致）。
func copyRegularFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开源文件 %s：%w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("创建目标文件 %s：%w", dst, err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("关闭目标文件 %s：%w", dst, cerr)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return fmt.Errorf("复制文件 %s -> %s：%w", src, dst, err)
	}
	return nil
}
