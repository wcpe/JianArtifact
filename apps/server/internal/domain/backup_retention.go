package domain

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// BackupRetentionOptions 是一轮保留策略的阈值（；初始值来自环境变量，
// 之后可经界面修改并在下一轮生效）。
//
// 两个阈值**同时生效、取更严格者**：任一条判定为「超限」就淘汰，而不是要求两条都超。
// KEEP_LAST 是「最多留几份」、RETENTION_DAYS 是「最多留几天」，两条都是上限，
// 上限之间取交集（超限条件是 OR），不是并集。
type BackupRetentionOptions struct {
	// KeepLast 是保留的最近自动备份份数；<= 0 表示不按份数限制。
	KeepLast int
	// RetentionDays 是保留的自动备份天数；<= 0 表示不限天数。
	RetentionDays int
}

// Enabled 报告本配置是否会真正淘汰任何东西。两条都是"不限制"时整个策略是空操作。
func (o BackupRetentionOptions) Enabled() bool { return o.KeepLast > 0 || o.RetentionDays > 0 }

// BackupRetentionResult 汇总一轮保留策略的计数，供作业日志与测试断言
// （对齐 StorageCleanupResult 的 Empty / Summary 范式）。
type BackupRetentionResult struct {
	// Candidates 是进入判定的自动备份总数（已按最近优先排序后的全量）。
	Candidates int
	// Deleted 是实际删除的包数。
	Deleted int
	// Skipped 是被差包引用为基线、因而跳过不删的包数。
	//
	// 这个数必须单独报出来：跳过是**预期内**的正常结果（有差包依赖基线时，基线就是不能删），
	// 若与"没有可删的包"混为一谈，运维看到"淘汰 0 份"会以为策略没生效。
	Skipped int
	// Failed 是删除过程中报错（非"被引用"）的包数。
	Failed int
}

// Empty 报告本轮是否什么都没做（用于抑制无意义的日志输出）。
func (r BackupRetentionResult) Empty() bool { return r.Deleted == 0 && r.Skipped == 0 && r.Failed == 0 }

// Summary 输出一轮保留策略的中文计数摘要（作业日志用）。
func (r BackupRetentionResult) Summary() string {
	return fmt.Sprintf("候选 %d 份 / 淘汰 %d 份 / 因被差包引用跳过 %d 份 / 失败 %d 份",
		r.Candidates, r.Deleted, r.Skipped, r.Failed)
}

// BackupRetentionService 对自动备份包执行保留策略。
//
// # 只淘汰 origin='auto'
//
// 候选集由 repository.BackupPackageRepo.ListRetentionCandidates 给出，SQL 里已写死
// `origin='auto' AND status='done'`。手动备份是人工决策产物，**永不**自动删除——
// 这不是"默认如此"，而是规格 §2.5 明确列出的范围外事项（不做手动包的自动保留策略）。
//
// # 被引用的基线跳过而不是报错
//
// 删除走的是 BackupService.Delete，它在被差包引用为基线时返回 ErrBackupHasDerived。
// 保留策略把该错误计为 Skipped 并继续处理下一个候选：
//   - 不报错，因为"有差包依赖某份基线"是完全正常的运维状态（规格 §2.2 要求跳过而非报错）；
//   - 也不越过它去删更老的包，因为那会让保留策略偷偷变成"删掉差包的基线"，
//     直接破坏派生包的可还原性。
//
// 复用 Delete 而不是自己拼 DELETE 语句，是为了让"能否删除"只有一处判定：
// 将来若给删除加上新的约束（例如被冻结窗口占用），保留策略自动继承，不会绕过。
//
// # 时间基准是 time.Now()
//
// 天数据阈值按「创建时间早于 now - N 天」判定，与 domain/storage_cleanup.go 的
// storageMetadataRetention 同一口径。作业流程（规格 §2.2）是「先生成一份新备份，
// 再应用保留策略」，因此基准点前一定存在一份刚生成的包，不存在"停机很久后启动、
// 第一轮就把自己所有备份判为超期"的窗口——这也是为什么本条不需要额外的
// "最近一份永不删"护栏：那条护栏会掩盖配置错误（把天数配成 0 却仍在删），
// 而它想防的情况在流程上不会发生。
type BackupRetentionService struct {
	backupSvc *BackupService
	repo      *repository.BackupPackageRepo
	// mu 保护 opts：作业每轮用最新配置覆盖阈值（追加：配置改为运行时可改），
	// 而 Run 可能正被手动触发或上一轮心跳调用，两者必须串行读，避免读到写一半的值。
	mu   sync.RWMutex
	opts BackupRetentionOptions
	now  func() time.Time
}

// NewBackupRetentionService 装配保留策略服务。
func NewBackupRetentionService(backupSvc *BackupService, repo *repository.BackupPackageRepo, opts BackupRetentionOptions) *BackupRetentionService {
	return &BackupRetentionService{
		backupSvc: backupSvc,
		repo:      repo,
		opts:      opts,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// SetOptions 用最新配置覆盖阈值（追加：三项配置改为运行时可改）。
//
// 每次 Run 之前由作业调用，使"界面上改完配置"在下一轮即生效，无需重启。
// 只覆盖阈值，不动 now（测试注入的时间基准必须保持稳定）。
func (s *BackupRetentionService) SetOptions(opts BackupRetentionOptions) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.opts = opts
	s.mu.Unlock()
}

// options 读取当前阈值快照。
func (s *BackupRetentionService) options() BackupRetentionOptions {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.opts
}

// Run 应用一次保留策略。单个包删除失败不中断整轮（继续处理剩余候选），
// 但失败会如实计入结果并随返回错误上报——不粉饰。
func (s *BackupRetentionService) Run() (BackupRetentionResult, error) {
	var result BackupRetentionResult
	if s == nil || s.repo == nil || s.backupSvc == nil {
		return result, nil
	}
	// 阈值取本轮快照，避免读到被 SetOptions 改了一半的值。
	opts := s.options()
	if !opts.Enabled() {
		return result, nil
	}

	candidates, err := s.repo.ListRetentionCandidates()
	if err != nil {
		return result, fmt.Errorf("列出自动备份候选：%w", err)
	}
	result.Candidates = len(candidates)
	if len(candidates) == 0 {
		return result, nil
	}

	cutoff := time.Time{}
	if opts.RetentionDays > 0 {
		cutoff = s.now().AddDate(0, 0, -opts.RetentionDays)
	}

	var errs []error
	for i, pkg := range candidates {
		// 候选已按 created_at DESC 排序（见 ListRetentionCandidates），
		// 因此下标 i 就是"从最近一份起的第 i+1 份"。
		byCount := opts.KeepLast > 0 && i >= opts.KeepLast
		byAge := opts.RetentionDays > 0 && parseBackupTime(pkg.CreatedAt).Before(cutoff)
		// 两条上限取更严格者：任一条超限即淘汰。
		if !byCount && !byAge {
			continue
		}
		if err := s.backupSvc.Delete(pkg.PackageID); err != nil {
			if errors.Is(err, ErrBackupHasDerived) {
				result.Skipped++
				continue
			}
			result.Failed++
			errs = append(errs, fmt.Errorf("淘汰备份包 %s：%w", pkg.PackageID, err))
			continue
		}
		result.Deleted++
	}
	if len(errs) > 0 {
		return result, errors.Join(errs...)
	}
	return result, nil
}

// parseBackupTime 解析登记里的 created_at（RFC3339Nano，UTC）。解析失败返回零值，
// 该包在时间维度上被判为"极早"。份数维度不受影响，仍按排序下标保护最近的若干份；
// 而时间维度上宁可偏向删除：一条无法解析的时间戳本身就是需要人工介入的坏数据，
// 且备份包体仍可从列表与日志中追溯。
func parseBackupTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
