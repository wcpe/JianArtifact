package domain

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
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
	// SkippedReason 非空表示"有待生效标记但本次未应用"，值是给运维看的完整原因与处置建议。
	// 出现它时 applied 为 false，且**不阻止启动**（见 ApplyPendingRestore 的说明）。
	SkippedReason string
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
//
// **标记读不了时不让服务起不来**（实测踩到的坑）：早期实现把 readPendingFile 的任何错误
// 都当致命错误返回，于是"标记文件属主/权限不对"会把服务彻底打死——启动失败 → systemd 重启 →
// 同一文件仍读不了 → 再失败，形成重启循环，现场只看到全站 502，看不到真正该做的事（改属主）。
// 真实的触发场景：运维用 sudo 跑 CLI 导入，CLI 以 root 写了 0600 root:root 的 restore.pending，
// 而服务以普通用户运行，读不了它。
//
// 取舍是有意的：待生效恢复是"稍后要应用的一次维护动作"，不是服务提供读服务的前提。
// 用可用性去换一个可延后动作，代价完全不成比例。因此这里改为**跳过本次恢复并交回原因**，
// 由调用方打印告警；标记与暂存目录**原样保留**（数据不丢），管理员修正属主后重启即可完成替换。
func ApplyPendingRestore(dataDir, dbPath, blobDir string) (applied bool, outcome RestoreOutcome, err error) {
	pending, err := readPendingFile(dataDir)
	if err != nil {
		return classifyMarkerReadError(dataDir, err)
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

// classifyMarkerReadError 决定"标记读取失败"该当致命错误还是当可跳过的环境问题。
//
// 抽成独立纯函数是为了**可被直接测试**：走真实文件路径测这一分支需要构造一个
// "内容合法但权限不可读"的文件，而 root（以及 Windows 的管理员）能无视权限位读取任何文件，
// 于是那条用例在开发机上只能 skip、在 CI 上才真正跑到——一个会在常见环境下静默跳过的用例，
// 等于没有守卫。把判定抽出来，就能对两个分支都给断言。
func classifyMarkerReadError(dataDir string, readErr error) (bool, RestoreOutcome, error) {
	if isUnreadableMarker(readErr) {
		// 环境问题（权限/属主），不是数据问题：跳过恢复、如实交回原因。
		return false, RestoreOutcome{SkippedReason: unreadableMarkerHint(dataDir, readErr)}, nil
	}
	return false, RestoreOutcome{}, fmt.Errorf("读取待生效标记：%w", readErr)
}

// isUnreadableMarker 判断标记读取失败是否属于"文件在、但读不了"这类**环境问题**。
//
// 只认权限类错误，不认"文件不存在"（那已在 readPendingFile 里归一化为正常状态）、
// 也不认"内容损坏"（那是数据问题，应当显式报错让人知道包坏了）。
func isUnreadableMarker(err error) bool {
	return errors.Is(err, fs.ErrPermission)
}

// unreadableMarkerHint 把"标记读不了"翻译成一条**可照做**的告警：
// 说清哪个文件、当前属主与权限、服务以什么身份运行、以及具体该敲哪条命令。
//
// 为什么要这么啰嗦：这个失败以前会让服务起不来，运维在现场只看到 502 与服务反复重启，
// 完全看不出"去改一个文件的属主"才是解法。告警必须把处置动作直接写出来。
//
// 刻意**不**读 stat 的 uid/gid：那需要 syscall.Stat_t，各平台字段不一致（Windows 没有），
// 会让本包失去跨平台可编译性。而"哪个文件、什么权限"已足够定位问题。
func unreadableMarkerHint(dataDir string, err error) string {
	path := pendingPath(dataDir)
	detail := ""
	if info, statErr := os.Stat(path); statErr == nil {
		detail = fmt.Sprintf("（权限 %s）", info.Mode().Perm())
	}
	current := "未知"
	if u, lookErr := user.Current(); lookErr == nil {
		current = fmt.Sprintf("%s(uid=%s)", u.Username, u.Uid)
	}
	return fmt.Sprintf(
		"检测到待生效的恢复标记 %s，但当前进程读不了它%s：%v。\n"+
			"    本次**跳过**该恢复并继续启动（数据未被改动，标记与暂存目录原样保留）。\n"+
			"    常见原因：曾用 sudo / root 运行过 `jianartifact backup import`，标记文件属主成了 root。\n"+
			"    当前进程身份：%s。修复：把属主改回服务运行身份，再重启即可完成替换——\n"+
			"        sudo chown -R <服务用户>:<服务组> %s %s\n"+
			"        sudo systemctl restart jianartifact",
		path, detail, err, current, path, stagingDir(dataDir))
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
