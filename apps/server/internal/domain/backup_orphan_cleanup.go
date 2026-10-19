package domain

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 备份孤儿回收（FR-40 追加八）。
//
// # 什么问题
//
// 备份包由两套持久化共同表示：`${JIAN_DATA_DIR}/backups/<id>.tar.gz`（文件，含同名
// `.index` 侧车）与 backup_package 登记行（SQLite）。BackupService.Delete 的删除顺序是
// **先删文件、再删登记**，因此正常路径不会留下"文件在、登记没了"的残留。
//
// 但两套持久化之间没有事务。以下都会打破这个不变量：
//   - 进程在 commit 前被杀、SQLite 文件被外部工具直接改、历史版本缺陷、运维手删记录；
//   - 把包体拷进来却没写登记（手工搬运、迁移脚本）。
//
// 一旦失步，这些包在界面上完全不可见——列表读登记表，于是磁盘被永久占用，用户既看不到
// 也不能删。这正是本作业要兜住的漏。
//
// # 为什么不是只按差集删
//
// 「未管理」不等于「该删」：用户可能刚把包拷进来准备导入。因此必须有两道护栏：
//
//  1. **宽限期**（grace）：只回收 mtime 早于 now-grace 的文件。刚落地的文件一律跳过，
//     给"导入"留出操作窗口。这与 storage-cleanup 的 emptyQuarantineDirGrace 同一思路。
//  2. **身份校验**：只回收能通过 archive.Manifest.Validate 的本产品备份包。无法识别的
//     文件（用户放错的其他 tar.gz、损坏包）不动——删掉它们超出"回收备份孤儿"的职责，
//     且会毁掉用户可能想抢救的数据。
//
// 与 BackupAdoptService 的关系：两者基于同一份差集（磁盘有、登记无），宽限期保证
// 用户先从界面导入的包不会被本作业抢先删掉。
//
// # 命名空间
//
// 只碰 backups/ 下的 `<id>.tar.gz` 与 `<id>.index`。`<id>.tar.gz.snap` 是生成中临时
// 文件，由生成流程的 defer 自清理，本作业不碰；其它文件类型同样不碰。
type BackupOrphanCleanupService struct {
	backups *BackupService
	adopt   *BackupAdoptService
	// grace 是宽限期：mtime 晚于 now-grace 的文件一律保留。
	grace time.Duration
	// maxDeletesPerRun 限制单轮删除数，避免首次上线在积压很久的实例上一次删掉海量文件。
	maxDeletesPerRun int
	now              func() time.Time
}

// BackupOrphanCleanupOptions 是一轮回收的阈值配置。
type BackupOrphanCleanupOptions struct {
	// Grace 是宽限期；<= 0 时回落到 defaultBackupOrphanGrace（不允许 0 宽限：
	// 那会让"刚拷贝进来、还没来得及导入"的包在下一轮就被删掉）。
	Grace time.Duration
}

// defaultBackupOrphanGrace 是默认宽限期：1 小时，足以覆盖"拷入 → 界面导入"的操作窗口。
const defaultBackupOrphanGrace = time.Hour

// maxBackupOrphanDeletesPerRun 是单轮删除上限，防止首次运行在积压实例上打爆磁盘 IO。
const maxBackupOrphanDeletesPerRun = 500

// BackupOrphanCleanupResult 汇总一轮回收的计数，供作业日志与测试断言。
type BackupOrphanCleanupResult struct {
	// Scanned 是扫描到的包体文件总数。
	Scanned int
	// Orphans 是判定为孤儿（登记里没有）的包数。
	Orphans int
	// Deleted 是实际删除的包数。
	Deleted int
	// Skipped 是在宽限期内、因此本轮保留的包数。
	Skipped int
	// Invalid 是无法识别为合法备份包、因此不动的包数。
	Invalid int
	// Failed 是删除报错的包数。
	Failed int
	// Capped 表示本轮因达到单轮上限而提前结束。
	Capped bool
}

// Empty 报告本轮是否什么都没做（用于抑制无意义的日志输出）。
func (r BackupOrphanCleanupResult) Empty() bool {
	return r.Orphans == 0 && r.Deleted == 0 && r.Skipped == 0 && r.Invalid == 0 && r.Failed == 0
}

// Summary 输出一轮回收的中文计数摘要（作业日志用）。
func (r BackupOrphanCleanupResult) Summary() string {
	s := fmt.Sprintf("扫描 %d 个包体 / 孤儿 %d 个 / 回收 %d 个 / 宽限期保留 %d 个 / 无法识别 %d 个 / 失败 %d 个",
		r.Scanned, r.Orphans, r.Deleted, r.Skipped, r.Invalid, r.Failed)
	if r.Capped {
		s += "（已达单轮上限，剩余留待下一轮）"
	}
	return s
}

// NewBackupOrphanCleanupService 装配孤儿回收服务。
func NewBackupOrphanCleanupService(backups *BackupService, adopt *BackupAdoptService, opts BackupOrphanCleanupOptions) *BackupOrphanCleanupService {
	grace := opts.Grace
	if grace <= 0 {
		grace = defaultBackupOrphanGrace
	}
	return &BackupOrphanCleanupService{
		backups:          backups,
		adopt:            adopt,
		grace:            grace,
		maxDeletesPerRun: maxBackupOrphanDeletesPerRun,
		now:              func() time.Time { return time.Now().UTC() },
	}
}

// Run 执行一轮孤儿回收。单个包删除失败不中断整轮，失败数如实上报。
func (s *BackupOrphanCleanupService) Run() (BackupOrphanCleanupResult, error) {
	var result BackupOrphanCleanupResult
	if s == nil || s.backups == nil || s.adopt == nil {
		return result, nil
	}

	// 直接复用 Scan 的差集口径：任何"未管理"判定只有一处实现，
	// 否则导入与回收会各算一套，迟早出现两边不一致的漏网包。
	candidates, err := s.adopt.Scan()
	if err != nil {
		return result, fmt.Errorf("扫描未管理备份包：%w", err)
	}

	// Scanned 取磁盘上的包体总数，用于日志里体现"绝大多数是正常包"。
	entries, err := os.ReadDir(s.backups.Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, fmt.Errorf("读取备份目录：%w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, backupPackageSuffix) && !strings.HasSuffix(name, backupSnapshotSuffix) {
			result.Scanned++
		}
	}

	result.Orphans = len(candidates)
	if len(candidates) == 0 {
		return result, nil
	}

	cutoff := s.now().Add(-s.grace)
	var errs []string
	for _, cand := range candidates {
		if result.Deleted >= s.maxDeletesPerRun {
			result.Capped = true
			break
		}
		// 无法识别的文件不动：可能是用户放错的其他归档，删除超出本作业职责。
		if !cand.Valid {
			result.Invalid++
			continue
		}
		// 宽限期：刚落地/刚被改动的文件保留，给"界面导入"留出窗口。
		// 时间戳解析失败时 parseBackupTime 返回零值（极早），因此会被判为可回收——
		// 但上面已经过了 Valid 校验（能读出合法 manifest），零值只可能来自异常 mtime，
		// 那种情况下一律保留更安全，故这里显式判零。
		modified := parseBackupTime(cand.ModifiedAt)
		if modified.IsZero() || modified.After(cutoff) {
			result.Skipped++
			continue
		}
		if err := s.removeOrphan(cand.FileID); err != nil {
			result.Failed++
			errs = append(errs, fmt.Sprintf("回收孤儿包 %s：%v", cand.FileID, err))
			continue
		}
		result.Deleted++
	}

	if len(errs) > 0 {
		return result, fmt.Errorf("%s", strings.Join(errs, "；"))
	}
	return result, nil
}

// removeOrphan 删除孤儿包的包体与侧车。
//
// 侧车缺失不算错（历史包可能本就没有），删除的目的是"删干净"而不是"断言它存在"，
// 与 BackupService.Delete 的口径一致。
func (s *BackupOrphanCleanupService) removeOrphan(fileID string) error {
	if !safeBackupFileID(fileID) {
		return fmt.Errorf("非法的包标识")
	}
	tarPath := filepath.Join(s.backups.Dir(), fileID+backupPackageSuffix)
	idxPath := filepath.Join(s.backups.Dir(), fileID+backupSidecarSuffix)
	if err := os.Remove(tarPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除包体：%w", err)
	}
	if err := os.Remove(idxPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除侧车索引：%w", err)
	}
	return nil
}
