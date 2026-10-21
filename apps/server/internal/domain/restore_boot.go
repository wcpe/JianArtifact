package domain

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// preRestoreDirPrefix 是覆盖前的自动备份目录前缀；pre-restore-<UTC时间戳>/ 作为回滚退路。
const preRestoreDirPrefix = "pre-restore-"

// preRestoreLabel 是"恢复后补回登记"的备注文案。
// 写成常量而不是内联字面量：它会落库并被用户看到，需要一处可改。
const preRestoreLabel = "恢复后补回"

// RestoreOutcome 是启动期应用一次待生效恢复的结果，供启动日志打印。
type RestoreOutcome struct {
	PackageID     string
	AssetCount    int
	BlobMerged    int
	BlobSkipped   int
	PreRestoreDir string // 覆盖前自动备份目录；无既有 db 时为空
	// ReRegistered 是恢复后从磁盘包体补回的备份登记数。
	// 非零说明"这份旧快照里没有这些包的登记，但包体还在磁盘上"——不补回来它们就看不见了。
	ReRegistered int
	// ImportsRestored 是从回滚备份里搬回的导入记录数。
	// 非零说明"恢复之前有过导入事件，而新快照里没有它们"——不搬回来就查不到历史了。
	ImportsRestored int
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
	s := fmt.Sprintf("包 %s：制品 %d 件，blob 合并 %d / 跳过 %d，回滚备份 %s",
		o.PackageID, o.AssetCount, o.BlobMerged, o.BlobSkipped, backup)
	if o.ReRegistered > 0 {
		s += fmt.Sprintf("，补回备份登记 %d 个", o.ReRegistered)
	}
	if o.ImportsRestored > 0 {
		s += fmt.Sprintf("，搬回导入记录 %d 条", o.ImportsRestored)
	}
	return s
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

	// 4. 把"被覆盖掉的那些备份登记"补回来。
	//
	// 这一步是本函数最容易被忽略、却最致命的地方：backup_package 表就存在被替换的
	// 那个数据库里，而备份包体（<id>.tar.gz）在数据目录下、**不在**快照内。恢复一份
	// 旧快照会让登记表退回旧内容，于是恢复之后"磁盘上明明有包、列表里却看不见"——
	// 用户看到的就是"备份包全丢了"。而 restore 恰恰是最需要备份兜底的时刻。
	//
	// 修复口径：以**磁盘为准**重建登记。凡包体还在、但新库里没有登记的，一律补登记
	// （状态 done、来源沿用包内 manifest）。这样恢复不会再把备份列表抹掉；
	// 反过来，包体已不存在的旧登记不补（它们本来就没法用）。
	restored := restoreBackupRegistrations(dbPath, dataDir)

	// 4b. 把导入记录从回滚备份里搬回来。
	//
	// backup_import 同样是"库内的表"，恢复后会退回旧快照的内容。后果是：
	// 恢复**之后**用户再也看不到"这次恢复是怎么失败的"——失败记录被它自己的恢复
	// 覆盖掉了。实测就是这样丢掉过一次失败原因，导致无法定位。
	//
	// 与备份登记不同，导入记录是**纯历史**，磁盘上没有对应物可依据，因此只能从
	// pre-restore 那份完整快照里搬运：它正是"被覆盖前的数据库"。
	importsRestored := restoreImportHistory(dbPath, preRestoreDir)

	// 5. 删除标记与暂存目录。
	if err := os.Remove(pendingPath(dataDir)); err != nil {
		return false, RestoreOutcome{}, fmt.Errorf("移除待生效标记：%w", err)
	}
	_ = os.RemoveAll(stagingDir(dataDir))

	return true, RestoreOutcome{
		PackageID:       pending.PackageID,
		AssetCount:      pending.Counts.Assets,
		BlobMerged:      pending.BlobMerged,
		BlobSkipped:     pending.BlobSkipped,
		PreRestoreDir:   preRestoreDir,
		ReRegistered:    restored,
		ImportsRestored: importsRestored,
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

// restoreBackupRegistrations 在数据库被恢复后，把"包体还在磁盘上、但新库里没有登记"
// 的备份包重新登记回去，返回补回的条数。
//
// # 为什么需要
//
// backup_package 登记表与被恢复的数据库是同一个文件，而备份包体在数据目录下、
// 不在快照里。恢复一份旧快照会让登记表退回旧内容，于是"磁盘上有包、列表里没有"。
// 用户看到的就是"恢复一次，备份包全没了"——偏偏 restore 正是最需要备份兜底的时刻。
//
// # 口径：以磁盘为准
//
// 只补"包体存在且能通过 manifest 校验"的包。旧快照里登记过、但包体已不在的那些不补
// （它们本来也没法用）。已在登记里的跳过，因此本函数幂等。
//
// # 为什么不用 BackupService
//
// 本函数在 persistence.Open **之前**执行（替换数据库时不能有活动连接）。
// 因此这里自建一条临时连接，用完即关；不复用服务层的仓储，避免在启动早期引入
// 完整的服务装配。
//
// 任何一步失败都只返回 0 而不是让启动失败：恢复本身已经成功，补登记是尽力而为的
// 补救动作，不该反过来把服务打死。
func restoreBackupRegistrations(dbPath, dataDir string) int {
	dir := filepath.Join(dataDir, "backups")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	// 先收集磁盘上可用的包标识。
	type diskPackage struct {
		id string
	}
	var onDisk []diskPackage
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".snapshot") {
			continue
		}
		id := strings.TrimSuffix(name, ".tar.gz")
		if id == "" {
			continue
		}
		onDisk = append(onDisk, diskPackage{id: id})
	}
	if len(onDisk) == 0 {
		return 0
	}

	db, err := persistence.Open(dbPath)
	if err != nil {
		return 0
	}
	defer func() { _ = db.Close() }()

	// 既有登记集合。
	registered := make(map[string]struct{}, len(onDisk))
	rows, err := db.Query(`SELECT package_id FROM backup_package`)
	if err != nil {
		return 0
	}
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr == nil {
			registered[id] = struct{}{}
		}
	}
	_ = rows.Close()

	restored := 0
	for _, p := range onDisk {
		if _, ok := registered[p.id]; ok {
			continue
		}
		// 直接读归档 manifest：这是包的权威身份，也是"能否使用"的最低门槛。
		// 不用 BackupService：它需要完整仓储装配，而本函数跑在服务装配之前。
		_, m, openErr := archive.Open(filepath.Join(dir, p.id+".tar.gz"))
		if openErr != nil {
			continue
		}
		if validateErr := m.Validate(); validateErr != nil {
			continue
		}
		size := packageSize(filepath.Join(dir, p.id+".tar.gz"))
		finished := m.CreatedAt.UTC().Format(time.RFC3339Nano)
		if _, execErr := db.Exec(`INSERT OR IGNORE INTO backup_package
			(package_id, mode, base_package_id, status, label, size_bytes, counts_json,
			 node_id, app_version, db_schema_version, created_at, finished_at, error_summary, origin)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.PackageID, string(m.Mode), m.BasePackageID, "done", preRestoreLabel, size, "{}",
			m.NodeID, m.AppVersion, m.DBSchemaVersion, finished, finished, "",
			"manual"); execErr == nil {
			restored++
			registered[m.PackageID] = struct{}{}
		}
	}
	return restored
}

// restoreImportHistory 把 pre-restore 快照里的导入记录搬回当前库，返回搬回的条数。
//
// # 为什么需要
//
// backup_import 是被恢复的那个数据库里的一张表，因此恢复会把它一并回滚。
// 于是"这次恢复失败了"的记录会被**它自己的恢复动作**覆盖掉——记录消失、
// 界面查不到、日志里也没有，现场完全无法定位。这真实发生过一次。
//
// # 数据来源
//
// 导入记录是纯历史，磁盘上没有对应物，所以只能从 pre-restore 快照里读——
// 那是"被覆盖前的数据库"的完整副本。若本次没有生成 pre-restore（全新实例首次恢复），
// 就没有可搬的历史，直接返回 0。
//
// # 覆盖策略
//
// 用 INSERT OR REPLACE：新旧两库可能有同 id 的记录（例如同一批历史），
// 以**旧库为准**——旧库是"恢复前真实发生的最后一次状态"，比新快照里的更接近事实。
// 任何一步失败都只返回 0，不让启动失败：恢复本身已经成功，搬历史是尽力而为。
func restoreImportHistory(dbPath, preRestoreDir string) int {
	if preRestoreDir == "" {
		return 0
	}
	oldDBPath := filepath.Join(preRestoreDir, filepath.Base(dbPath))
	if _, err := os.Stat(oldDBPath); err != nil {
		return 0
	}

	oldDB, err := persistence.Open(oldDBPath)
	if err != nil {
		return 0
	}
	defer func() { _ = oldDB.Close() }()

	// 先确认旧库确实有这张表（老版本可能没有），避免对不存在的表查询报错。
	var tableCount int
	if err := oldDB.Get(&tableCount,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='backup_import'`); err != nil {
		return 0
	}
	if tableCount == 0 {
		return 0
	}

	rows, err := oldDB.Query(`SELECT import_id, origin, status, source_url, operator,
		overwrite, deep, total_bytes, fetched_bytes, blob_count, package_id,
		error_code, error_summary, created_at, updated_at, finished_at, restore_pending_at
		FROM backup_import`)
	if err != nil {
		return 0
	}
	type importRow struct {
		ImportID         string
		Origin           string
		Status           string
		SourceURL        string
		Operator         string
		Overwrite        bool
		Deep             bool
		TotalBytes       int64
		FetchedBytes     int64
		BlobCount        int
		PackageID        string
		ErrorCode        string
		ErrorSummary     string
		CreatedAt        string
		UpdatedAt        string
		FinishedAt       *string
		RestorePendingAt *string
	}
	var collected []importRow
	for rows.Next() {
		var r importRow
		if scanErr := rows.Scan(&r.ImportID, &r.Origin, &r.Status, &r.SourceURL, &r.Operator,
			&r.Overwrite, &r.Deep, &r.TotalBytes, &r.FetchedBytes, &r.BlobCount, &r.PackageID,
			&r.ErrorCode, &r.ErrorSummary, &r.CreatedAt, &r.UpdatedAt, &r.FinishedAt,
			&r.RestorePendingAt); scanErr == nil {
			collected = append(collected, r)
		}
	}
	_ = rows.Close()
	if len(collected) == 0 {
		return 0
	}

	newDB, err := persistence.Open(dbPath)
	if err != nil {
		return 0
	}
	defer func() { _ = newDB.Close() }()

	restored := 0
	for _, r := range collected {
		if _, execErr := newDB.Exec(`INSERT OR REPLACE INTO backup_import
			(import_id, origin, status, source_url, operator, overwrite, deep,
			 total_bytes, fetched_bytes, blob_count, package_id, error_code, error_summary,
			 created_at, updated_at, finished_at, restore_pending_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ImportID, r.Origin, r.Status, r.SourceURL, r.Operator, r.Overwrite, r.Deep,
			r.TotalBytes, r.FetchedBytes, r.BlobCount, r.PackageID, r.ErrorCode, r.ErrorSummary,
			r.CreatedAt, r.UpdatedAt, r.FinishedAt, r.RestorePendingAt); execErr == nil {
			restored++
		}
	}
	return restored
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
