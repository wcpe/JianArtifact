package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 演练错误。
var (
	// ErrDrillPackageNotDone 表示目标包尚未生成完成（非 done），没有可演练的包体。
	ErrDrillPackageNotDone = errors.New("备份包尚未生成完成，无法演练")
	// ErrDrillLevelUnknown 表示请求的演练级别不是 shallow / deep。
	ErrDrillLevelUnknown = errors.New("未知的演练级别")
)

// drillStagingDirName 是演练临时目录名（落在 data 目录下、每次演练独立子目录）。
//
// 为什么放 data 目录而不是系统临时目录：解包会产生与包体等量的临时文件，
// 深校验大库时可达数 GB。系统 /tmp 在多数部署里是内存盘或容量很小的分区，
// 而 data 目录所在分区按定义容得下这个实例的数据（备份包本身也写在那里）。
const drillStagingDirName = "backup-drill"

// drillCountTables 是演练对解出的 SQLite 做行数比对的白名单。
//
// 与 backupCountTables（生成侧）刻意不同：生成侧的清单服务于「包里带了哪些实体」的
// 人读计数，演练侧关心的是「恢复后业务是否可用」，因此取的是访问路径上不可缺的表。
// 这里只校验表**可查询**（结构完整、无损坏），不比对具体数值——演练对象是任意一个
// 历史包，没有"应有行数"可对；真正的数值比对由导入端按 manifest 的 counts 完成。
var drillCountTables = []string{"user", "repository", "acl", "asset", "format_metadata"}

// DrillBackupOptions 控制一次恢复演练。
type DrillBackupOptions struct {
	// Level 为 shallow（结构摘要校验）或 deep（额外逐 blob 比对内容摘要）。
	Level string
}

// DrillCheck 是演练报告中的一项结论。
type DrillCheck struct {
	// Name 是检查项标识（稳定，供前端与日志按名引用）。
	Name string `json:"name"`
	// OK 表示该项是否通过。
	OK bool `json:"ok"`
	// Detail 是人读结论（通过时说明规模，失败时说明原因）。
	Detail string `json:"detail"`
	// DurationMS 是该项耗时；0 表示未单独计时。
	DurationMS int64 `json:"durationMs,omitempty"`
}

// DrillResult 是一次演练的完整结果（报告落库后的形态）。
type DrillResult struct {
	PackageID string        `json:"packageId"`
	Level     string        `json:"level"`
	OK        bool          `json:"ok"`
	StartedAt time.Time     `json:"startedAt"`
	Finished  time.Time     `json:"finishedAt"`
	Duration  time.Duration `json:"-"`
	Checks    []DrillCheck  `json:"checks"`
	// ErrorSummary 为失败原因摘要；成功时为空。
	ErrorSummary string `json:"errorSummary,omitempty"`
	// Verification 是落库后的报告记录（含 id，供前端按 id 查详情）。
	Verification repository.BackupVerification `json:"verification"`
	// StagingDir 是本次演练使用的隔离临时目录（诊断用；返回时已清理）。
	StagingDir string `json:"stagingDir,omitempty"`
}

// BackupDrillService 执行恢复演练：在隔离临时目录里真解包、真校验，产出结构化报告（FR-40 §2.3）。
//
// # 与真恢复的边界（关键约束）
//
// 本服务**绝不**调用 RestoreService.Stage，也绝不写 restore.pending / restore-staging/。
// 那些是"下次重启触发真替换"的副作用，演练必须是纯只读路径：只读包体、只写自己的临时目录。
// 由 TestBackupDrillNeverTouchesRestoreMarkers 守护这条边界。
//
// 之所以不"复用" RestoreService 的校验代码：Stage 的职责是"落盘待生效的恢复"，
// 它的每一步都绑定在"即将替换本实例"这个前提上。演练要的是同一个校验动作、
// 但完全不产生待生效状态，两者共享的只有 archive.Reader 这一层原语，
// 而它本来就是只读的——共享已经发生在正确的层次上。
type BackupDrillService struct {
	backupSvc  *BackupService
	verifyRepo *repository.BackupVerificationRepo
	dataDir    string
	now        func() time.Time
}

// NewBackupDrillService 装配演练服务。dataDir 用于放置隔离临时目录。
func NewBackupDrillService(backupSvc *BackupService, verifyRepo *repository.BackupVerificationRepo, dataDir string) *BackupDrillService {
	return &BackupDrillService{
		backupSvc:  backupSvc,
		verifyRepo: verifyRepo,
		dataDir:    dataDir,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// StagingRoot 返回演练临时目录的根（每次演练在其下建独立子目录）。
func (s *BackupDrillService) StagingRoot() string {
	return filepath.Join(s.dataDir, drillStagingDirName)
}

// Drill 对指定备份包执行一次恢复演练，并把报告落库。
//
// 报告**无论成败都会落库**：失败的报告正是演练最该留下的东西。
// 只有"包不存在 / 状态不对 / 报告写不进去"这类连演练都没法开始的情况才返回错误。
func (s *BackupDrillService) Drill(ctx context.Context, packageID string, opts DrillBackupOptions) (DrillResult, error) {
	level := opts.Level
	if level == "" {
		level = repository.VerificationLevelShallow
	}
	if level != repository.VerificationLevelShallow && level != repository.VerificationLevelDeep {
		return DrillResult{}, fmt.Errorf("%w：%q（支持 shallow / deep）", ErrDrillLevelUnknown, opts.Level)
	}

	// ① 校验包存在且已生成完成。
	rec, err := s.backupSvc.Get(packageID)
	if err != nil {
		return DrillResult{}, err
	}
	if rec.Status != repository.BackupStatusDone {
		return DrillResult{}, fmt.Errorf("%w：%s 当前状态 %s", ErrDrillPackageNotDone, packageID, rec.Status)
	}

	started := s.now()
	result := DrillResult{PackageID: packageID, Level: level, StartedAt: started}

	// ② 建隔离临时目录。每次演练独立子目录，避免并发演练互相踩踏。
	staging := filepath.Join(s.StagingRoot(), fmt.Sprintf("%s-%d", packageID, started.UnixNano()))
	result.StagingDir = staging
	// 无论成败都清理临时目录：这是"演练不留痕"的硬要求（规格 §3.4）。
	// 用 defer 而不是在每个失败分支里手写清理，避免将来新增分支时漏掉一条路径。
	defer func() { _ = os.RemoveAll(staging) }()

	checks, runErr := s.runChecks(ctx, rec, staging, level)
	result.Checks = checks
	result.Finished = s.now()
	result.Duration = result.Finished.Sub(started)
	result.OK = runErr == nil
	if runErr != nil {
		result.ErrorSummary = runErr.Error()
	}

	// ③ 报告落库（成败都写）。
	checksJSON, err := json.Marshal(checks)
	if err != nil {
		return result, fmt.Errorf("序列化演练结论：%w", err)
	}
	report, err := s.verifyRepo.Create(repository.BackupVerification{
		PackageID:    packageID,
		Level:        level,
		OK:           result.OK,
		StartedAt:    started.Format(time.RFC3339Nano),
		FinishedAt:   result.Finished.Format(time.RFC3339Nano),
		DurationMS:   result.Duration.Milliseconds(),
		ChecksJSON:   string(checksJSON),
		ErrorSummary: result.ErrorSummary,
	})
	if err != nil {
		return result, fmt.Errorf("写入演练报告：%w", err)
	}
	result.Verification = report
	return result, nil
}

// runChecks 依次执行四项检查，返回逐项结论与首个致命错误。
//
// 设计取舍：**后续检查在遇到失败后继续执行**（能跑的都跑），而不是首错即停。
// 演练的价值恰恰在于"这份包到底哪里坏了"——只报第一条会让运维反复重跑。
// 致命错误取第一条，因为它是根因；后续失败往往是它的连锁反应。
func (s *BackupDrillService) runChecks(ctx context.Context, rec *repository.BackupPackage, staging, level string) ([]DrillCheck, error) {
	deep := level == repository.VerificationLevelDeep
	var (
		checks   []DrillCheck
		firstErr error
		reader   *archive.Reader
		manifest archive.Manifest
	)

	// 检查 1：打开包并校验完整性（deep 时逐 blob 比对内容摘要）。
	started := time.Now()
	r, m, err := s.backupSvc.OpenReader(rec.PackageID)
	reader, manifest = r, m
	check := DrillCheck{Name: "integrity"}
	if err != nil {
		check.OK = false
		check.Detail = err.Error()
		firstErr = err
	} else if err := r.Verify(deep); err != nil {
		check.OK = false
		check.Detail = err.Error()
		firstErr = err
	} else {
		check.OK = true
		check.Detail = fmt.Sprintf("归档完整：制品 %d 件，blob %d 个（%d 字节），级别 %s",
			m.Counts.Assets, m.Blobs.Count, m.Blobs.TotalBytes, level)
	}
	check.DurationMS = time.Since(started).Milliseconds()
	checks = append(checks, check)

	// 检查 2：真解包到隔离目录。包体已损坏时这一步通常也会失败，属于预期内的连锁结果。
	started = time.Now()
	check = DrillCheck{Name: "extract"}
	extracted := false
	if err := ctx.Err(); err != nil {
		check.Detail = err.Error()
		firstErr = firstErrOr(firstErr, err)
	} else if reader == nil {
		check.Detail = "前置检查未通过，跳过解包"
	} else if err := reader.Extract(staging, nil); err != nil {
		check.Detail = err.Error()
		firstErr = firstErrOr(firstErr, err)
	} else {
		check.OK = true
		extracted = true
		check.Detail = fmt.Sprintf("已解包到隔离目录：%s", staging)
	}
	check.DurationMS = time.Since(started).Milliseconds()
	checks = append(checks, check)

	// 检查 3：解出的 SQLite 一致性（PRAGMA quick_check）+ 关键表可查询。
	started = time.Now()
	check = DrillCheck{Name: "sqlite"}
	dbPath := filepath.Join(staging, archive.DBName)
	if !extracted {
		check.Detail = "前置检查未通过，跳过 SQLite 校验"
	} else if err := persistence.SnapshotIntegrity(dbPath, drillCountTables); err != nil {
		check.Detail = err.Error()
		firstErr = firstErrOr(firstErr, err)
	} else {
		check.OK = true
		check.Detail = fmt.Sprintf("quick_check 通过，关键表（%d 张）可查询", len(drillCountTables))
	}
	check.DurationMS = time.Since(started).Milliseconds()
	checks = append(checks, check)

	// 检查 4：解出的 blob 集合与 manifest 的规模一致（内容摘要已由检查 1 的 deep 覆盖）。
	started = time.Now()
	check = DrillCheck{Name: "blobs"}
	if !extracted || reader == nil {
		check.Detail = "前置检查未通过，跳过 blob 规模比对"
	} else if err := compareExtractedBlobs(staging, manifest); err != nil {
		check.Detail = err.Error()
		firstErr = firstErrOr(firstErr, err)
	} else {
		check.OK = true
		check.Detail = fmt.Sprintf("解出的 blob 数与 manifest 一致：%d 个", manifest.Blobs.Count)
	}
	check.DurationMS = time.Since(started).Milliseconds()
	checks = append(checks, check)

	return checks, firstErr
}

// compareExtractedBlobs 比对解出的 blob 文件数与 manifest 声明是否一致。
//
// 只数条目、不重算内容摘要：deep 级别的检查 1 已经流式读过每个 blob 并比对过摘要，
// 这里重复计算等于把同一份数据再读一遍，对深校验大库是纯浪费。
//
// 目录不存在 == 零个 blob，而不是错误：Reader.Extract 只为真实存在的 blob 条目建目录，
// 因此一个没有任何资产的实例（合法的空备份包）解包后不会出现 blobs/。
// 把它当错误会让"空实例的包"永远演练失败，而这恰恰是最该通过的一种包。
func compareExtractedBlobs(staging string, manifest archive.Manifest) error {
	root := filepath.Join(staging, archive.BlobDir)
	count := 0
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	switch {
	case os.IsNotExist(err):
		count = 0
	case err != nil:
		return fmt.Errorf("遍历解出的 blob 目录：%w", err)
	}
	if count != manifest.Blobs.Count {
		return fmt.Errorf("解出的 blob 数不符：manifest %d 实际 %d", manifest.Blobs.Count, count)
	}
	return nil
}

// firstErrOr 保留第一条错误（根因），后续连锁失败只进 checks 不覆盖根因。
func firstErrOr(current, next error) error {
	if current != nil {
		return current
	}
	return next
}

// ListVerifications 返回某包的历史演练报告（分页，最近优先）。
func (s *BackupDrillService) ListVerifications(packageID string, limit, offset int) ([]repository.BackupVerification, int, error) {
	if _, err := s.backupSvc.Get(packageID); err != nil {
		return nil, 0, err
	}
	items, err := s.verifyRepo.ListByPackage(packageID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.verifyRepo.CountByPackage(packageID)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}
