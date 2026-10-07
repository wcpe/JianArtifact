package domain

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// storageSQLTimeLayout 与 SQLite datetime('now') 的输出一致（UTC，秒精度），
// 使 Go 侧算出的时间窗可与表内文本列直接做字典序比较。
const storageSQLTimeLayout = "2006-01-02 15:04:05"

// emptyQuarantineDirGrace 是隔离区空目录清理的固定宽限期：不设开关，代码里写死。
//
// 它是防误删护栏，而不是可调策略：Stage 的落盘顺序是「先 MkdirAll 建操作目录、再
// rename 隔离文件进目录」，两者之间存在一个极短的「目录已存在但为空」窗口；
// 一个可被运维调成 0 的开关正好能打开这个窗口，把在途操作的隔离目录删掉。
// 固定 1 小时远长于任何单次资产操作（该窗口以微秒计），且顺序执行时每个操作目录
// 都只会先变空再被封存，因此既不会误删在途目录，也能让成功回收后的空壳目录
// 在下一轮被收走。
const emptyQuarantineDirGrace = time.Hour

// maxProxyCacheDeletesPerRun 是单个清理轮次删除代理缓存资产的硬上限。
// 代理缓存是可重新拉取的内容，但一次删爆仍会引发上游流量洪峰，
// 因此先用上限收敛每轮删除量，剩余部分由后续轮次继续收敛。
const maxProxyCacheDeletesPerRun = 200

// StorageCleanupOptions 是一轮存储治理清理的阈值配置，取值来自环境变量。
// 每个阈值 <= 0 即关闭对应子步骤（隔离区空目录清理无需阈值：它由固定宽限期保护）。
type StorageCleanupOptions struct {
	MetadataRetention time.Duration // 终态操作与隔离元数据的保留期
	TempMaxAge        time.Duration // OCI 上传临时文件最长滞留时长
}

// StorageCleanupResult 汇总一轮清理的计数，供作业日志与测试断言。
type StorageCleanupResult struct {
	MutationRows   int // 裁剪掉的 asset_mutation 终态行
	ItemRows       int // 裁剪掉的 asset_mutation_item 行
	QuarantineRows int // 裁剪掉的 blob_quarantine 终态行
	EmptyDirs      int // 删除的隔离区空目录
	TempFiles      int // 删除的过期上传临时文件
	CacheAssets    int // 删除的过期代理缓存资产
}

// Empty 报告本轮是否什么都没清掉（用于抑制无意义的日志输出）。
func (r StorageCleanupResult) Empty() bool {
	return r.MutationRows == 0 && r.ItemRows == 0 && r.QuarantineRows == 0 &&
		r.EmptyDirs == 0 && r.TempFiles == 0 && r.CacheAssets == 0
}

// Summary 输出一轮清理的中文计数摘要（作业日志用）。
func (r StorageCleanupResult) Summary() string {
	return fmt.Sprintf("终态操作 %d 行 / 操作明细 %d 行 / 隔离终态记录 %d 行 / 空隔离目录 %d 个 / 过期上传临时文件 %d 个 / 代理缓存资产 %d 个",
		r.MutationRows, r.ItemRows, r.QuarantineRows, r.EmptyDirs, r.TempFiles, r.CacheAssets)
}

// StorageCleanupService 执行 storage-cleanup 调度作业的全部子步骤：
// 终态元数据裁剪、隔离区空目录清理、过期上传临时文件清理、代理缓存保留。
//
// # 本批不做：pending_gc 滞留件回收（留待后续增量）
//
// 计划 §4.2 的第一件事是「用启动恢复的同一条既有代码路径把 blob_quarantine 中
// pending_gc 的滞留条目推进到终态」。
//
// 前置能力缺口（这是不做它的唯一原因）：「在途操作判定」接口不存在，因此无法满足
// 计划为该动作设定的前置条件「宽限期 + 在途排除（只处理操作引擎确认不在途的条目）」。
//
//   - AssetMutationCoordinator 的写门 gate 是私有字段（asset_mutation.go:20-28），
//     公开面只有 Read（只取读锁，:54）与 Apply*（取写锁但会真的执行一次资产变更，:61-113），
//     没有任何「在写门下执行任意函数」的原语，也没有在途 operationID 的注册表/集合。
//     Recover / RecoverReceived（:364-367）不是判定接口，它们的语义是「进程刚启动，
//     因此本地不存在任何在途操作」，依赖的正是定时作业无法假设的那个不变量。
//   - recover() 前半段（:382-413）以「全部 intent 都是崩溃遗留」为前提做 restore +
//     MarkRolledBack。从定时器调用它会与正在进行的操作直接冲突：把隔离中的 blob
//     从活操作脚下恢复走，并把仍将提交的操作标成 rolled_back。
//   - recover() 后半段（:414-425）用 ListPendingQuarantine 取 pending_gc/staged 记录，
//     再靠 op.Status == "completed" 过滤。但 pending_gc 恰恰是**在途窗口内**写入的状态
//     （:169 先标 pending_gc，:172 才 finalize），因此该查询结果里确实可能含活操作的记录；
//     且已提交但尚未完成 finalizeSnapshot 的操作（:191-204）与清理会竞争同一份回滚快照
//     与隔离文件，而没有写门入口就无法消除这个窗口。
//
// 结论：不自己发明一套文件删除逻辑，也不把这一点悄悄做掉——滞留件回收整体留待后续增量
// （前置能力：操作引擎提供「在写门下执行、且能判定操作是否在途」的入口，或把回收动作
// 收敛进操作引擎自身）。宁可不做，不可破坏回滚原子性与 ADR-0024 的
// 「引用归零→同步物理回收」语义。滞留件的现状不会因此变差：它仍与今天一样由启动恢复处理。
//
// 本批实际执行的四件事——「已无任何字节关联的终态元数据裁剪」「已空的隔离目录」
// 「过期上传临时文件」「走既有删除通道的代理缓存保留」——都不会触碰任何仍在途的文件。
type StorageCleanupService struct {
	meta     *repository.StorageCleanupRepo
	assetSvc *AssetService
	blobs    *blobstore.Store
	opts     StorageCleanupOptions
	now      func() time.Time
	// cacheDeletesPerRun 覆盖单轮代理缓存删除上限；<= 0 时回落到 maxProxyCacheDeletesPerRun。
	cacheDeletesPerRun int
}

// NewStorageCleanupService 构造存储治理清理服务。assetSvc 提供既有资产删除通道
// （引用归零→同步物理回收），blobs 提供隔离区与上传暂存目录的维护原语。
func NewStorageCleanupService(meta *repository.StorageCleanupRepo, assetSvc *AssetService, blobs *blobstore.Store, opts StorageCleanupOptions) *StorageCleanupService {
	return &StorageCleanupService{
		meta: meta, assetSvc: assetSvc, blobs: blobs, opts: opts, now: time.Now,
		cacheDeletesPerRun: maxProxyCacheDeletesPerRun,
	}
}

// SetProxyCacheDeletesPerRun 调整单轮代理缓存删除上限（默认 maxProxyCacheDeletesPerRun=200；
// 测试与调优用，<= 0 忽略）。把上限压小后，验证「每轮删除量有上限、剩余留待下一轮」
// 只需造「上限 + 少量」个资产，而不必造满默认上限的规模。
func (s *StorageCleanupService) SetProxyCacheDeletesPerRun(n int) {
	if s == nil || n <= 0 {
		return
	}
	s.cacheDeletesPerRun = n
}

// proxyCacheDeleteLimit 返回本轮生效的删除上限；零值/未设置时回落到默认常量，
// 使直接构造的零值服务保持与今天完全一致的行为。
func (s *StorageCleanupService) proxyCacheDeleteLimit() int {
	if s == nil || s.cacheDeletesPerRun <= 0 {
		return maxProxyCacheDeletesPerRun
	}
	return s.cacheDeletesPerRun
}

// Run 执行一轮清理并返回计数。单个子步骤失败不会跳过其余子步骤，
// 全部错误以 errors.Join 合并上报，由调度器记录为一次失败。
func (s *StorageCleanupService) Run() (StorageCleanupResult, error) {
	var result StorageCleanupResult
	if s == nil {
		return result, nil
	}
	now := s.now()
	var errs []error

	if s.meta != nil && s.opts.MetadataRetention > 0 {
		counts, err := s.meta.PruneTerminalMutations(storageSQLTime(now.Add(-s.opts.MetadataRetention)))
		if err != nil {
			errs = append(errs, err)
		} else {
			result.MutationRows = counts.Mutations
			result.ItemRows = counts.Items
			result.QuarantineRows = counts.Quarantine
		}
	}

	// 空目录清理不依赖元数据裁剪是否成功：两者的判定条件彼此独立
	// （前者只删「目录为空且已过固定宽限期」，后者只删「终态且已过保留期」）。
	if s.blobs != nil {
		removed, err := s.blobs.RemoveEmptyQuarantineDirs(now.Add(-emptyQuarantineDirGrace))
		if err != nil {
			errs = append(errs, fmt.Errorf("清理隔离区空目录：%w", err))
		} else {
			result.EmptyDirs = removed
		}
	}

	if s.blobs != nil && s.opts.TempMaxAge > 0 {
		removed, skipped, err := s.blobs.CleanupOCIUploadTempsBefore(now.Add(-s.opts.TempMaxAge))
		if err != nil {
			errs = append(errs, fmt.Errorf("清理过期上传临时文件：%w", err))
		} else {
			result.TempFiles = removed
			if skipped > 0 {
				log.Printf("存储治理清理：%d 个过期上传临时文件暂不可删除，留待下一轮重试", skipped)
			}
		}
	}

	cacheDeleted, err := s.purgeExpiredProxyCache(now)
	if err != nil {
		errs = append(errs, err)
	}
	result.CacheAssets = cacheDeleted

	return result, errors.Join(errs...)
}

// purgeExpiredProxyCache 对开启了 cacheRetentionDays 的 proxy 仓库删除超期缓存资产。
// 删除统一走 AssetService.Delete（即既有资产变更通道）：引用归零时在同一操作内同步物理回收，
// 不引入异步 GC。默认关闭：未配置该值的仓库一个字节都不会被删除。
func (s *StorageCleanupService) purgeExpiredProxyCache(now time.Time) (int, error) {
	if s.meta == nil || s.assetSvc == nil {
		return 0, nil
	}
	repos, err := s.meta.ListProxyRepositories()
	if err != nil {
		return 0, fmt.Errorf("列举代理仓库：%w", err)
	}
	remaining := s.proxyCacheDeleteLimit()
	deleted := 0
	var errs []error
	for _, repo := range repos {
		if remaining <= 0 {
			break
		}
		cfg, err := repo.DecodeConfig()
		if err != nil {
			errs = append(errs, fmt.Errorf("解析仓库 %s 的配置：%w", repo.Name, err))
			continue
		}
		if cfg.CacheRetentionDays <= 0 {
			// 缺省（0）即关闭；这类仓库完全不参与本步骤。
			continue
		}
		stale, err := s.meta.ListStaleAssets(repo.ID, storageSQLTime(now.AddDate(0, 0, -cfg.CacheRetentionDays)), remaining)
		if err != nil {
			errs = append(errs, fmt.Errorf("列举仓库 %s 的超期缓存：%w", repo.Name, err))
			continue
		}
		repoDeleted := 0
		for _, asset := range stale {
			if err := s.assetSvc.Delete(repo.Name, asset.Path); err != nil {
				// 并发删除/覆盖导致的缺失按已消失处理；其余错误如实上报但不中断本轮。
				if !errors.Is(err, ErrNotFound) {
					errs = append(errs, fmt.Errorf("删除仓库 %s 的超期缓存 %s：%w", repo.Name, asset.Path, err))
				}
				continue
			}
			repoDeleted++
			deleted++
			remaining--
			if remaining <= 0 {
				break
			}
		}
		if repoDeleted > 0 {
			log.Printf("代理缓存保留：仓库 %s 删除超期缓存资产 %d 个（保留 %d 天）", repo.Name, repoDeleted, cfg.CacheRetentionDays)
		}
	}
	if remaining <= 0 {
		log.Printf("代理缓存保留：本轮达到 %d 个删除上限，剩余超期缓存留待下一轮", s.proxyCacheDeleteLimit())
	}
	return deleted, errors.Join(errs...)
}

// storageSQLTime 把时间格式化为与 SQLite datetime('now') 同构的 UTC 文本。
func storageSQLTime(t time.Time) string {
	return t.UTC().Format(storageSQLTimeLayout)
}
