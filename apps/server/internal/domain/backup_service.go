package domain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 备份服务错误。这些错误会被 API 层映射为明确状态码，不做模糊化处理。
var (
	ErrBackupNotFound    = errors.New("备份包不存在")
	ErrBackupInProgress  = errors.New("已有备份生成任务在进行中")
	ErrBackupHasDerived  = errors.New("该备份包仍被增量包引用，不能删除")
	ErrBackupIncomplete  = errors.New("备份包尚未生成完成")
	ErrBackupFileMissing = errors.New("备份包文件缺失")
	// ErrBackupBaseUnavailable 表示请求以某基线包生成差包，但基线不可用
	// （不存在 / 未完成 / 侧车索引缺失），无法求差。信息中带基线 id 与原因。
	ErrBackupBaseUnavailable = errors.New("基线包不可用，无法生成增量差包")
)

// backupCountTables 是包内计数的表白名单，顺序固定以产出稳定结果。
// user_group / user_group_member 是 FR-36 的授权元数据（迁移 0046）：它们与 acl 一样决定
// 「谁能访问什么」，备份包漏掉它们会让恢复后的节点丢失组授权，故一并计入。
var backupCountTables = []string{"user", "api_token", "repository", "acl", "asset", "format_metadata", "user_group", "user_group_member"}

// CreateBackupOptions 控制一次备份生成。
type CreateBackupOptions struct {
	// Mode 为 hot（热备份，不停服）或 frozen（冻结窗口）。
	Mode archive.Mode
	// Label 是便于人识别的备注，可为空。
	Label string
	// Progress 可选：每写入一个 blob 回调一次。
	Progress archive.ProgressFunc
	// BasePackageID 非空时生成增量差包：只携带相对该基线包的新增 blob。
	// 要求基线包存在、状态 done 且其侧车索引可用（否则 ErrBackupBaseUnavailable）。
	BasePackageID string
	// Origin 是生成来源（FR-40）：空值等价于 repository.BackupOriginManual，
	// 自动备份作业传 repository.BackupOriginAuto。该标记决定保留策略是否可淘汰本包。
	Origin string
}

// BackupService 负责节点备份包的生成、登记、校验与下载。
//
// 包体落盘在 ${JIAN_DATA_DIR}/backups/<package_id>.tar.gz，登记元数据在 backup_package 表。
// 生成过程串行化：同一时刻只允许一个生成任务，避免快照与磁盘写入互相挤压。
type BackupService struct {
	db         *persistence.DB
	repo       *repository.BackupPackageRepo
	blobs      *blobstore.Store
	dataDir    string
	dbPath     string
	appVersion string
	nodeID     func() string
	now        func() time.Time

	mu         sync.Mutex
	generating bool
	// diffMu 串行化"挂载包内快照 → 查询 → 卸载"这一段。
	//
	// ATTACH 是**连接级**状态：连接池上限为 1，但同一连接上仍可能被并发 goroutine
	// 交错使用（sqlx 会复用），一旦两个请求交错 ATTACH/DETACH，
	// 后一个 DETACH 会把前一个正在用的库卸掉。用锁把整段包住最直白也最安全。
	diffMu sync.Mutex
}

// NewBackupService 装配备份服务。nodeID 允许为 nil（单节点场景）。
func NewBackupService(
	db *persistence.DB,
	repo *repository.BackupPackageRepo,
	blobs *blobstore.Store,
	dataDir, dbPath, appVersion string,
	nodeID func() string,
) *BackupService {
	if nodeID == nil {
		nodeID = func() string { return "" }
	}
	return &BackupService{
		db:         db,
		repo:       repo,
		blobs:      blobs,
		dataDir:    dataDir,
		dbPath:     dbPath,
		appVersion: appVersion,
		nodeID:     nodeID,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// Dir 是备份包目录。
func (s *BackupService) Dir() string { return filepath.Join(s.dataDir, "backups") }

// PackagePath 是某备份包的归档路径。
func (s *BackupService) PackagePath(packageID string) string {
	return filepath.Join(s.Dir(), packageID+".tar.gz")
}

// SidecarPath 是某备份包的侧车索引路径：与包同目录、固定后缀 .index，
// 格式与包内 blobs.index 一致（确定性排序的 "<sha256> <size>" 行）。
//
// 全量包的侧车 = 该包完整 blob 集合；差包的侧车 = 应用该差包后的完整集合（并集）。
// 侧车把"算基线 blob 集合"从 O(基线归档大小) 降到 O(索引大小)；基线侧车缺失时
// 生成差包必须明确报错，不得静默退化成扫描整包。
func (s *BackupService) SidecarPath(packageID string) string {
	return filepath.Join(s.Dir(), packageID+".index")
}

// readSidecar 读取某包已写入的侧车索引。
func (s *BackupService) readSidecar(packageID string) (archive.BlobIndex, error) {
	f, err := os.Open(s.SidecarPath(packageID))
	if err != nil {
		return nil, fmt.Errorf("打开侧车索引 %s：%w", packageID, err)
	}
	defer func() { _ = f.Close() }()
	idx, err := archive.ParseBlobIndex(f)
	if err != nil {
		return nil, fmt.Errorf("解析侧车索引 %s：%w", packageID, err)
	}
	return idx, nil
}

// writeSidecar 把完整 blob 集合写入侧车索引。
func (s *BackupService) writeSidecar(packageID string, idx archive.BlobIndex) error {
	if err := os.WriteFile(s.SidecarPath(packageID), idx.Render(), 0o600); err != nil {
		return fmt.Errorf("写侧车索引 %s：%w", packageID, err)
	}
	return nil
}

// List 按最近优先返回登记记录。
func (s *BackupService) List(limit, offset int) ([]repository.BackupPackage, error) {
	return s.repo.List(limit, offset)
}

// Count 返回备份包总数。
func (s *BackupService) Count() (int, error) { return s.repo.Count() }

// Get 读取单个登记记录。
func (s *BackupService) Get(packageID string) (*repository.BackupPackage, error) {
	rec, err := s.repo.Get(packageID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrBackupNotFound
	}
	return rec, err
}

// OpenReader 打开已完成的备份包供下载或进一步校验。
func (s *BackupService) OpenReader(packageID string) (*archive.Reader, archive.Manifest, error) {
	rec, err := s.Get(packageID)
	if err != nil {
		return nil, archive.Manifest{}, err
	}
	if rec.Status != repository.BackupStatusDone {
		return nil, archive.Manifest{}, ErrBackupIncomplete
	}
	path := s.PackagePath(packageID)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, archive.Manifest{}, ErrBackupFileMissing
		}
		return nil, archive.Manifest{}, fmt.Errorf("读取备份包 %s：%w", packageID, err)
	}
	r, m, err := archive.Open(path)
	if err != nil {
		return nil, archive.Manifest{}, err
	}
	return r, m, nil
}

// Verify 校验备份包完整性；deep 为 true 时逐 blob 比对内容摘要。
func (s *BackupService) Verify(packageID string, deep bool) (archive.Manifest, error) {
	r, m, err := s.OpenReader(packageID)
	if err != nil {
		return archive.Manifest{}, err
	}
	if err := r.Verify(deep); err != nil {
		return m, err
	}
	return m, nil
}

// Delete 删除备份包文件、侧车索引与登记。
//
// 仍被增量差包引用的基线不允许删除：否则派生包在目标端永远无法还原。
//
// **侧车索引必须与包体同生共死**：它只服务于"为后续差包求差"，包一旦不存在，
// 侧车就成了无人引用的孤儿文件。保留策略是周期性淘汰（FR-40），漏删侧车会让
// `${JIAN_DATA_DIR}/backups/` 里的索引只增不减——包体被淘汰而索引永久残留，
// 与"保留策略兜住磁盘占用"的设计目的直接冲突。
func (s *BackupService) Delete(packageID string) error {
	if _, err := s.Get(packageID); err != nil {
		return err
	}
	derived, err := s.repo.HasDerived(packageID)
	if err != nil {
		return err
	}
	if derived {
		return ErrBackupHasDerived
	}
	if err := os.Remove(s.PackagePath(packageID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除备份包文件：%w", err)
	}
	// 侧车缺失不算错：早期版本删除时未清理侧车，现存实例可能本就没有它；
	// 这里的目的是"删干净"，不是"断言它一定存在"。
	if err := os.Remove(s.SidecarPath(packageID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除备份包侧车索引：%w", err)
	}
	return s.repo.Delete(packageID)
}

// ReconcileStartup 收口进程重启遗留的生成中包，不让列表里留下永久"生成中"幻影：
//   - 归档完整可读（manifest 身份正确）→ 判为 done。还原进来的包正是这种形态：它的内嵌
//     快照取自生成过程中，登记行仍是生成中，但包体在本节点可用；误标 failed 会让目标节点
//     无法基于它继续生成增量差包（FR-134 要求基线为 done）。
//   - 归档缺失或不可读 → 判为 failed（真中断）。
func (s *BackupService) ReconcileStartup() (int64, error) {
	const pageSize = 200
	var recovered int64
	for offset := 0; ; offset += pageSize {
		rows, err := s.repo.List(pageSize, offset)
		if err != nil {
			return recovered, fmt.Errorf("列出备份包：%w", err)
		}
		for _, rec := range rows {
			if !isBackupInFlight(rec.Status) {
				continue
			}
			manifest, ok := s.readPackageManifest(rec.PackageID)
			if !ok {
				continue
			}
			counts, err := json.Marshal(manifest.Counts)
			if err != nil {
				continue
			}
			var size int64
			if info, err := os.Stat(s.PackagePath(rec.PackageID)); err == nil {
				size = info.Size()
			}
			if err := s.repo.Finish(rec.PackageID, true, size, string(counts), ""); err != nil {
				continue
			}
			recovered++
		}
		if len(rows) < pageSize {
			break
		}
	}
	n, err := s.repo.MarkInterrupted("服务重启，备份生成被中断")
	if err != nil {
		return recovered, err
	}
	return recovered + n, nil
}

// isBackupInFlight 判断登记状态是否属于"生成中"（与 MarkInterrupted 的口径一致）。
func isBackupInFlight(status string) bool {
	return status == repository.BackupStatusQueued ||
		status == repository.BackupStatusSnapshotting ||
		status == repository.BackupStatusPacking
}

// readPackageManifest 读取归档内的 manifest；归档缺失 / 半截 / 非本产品包时返回 false。
func (s *BackupService) readPackageManifest(packageID string) (archive.Manifest, bool) {
	path := s.PackagePath(packageID)
	if _, err := os.Stat(path); err != nil {
		return archive.Manifest{}, false
	}
	r, m, err := archive.Open(path)
	if err != nil {
		return archive.Manifest{}, false
	}
	_ = r
	if m.PackageID == "" || m.Kind != archive.Kind {
		return archive.Manifest{}, false
	}
	return m, true
}

// readPackageFile 读取磁盘上某**文件标识**对应的归档 manifest 并做完整校验。
//
// 与 readPackageManifest 的区别：后者按已登记的 packageID 定位，只做轻量身份判定；
// 本方法服务于「未管理包」（尚无登记，只有文件名），因此必须走 archive.Manifest.Validate，
// 把缺字段、版本不符、模式未知的包挡在登记之外——否则一条坏登记会让校验/演练/恢复
// 在更晚的环节才失败，且错误信息远离根因。
func (s *BackupService) readPackageFile(fileID string) (archive.Manifest, bool) {
	path := filepath.Join(s.Dir(), fileID+".tar.gz")
	if _, err := os.Stat(path); err != nil {
		return archive.Manifest{}, false
	}
	_, m, err := archive.Open(path)
	if err != nil {
		return archive.Manifest{}, false
	}
	if err := m.Validate(); err != nil {
		return archive.Manifest{}, false
	}
	return m, true
}

// readPackageBlobIndex 从归档内 blobs.index 读回完整 blob 集合，用于补写侧车。
func (s *BackupService) readPackageBlobIndex(packageID string) (archive.BlobIndex, error) {
	r, _, err := s.OpenReader(packageID)
	if err != nil {
		return nil, err
	}
	return r.BlobIndex()
}

// Create 登记并启动后台生成，立即返回 queued 记录。
//
// 生成耗时与包体积同阶（大仓库为分钟级），不能占用 HTTP 请求线程；调用方轮询列表
// 观察进度。生成在 WithCancel 隔离的上下文里继续，请求中断不会打断已开始的打包。
func (s *BackupService) Create(ctx context.Context, opts CreateBackupOptions) (repository.BackupPackage, error) {
	rec, err := s.prepare(opts, repository.BackupStatusQueued)
	if err != nil {
		return rec, err
	}
	go func() {
		defer s.endGenerate()
		if _, err := s.run(context.WithoutCancel(ctx), rec, opts); err != nil {
			log.Printf("备份：包 %s 生成失败：%v", rec.PackageID, err)
		}
	}()
	return rec, nil
}

// Generate 同步生成一个全量备份包（供 CLI 与测试使用）。
//
// 步骤：一致性快照 → 按快照 asset 表枚举 blob → 流式打包 → 收尾登记。
// 任何一步失败都会把登记置为 failed 并清理半成品文件，绝不留下可被误用的残包。
func (s *BackupService) Generate(ctx context.Context, opts CreateBackupOptions) (repository.BackupPackage, error) {
	rec, err := s.prepare(opts, repository.BackupStatusSnapshotting)
	if err != nil {
		return rec, err
	}
	defer s.endGenerate()
	return s.run(ctx, rec, opts)
}

// prepare 校验模式、占据生成槽位并登记记录。成功后调用方必须配一次 endGenerate。
func (s *BackupService) prepare(opts CreateBackupOptions, status string) (repository.BackupPackage, error) {
	if opts.Mode != archive.ModeHot && opts.Mode != archive.ModeFrozen {
		return repository.BackupPackage{}, fmt.Errorf("%w：未知备份模式 %q", ErrValidation, opts.Mode)
	}
	// 增量差包必须先确认基线可用：存在、done、侧车索引就绪，否则无法求差。
	if opts.BasePackageID != "" {
		if err := s.validateBase(opts.BasePackageID); err != nil {
			return repository.BackupPackage{}, err
		}
	}
	if !s.beginGenerate() {
		return repository.BackupPackage{}, ErrBackupInProgress
	}
	rec := repository.BackupPackage{
		PackageID:       newPackageID(s.now()),
		Mode:            string(opts.Mode),
		Status:          status,
		Label:           opts.Label,
		BasePackageID:   opts.BasePackageID,
		NodeID:          s.nodeID(),
		AppVersion:      s.appVersion,
		CountsJSON:      "{}",
		DBSchemaVersion: s.dbSchemaVersion(),
		CreatedAt:       s.now().Format(time.RFC3339Nano),
		Origin:          normalizeBackupOrigin(opts.Origin),
	}
	if err := os.MkdirAll(s.Dir(), 0o750); err != nil {
		s.endGenerate()
		return rec, fmt.Errorf("创建备份目录：%w", err)
	}
	if err := s.repo.Create(rec); err != nil {
		s.endGenerate()
		return rec, fmt.Errorf("登记备份包：%w", err)
	}
	return rec, nil
}

// validateBase 确认以 baseID 为基线生成差包的条件全部满足：
// 基线包存在、状态 done、且其侧车索引可用。任一不满足返回 ErrBackupBaseUnavailable，
// 信息中带基线 id 与原因——侧车缺失时不退化成扫描整包，而是明确报错。
func (s *BackupService) validateBase(baseID string) error {
	rec, err := s.Get(baseID)
	if err != nil {
		if errors.Is(err, ErrBackupNotFound) {
			return fmt.Errorf("%w：基线包 %s 不存在", ErrBackupBaseUnavailable, baseID)
		}
		return fmt.Errorf("%w：查询基线包 %s：%v", ErrBackupBaseUnavailable, baseID, err)
	}
	if rec.Status != repository.BackupStatusDone {
		return fmt.Errorf("%w：基线包 %s 状态为 %s（需 done）", ErrBackupBaseUnavailable, baseID, rec.Status)
	}
	if _, err := os.Stat(s.SidecarPath(baseID)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w：基线包 %s 的侧车索引缺失，无法求差（请重新生成基线包）", ErrBackupBaseUnavailable, baseID)
		}
		return fmt.Errorf("%w：读取基线包 %s 侧车索引：%v", ErrBackupBaseUnavailable, baseID, err)
	}
	return nil
}

// run 执行快照与打包，并负责收尾登记与半成品清理。
func (s *BackupService) run(ctx context.Context, rec repository.BackupPackage, opts CreateBackupOptions) (out repository.BackupPackage, err error) {
	packageID := rec.PackageID

	// tempSnapshot 与包体同目录（同一文件系统），失败时一并清理。
	tempSnapshot := s.PackagePath(packageID) + ".snap"
	committed := false
	defer func() {
		_ = os.Remove(tempSnapshot)
		if !committed {
			_ = os.Remove(s.PackagePath(packageID))
			// 侧车索引与包体同生共死：生成未提交（失败或半成品）必须一并清理，
			// 否则残留侧车会让后续派生差包误判基线可用。
			_ = os.Remove(s.SidecarPath(packageID))
			// 必须改命名返回值而不是参数 rec：return 语句已先把 out 赋好，
			// 只改 rec 会让调用方拿到与库内登记不一致的旧状态。
			out.Status = repository.BackupStatusFailed
			out.ErrorSummary = errText(err)
			_ = s.repo.Finish(packageID, false, 0, out.CountsJSON, out.ErrorSummary)
		}
	}()

	if err := ctx.Err(); err != nil {
		return rec, err
	}
	if err := persistence.SnapshotFile(s.dbPath, tempSnapshot); err != nil {
		return rec, fmt.Errorf("生成一致性快照：%w", err)
	}

	blobRefs, err := persistence.AssetBlobs(tempSnapshot)
	if err != nil {
		return rec, err
	}
	refs := make([]archive.BlobRef, 0, len(blobRefs))
	for hash, size := range blobRefs {
		refs = append(refs, archive.BlobRef{Hash: hash, Size: size})
	}
	// map 遍历无序，规范化以保证 manifest 摘要稳定。
	sort.Slice(refs, func(i, j int) bool { return refs[i].Hash < refs[j].Hash })

	// current 是当前快照的完整 blob 集合（确定性排序）。差包与全量包都以它为基准
	// 计算侧车（应用后的完整集合），差包再从中减去基线集合得到包体差集。
	current := archive.NewBlobIndex(refs)

	counts, err := s.collectCounts(tempSnapshot)
	if err != nil {
		return rec, err
	}
	if err := s.repo.UpdateProgress(packageID, repository.BackupStatusPacking, 0, counts); err != nil {
		return rec, fmt.Errorf("更新生成进度：%w", err)
	}

	// 包体携带的 blob 集合：全量包 = 完整集合；差包 = 相对基线的新增集合。
	bodyRefs := current
	manifest := archive.Manifest{
		SchemaVersion:   archive.SchemaVersion,
		Kind:            archive.Kind,
		PackageID:       packageID,
		Mode:            opts.Mode,
		CreatedAt:       s.now(),
		NodeID:          rec.NodeID,
		AppVersion:      s.appVersion,
		DBSchemaVersion: rec.DBSchemaVersion,
		Counts:          parseCounts(counts),
	}
	if opts.BasePackageID != "" {
		baseIdx, err := s.readSidecar(opts.BasePackageID)
		if err != nil {
			return rec, fmt.Errorf("读取基线侧车 %s：%w", opts.BasePackageID, err)
		}
		diff := archive.BlobSetDiff(baseIdx, current)
		bodyRefs = diff
		// Expected = 应用该差包后的完整集合摘要（并集），供导入端判断基线是否就位。
		expected := current.Summary()
		manifest.BasePackageID = opts.BasePackageID
		manifest.Expected = &expected
		// 运维要能一眼看出省了多少传输：差集规模、新增字节、相对基线的比例。
		baseTotal := current.TotalBytes()
		deltaTotal := bodyRefs.TotalBytes()
		saved := baseTotal - deltaTotal
		savedPct := 0.0
		if baseTotal > 0 {
			savedPct = float64(saved) / float64(baseTotal) * 100
		}
		log.Printf("备份：差包 %s 基于 %s：差集 blob %d（%d 字节）/ 基线 %d 字节，少传 %.1f%%",
			packageID, opts.BasePackageID, len(bodyRefs), deltaTotal, baseTotal, savedPct)
	}

	if _, err = archive.WriteBundle(s.PackagePath(packageID), manifest, tempSnapshot, bodyRefs, s.blobs, opts.Progress); err != nil {
		return rec, fmt.Errorf("打包备份：%w", err)
	}

	// 写侧车索引（完整 blob 集合）。全量包写完整集合，差包写并集——差包之上可再派生差包。
	// 必须早于 committed：侧车缺失会让后续派生差包误判基线可用。
	if err := s.writeSidecar(packageID, current); err != nil {
		return rec, err
	}

	info, err := os.Stat(s.PackagePath(packageID))
	if err != nil {
		return rec, fmt.Errorf("读取备份包大小：%w", err)
	}
	if err := s.repo.Finish(packageID, true, info.Size(), counts, ""); err != nil {
		return rec, fmt.Errorf("收尾备份登记：%w", err)
	}
	committed = true

	rec.Status = repository.BackupStatusDone
	rec.SizeBytes = info.Size()
	rec.CountsJSON = counts
	finish := s.now().Format(time.RFC3339Nano)
	rec.FinishedAt = &finish
	return rec, nil
}

func (s *BackupService) collectCounts(snapshotPath string) (string, error) {
	raw, err := persistence.RowCounts(snapshotPath, backupCountTables)
	if err != nil {
		return "", err
	}
	counts := archive.Counts{
		Users:          raw["user"],
		Tokens:         raw["api_token"],
		Repositories:   raw["repository"],
		ACLs:           raw["acl"],
		Assets:         raw["asset"],
		FormatMetadata: raw["format_metadata"],
	}
	data, err := json.Marshal(counts)
	if err != nil {
		return "", fmt.Errorf("序列化计数：%w", err)
	}
	return string(data), nil
}

func (s *BackupService) dbSchemaVersion() int {
	version, err := s.db.CurrentVersion()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(version)
	if err != nil {
		return 0
	}
	return n
}

func (s *BackupService) beginGenerate() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generating {
		return false
	}
	s.generating = true
	return true
}

func (s *BackupService) endGenerate() {
	s.mu.Lock()
	s.generating = false
	s.mu.Unlock()
}

// newPackageID 生成形如 bk-20260910-162701-a1b2c3 的包标识。
func newPackageID(now time.Time) string {
	var raw [3]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// 熵源不可用时退化为时间戳后缀，仍保证可读与基本唯一。
		return fmt.Sprintf("bk-%s-%06d", now.Format("20060102-150405"), now.Nanosecond()/1000%1000000)
	}
	return fmt.Sprintf("bk-%s-%s", now.Format("20060102-150405"), hex.EncodeToString(raw[:]))
}

func parseCounts(countsJSON string) archive.Counts {
	var c archive.Counts
	if err := json.Unmarshal([]byte(countsJSON), &c); err != nil {
		return archive.Counts{}
	}
	return c
}

// normalizeBackupOrigin 把选项里的来源收敛到列 CHECK 允许的两个取值：
// 空值（以及任何未知值）一律落到 manual。
//
// 为什么未知值不报错而是回落：来源只影响「能否被保留策略自动淘汰」，落成 manual 的后果
// 是"这份包被保守地多留一份"，而报错会让一次正常的备份生成整体失败。宁可多留，不可失败。
func normalizeBackupOrigin(origin string) string {
	if origin == repository.BackupOriginAuto {
		return repository.BackupOriginAuto
	}
	return repository.BackupOriginManual
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
