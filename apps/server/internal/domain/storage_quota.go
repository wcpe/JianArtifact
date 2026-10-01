package domain

import (
	"errors"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// ErrRepoStorageQuotaExceeded 表示写入会突破仓库级存储配额（FR-41 §4.1）。
// 它只作判定用途：真正的错误值由 StorageQuotaError 承载，并同时满足
// errors.Is(err, ErrQuotaExceeded)，从而复用既有 429 映射口径。
var ErrRepoStorageQuotaExceeded = errors.New("仓库存储配额已超限")

// RejectionReasonQuota 是发布拒绝原因的**闭集枚举**取值之一（FR-41 §4.5）。
// 该取值与 docs/specs/0.11.0-prometheus-metrics.md 的 reason 标签取值一一对应：
// **新增取值必须先改规格并同步测试与 OPERATIONS.md**，不得在实现期就地塞入自由字符串
// （否则会把无界值带进指标标签，违反该规格第 22 行）。
const RejectionReasonQuota = "quota"

// RejectionRecorder 记录一次发布拒绝，供进程内指标累加（FR-41 §4.5）。
// 由装配层注入 *metrics.Registry；nil 表示不记录（测试与未接线部署）。
type RejectionRecorder interface {
	// PublishRejection 累加一次按原因分区的发布拒绝计数；reason 必须是闭集枚举取值。
	PublishRejection(reason string)
}

// RepoQuotaLimits 是一个仓库的存储配额上限；0 表示不限。
// 口径为**逻辑字节** SUM(asset.size) 与**制品数** COUNT(*)，
// 与去重后的物理占用不等价（去重 blob 无法按仓库归属）。
type RepoQuotaLimits struct {
	Bytes  int64
	Assets int64
}

// Empty 表示未配置任何配额（0/缺省 = 不限）。
func (l RepoQuotaLimits) Empty() bool { return l.Bytes == 0 && l.Assets == 0 }

// StorageQuotaError 描述一次被仓库存储配额拒绝的写入。
// 错误消息只包含仓库名与占用/上限数字，**不含任何文件系统路径**（对齐 docs/API.md:10）。
type StorageQuotaError struct {
	Repository string
	Limits     RepoQuotaLimits
	UsedBytes  int64
	UsedAssets int64
}

// Error 渲染可读的拒绝原因：含「当前占用」与「上限」。
func (e *StorageQuotaError) Error() string {
	used := make([]string, 0, 2)
	limits := make([]string, 0, 2)
	if e.Limits.Bytes > 0 {
		used = append(used, strconv.FormatInt(e.UsedBytes, 10)+" 字节")
		limits = append(limits, strconv.FormatInt(e.Limits.Bytes, 10)+" 字节")
	}
	if e.Limits.Assets > 0 {
		used = append(used, strconv.FormatInt(e.UsedAssets, 10)+" 件")
		limits = append(limits, strconv.FormatInt(e.Limits.Assets, 10)+" 件")
	}
	// 分隔符用顿号而非「/」：错误消息不得含文件系统分隔符——跨平台守卫会一并检查 / 与 \，
	// 用「/」会在 Linux 上被误判成路径（CI 实测：本地 Windows 通过、Linux 失败）。
	text := "仓库 " + e.Repository + " 存储配额已超限：当前占用 " + strings.Join(used, "、") +
		"，上限 " + strings.Join(limits, "、")
	return text
}

// Is 让仓库配额拒绝同时匹配既有发布额度错误，保证协议层现成的
// ErrQuotaExceeded → 429 quota_exceeded 映射无需改动即可覆盖本错误。
func (e *StorageQuotaError) Is(target error) bool {
	return target == ErrQuotaExceeded || target == ErrRepoStorageQuotaExceeded
}

// StorageQuotaGuard 在资产提交事务内复检仓库存储配额（FR-41 §4.1 判定时序第 3 级）。
// 与资产写入处于同一事务：越限返回错误即整批回滚，因此已提交状态永不越界。
// items 为本次操作的全部资产项（含删除项）。
type StorageQuotaGuard func(tx *sqlx.Tx, items []repository.AssetMutationItem) error

// markQuotaExempt 把一次写入标记为豁免仓库存储配额（FR-41 §4.1）。
//
// 只允许迁移导入与备份恢复这类**管理员批量操作**调用：这类操作属可预估的批量写入，
// 配额不阻断它（计划 §6.3），但提交点会记日志。标记随资产对象流入提交点判定，
// 因此无需为各条发布路径增加旁路参数；协议发布路径不得调用本函数。
func markQuotaExempt(asset *repository.Asset) *repository.Asset {
	if asset != nil {
		asset.QuotaExempt = true
	}
	return asset
}

// RepoQuotaService 判定仓库级存储配额（FR-41 §4.1）。
//
// 判定时序分四级，本服务承担其中三级：
//  1. 已知 Content-Length 的入口调用 Admission（协议层在读取请求体之前调用）；
//  2. 流式入口用 Admission 返回的上限包成限额感知读取器做早拒；
//  3. 提交点调用 CheckCommit 做权威复检（同一事务，回滚越限写入）；
//  4. 拒绝后的暂存 blob 由既有失败路径回收（RemoveIfUnreferenced）。
//
// 并发语义：不引入预留机制。预检可能同时通过，但提交点由 SQLite 单写者串行化，
// 最多一个在途上传在提交点被拒（同一个 429），绝不出现越界的已提交状态。
type RepoQuotaService struct {
	repos   *repository.RepoRepo
	assets  *repository.AssetRepo
	metrics RejectionRecorder
}

// NewRepoQuotaService 构造仓库存储配额服务。
func NewRepoQuotaService(repos *repository.RepoRepo, assets *repository.AssetRepo) *RepoQuotaService {
	return &RepoQuotaService{repos: repos, assets: assets}
}

// SetRejectionRecorder 注入发布拒绝计数器（FR-41 §4.5）；nil 表示不累加指标。
func (s *RepoQuotaService) SetRejectionRecorder(r RejectionRecorder) { s.metrics = r }

// RecordStreamRejection 记录一次由**限额感知读取器**在流中触发的早拒（FR-41 §4.1 第 2 级）。
// 这类拒绝发生在读取器内部、没有经过 Admission/CheckCommit，本服务观察不到，
// 因此由观察到 ErrQuotaExceeded 的协议层显式上报，避免指标漏计。
// 预检与提交点的拒绝已由本服务自行累加，调用方不得对它们重复上报。
func (s *RepoQuotaService) RecordStreamRejection() { s.recordRejection() }

// Admission 判定一次发布的准入，并返回流式读取必须遵守的字节上限。
//
// 参数：
//   - repoName：仓库名（可为别名）；
//   - path：制品路径；空串表示路径尚未解析（如 multipart 尚未读出文件名）；
//   - size：已知内容长度；<0 表示长度未知（流式）。
//
// 返回 allowedBytes：>0 表示调用方必须用限额感知读取器把读取限制在该字节数内；
// 0 表示不限。越限返回 *StorageQuotaError（满足 errors.Is(err, ErrQuotaExceeded)）。
//
// 计量口径为「当前占用 + 本次净增量」，覆盖写的净增量 = 本次大小 − 被覆盖的旧大小。
func (s *RepoQuotaService) Admission(repoName, path string, size int64) (int64, error) {
	repo, limits, enabled, err := s.lookup(repoName)
	if err != nil || !enabled {
		return 0, err
	}
	count, used, err := s.assets.CountAndSizeByRepo(repo.ID)
	if err != nil {
		return 0, err
	}
	oldSize, exists, err := s.existingSize(repo.ID, path)
	if err != nil {
		return 0, err
	}
	// 制品数：只有新增路径才增件，覆盖写不改变件数。
	if limits.Assets > 0 && !exists && count+1 > limits.Assets {
		return 0, s.reject(repo.Name, limits, used, count)
	}
	if limits.Bytes == 0 {
		return 0, nil
	}
	if size >= 0 {
		// 已知长度：在读取请求体之前判定，不合格直接拒绝、不读流。
		if used-oldSize+size > limits.Bytes {
			return 0, s.reject(repo.Name, limits, used, count)
		}
		return 0, nil
	}
	if path == "" {
		// 路径未解析：无法计算覆盖写抵扣，取「绝不可能误拒合法写入」的安全上界（整仓上限）；
		// 精确判定由路径解析后的 Admission 与提交点复检负责。
		return limits.Bytes, nil
	}
	// 长度未知但路径已知：返回允许读入的最大字节数，越限即中断，不允许先写满再拒绝。
	allowed := limits.Bytes - used + oldSize
	if allowed <= 0 {
		return 0, s.reject(repo.Name, limits, used, count)
	}
	return allowed, nil
}

// CheckCommit 是提交点权威复检（FR-41 §4.1 判定时序第 3 级）：在资产写入的**同一事务内**
// 重新判定。事务视图已包含本次写入，故读到的是「当前占用 + 本次净增量」（覆盖写天然抵扣旧大小）。
//
// 判定范围与边界：
//   - 只判定**写入项**（After != nil）：删除项只会降低占用，不能被配额阻断（否则管理员调低
//     配额后连删除都被锁死）；
//   - 判定的是提交后占用 ≤ 上限，因此当仓库已被管理员调低到超限状态时，即便是"缩小体积"的
//     覆盖写也会被拒（保守：先要回到限内），这是有意的可预期语义；
//   - 标记为 QuotaExempt 的写入项（迁移导入等管理员批量操作）不参与判定，仅记日志。
func (s *RepoQuotaService) CheckCommit(tx *sqlx.Tx, items []repository.AssetMutationItem) error {
	writeIDs := make([]int64, 0, len(items))
	exemptIDs := make([]int64, 0, len(items))
	exemptItems := 0
	seenWrite := make(map[int64]bool, len(items))
	seenExempt := make(map[int64]bool, len(items))
	for i := range items {
		// 删除项只会降低占用，不能被配额阻断。
		if items[i].After == nil {
			continue
		}
		if items[i].After.QuotaExempt {
			exemptItems++
			if !seenExempt[items[i].RepositoryID] {
				seenExempt[items[i].RepositoryID] = true
				exemptIDs = append(exemptIDs, items[i].RepositoryID)
			}
			continue
		}
		if !seenWrite[items[i].RepositoryID] {
			seenWrite[items[i].RepositoryID] = true
			writeIDs = append(writeIDs, items[i].RepositoryID)
		}
	}
	if len(exemptIDs) > 0 {
		// 迁移导入与备份恢复属管理员批量操作，显式豁免配额（计划 §4.1）；仅记日志供运维追溯。
		// 只输出仓库 id 与项数，不含任何文件系统路径。
		log.Printf("仓库存储配额豁免：管理员批量写入仓库 id=%v 项数=%d", exemptIDs, exemptItems)
	}
	if len(writeIDs) == 0 {
		return nil
	}
	// 排序保证多仓越限时报错稳定，避免测试与排障受 map 遍历顺序影响。
	sort.Slice(writeIDs, func(i, j int) bool { return writeIDs[i] < writeIDs[j] })
	for _, repoID := range writeIDs {
		repo, err := s.repos.GetByIDTx(tx, repoID)
		if err != nil {
			return err
		}
		cfg, err := repo.DecodeConfig()
		if err != nil {
			return err
		}
		limits := RepoQuotaLimits{Bytes: cfg.QuotaBytes, Assets: cfg.QuotaAssets}
		if limits.Empty() {
			continue
		}
		count, used, err := s.assets.CountAndSizeByRepoTx(tx, repoID)
		if err != nil {
			return err
		}
		if limits.Assets > 0 && count > limits.Assets {
			return s.reject(repo.Name, limits, used, count)
		}
		if limits.Bytes > 0 && used > limits.Bytes {
			return s.reject(repo.Name, limits, used, count)
		}
	}
	return nil
}

// lookup 读取仓库与其配额上限。仓库不存在时不判定，交由既有 404 / 409 逻辑处理；
// 类型不是 hosted 时也不判定：group 不承载写入，proxy 的缓存写入发生在读取回源路径上
// （没有预检与早拒），且这两类仓库在配置层就已经被拒绝非 0 配额
// （repository_service.go 的 validateConfig），因此这里只是防御性兜底。
// 未配置配额（0/缺省）时 enabled=false。
func (s *RepoQuotaService) lookup(repoName string) (*repository.Repository, RepoQuotaLimits, bool, error) {
	repo, err := s.repos.GetByName(repoName)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, RepoQuotaLimits{}, false, nil
	}
	if err != nil {
		return nil, RepoQuotaLimits{}, false, err
	}
	if repo.Type != "hosted" {
		return repo, RepoQuotaLimits{}, false, nil
	}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return nil, RepoQuotaLimits{}, false, err
	}
	limits := RepoQuotaLimits{Bytes: cfg.QuotaBytes, Assets: cfg.QuotaAssets}
	return repo, limits, !limits.Empty(), nil
}

// existingSize 返回路径上既有制品的大小与是否存在；path 为空视为不存在。
func (s *RepoQuotaService) existingSize(repoID int64, path string) (int64, bool, error) {
	if path == "" {
		return 0, false, nil
	}
	asset, err := s.assets.GetByPath(repoID, path)
	if errors.Is(err, repository.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return asset.Size, true, nil
}

// reject 累加拒绝计数并构造统一的配额超限错误。
func (s *RepoQuotaService) reject(repoName string, limits RepoQuotaLimits, usedBytes, usedAssets int64) error {
	s.recordRejection()
	return &StorageQuotaError{Repository: repoName, Limits: limits, UsedBytes: usedBytes, UsedAssets: usedAssets}
}

// recordRejection 累加一次发布拒绝计数（原因取自闭集枚举）。
func (s *RepoQuotaService) recordRejection() {
	if s.metrics != nil {
		// 取值来自闭集枚举（v1 只有 quota）；新增取值必须先改规格。
		s.metrics.PublishRejection(RejectionReasonQuota)
	}
}
