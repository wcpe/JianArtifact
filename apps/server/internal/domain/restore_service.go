package domain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 恢复导入错误。导入端据此向调用方给出明确的失败语义（而非泛化的 500）。
var (
	// ErrRestoreTargetNotEmpty 表示目标实例非空，导入将覆盖现有数据，需显式 --overwrite。
	ErrRestoreTargetNotEmpty = errors.New("目标实例非空，导入将覆盖现有数据")
	// ErrRestoreIncompatible 表示备份包所需数据库版本高于本程序（本地程序太旧）。
	ErrRestoreIncompatible = errors.New("备份包所需数据库版本高于本程序，请先升级")
	// ErrRestorePending 表示已存在待生效的恢复，不允许叠加第二次。
	ErrRestorePending = errors.New("已存在待生效的备份恢复，请先重启服务完成替换")
	// ErrRestoreIncrementalUnsupported 表示暂不支持增量差包导入。
	ErrRestoreIncrementalUnsupported = errors.New("增量包导入尚未支持，请先导入基线包")
	// ErrRestoreBaseMissing 表示差包所依赖的基线未就位（目标缺少基线包应有的 blob），
	// 必须先导入基线包。信息中写明缺失的基线 id，且不落任何文件、不留标记。
	ErrRestoreBaseMissing = errors.New("缺少基线包，无法导入增量差包")
	// ErrRestoreTooLarge 表示备份包声明的规模超出护栏（防解压炸弹 / 磁盘打满）。
	ErrRestoreTooLarge = errors.New("备份包声明的规模超出上限")
	// ErrRestoreInProgress 表示已有导入正在进行（暂存目录与标记是单例资源）。
	ErrRestoreInProgress = errors.New("已有导入正在进行")
)

// 导入规模护栏。归档是不可信输入（可能来自 URL 拉取或分片上传），
// tar.gz 可声明并产出远超自身大小的内容；archive.Reader.Verify 只保证
// "内容与声明一致"，**不限制声明本身有多大**，因此必须在任何磁盘写入前设上限。
const (
	// maxRestoreDBBytes 限制单个快照的声明大小。
	maxRestoreDBBytes = int64(8) << 30
	// maxRestoreBlobBytes 限制 blob 集合的声明总字节数。
	maxRestoreBlobBytes = int64(50) << 30
	// maxRestoreBlobCount 限制 blob 集合的声明条目数。
	maxRestoreBlobCount = 5_000_000
)

// 磁盘布局常量（相对于 ${JIAN_DATA_DIR}）。
const (
	// restoreStagingDirName 是解包暂存目录；校验通过后等待重启替换。
	restoreStagingDirName = "restore-staging"
	// restorePendingFile 是待生效标记；存在则下次启动执行数据库替换。
	restorePendingFile = "restore.pending"
)

// RestoreRequest 描述一次恢复导入请求。
type RestoreRequest struct {
	// SourcePath 是归档文件路径（CLI 直传 / Web 拉取落盘 / 分片上传合并后的路径）。
	SourcePath string
	// Origin 标识导入通道：cli / upload / url。
	Origin string
	// Operator 是发起恢复的操作者标识（仅供审计与标记记录）。
	Operator string
	// Overwrite 允许覆盖非空目标实例。
	Overwrite bool
	// Deep 在解包前逐 blob 比对内容摘要（耗时与包同阶）。
	Deep bool
}

// RestoreResult 是一次暂存导入的结果（尚未替换数据库）。
type RestoreResult struct {
	ImportID    string
	Manifest    archive.Manifest
	BlobMerged  int
	BlobSkipped int
	Staged      bool
	PendingPath string
}

// PendingRestore 是 restore.pending 标记文件的结构，供启动期读取并用于回滚信息。
type PendingRestore struct {
	ImportID             string         `json:"importId"`
	PackageID            string         `json:"packageId"`
	Origin               string         `json:"origin"`
	Operator             string         `json:"operator"`
	StagedAt             time.Time      `json:"stagedAt"`
	StagedDBRelativePath string         `json:"stagedDbRelativePath"` // 相对 dataDir，供启动期定位暂存 db
	DBSchemaVersion      int            `json:"dbSchemaVersion"`
	Counts               archive.Counts `json:"counts"`
	BlobCount            int            `json:"blobCount"`
	BlobTotalBytes       int64          `json:"blobTotalBytes"`
	BlobMerged           int            `json:"blobMerged"`
	BlobSkipped          int            `json:"blobSkipped"`
}

// RestoreService 负责节点备份包的导入编排：校验 → 暂存 → 合并 blob → 写待生效标记。
//
// 数据库文件的替换不在本服务内完成（运行中无法安全替换 SQLite），由启动期的
// ApplyPendingRestore 在 persistence.Open 之前原子完成。blob 因内容寻址且原子落盘，
// 可在运行中直接合并进目标 blobstore，无需重启。
type RestoreService struct {
	db          *persistence.DB
	dataDir     string
	dbPath      string
	blobs       *blobstore.Store
	stagingDir  string
	pendingPath string
	now         func() time.Time

	// 串行化导入：暂存目录与待生效标记都是单例资源，两个并发导入（例如
	// HTTP 导入与 CLI 同时发起）会互相踩踏，必须互斥。
	mu        sync.Mutex
	importing bool
}

// NewRestoreService 装配恢复导入服务。blob 存储按 blobDir 内部构造（内容寻址、原子落盘）。
func NewRestoreService(db *persistence.DB, dataDir, dbPath, blobDir string) *RestoreService {
	return &RestoreService{
		db:          db,
		dataDir:     dataDir,
		dbPath:      dbPath,
		blobs:       blobstore.NewStore(blobDir),
		stagingDir:  filepath.Join(dataDir, restoreStagingDirName),
		pendingPath: filepath.Join(dataDir, restorePendingFile),
		now:         func() time.Time { return time.Now().UTC() },
	}
}

// StagingDir 返回暂存目录绝对路径（供测试与诊断）。
func (s *RestoreService) StagingDir() string { return s.stagingDir }

// PendingPath 返回待生效标记文件路径。
func (s *RestoreService) PendingPath() string { return s.pendingPath }

// MarkedPending 读取并解析待生效标记；不存在返回 (nil, nil)，损坏返回错误。
func (s *RestoreService) MarkedPending() (*PendingRestore, error) {
	return readPendingFile(s.dataDir)
}

// TargetNonEmpty 返回目标实例是否非空。
//
// 判据**排除内置 anonymous 主体**：迁移 0007_anonymous_setting.sql 会无条件植入它，
// 若把 user 表原始行数算进去，任何全新实例都会被判为「非空」，迫使主用例
// （把包导入到一台新机器）也要加 --overwrite —— 那样这个护栏就退化成噪音了。
func (s *RestoreService) TargetNonEmpty() (bool, error) {
	var occ struct {
		Users        int `db:"users"`
		Repositories int `db:"repositories"`
		Assets       int `db:"assets"`
	}
	err := s.db.Get(&occ, `
		SELECT (SELECT COUNT(*) FROM user       WHERE username <> ?) AS users,
		       (SELECT COUNT(*) FROM repository)                     AS repositories,
		       (SELECT COUNT(*) FROM asset)                          AS assets`,
		AnonymousUsername)
	if err != nil {
		return false, fmt.Errorf("读取目标占用：%w", err)
	}
	return occ.Users > 0 || occ.Repositories > 0 || occ.Assets > 0, nil
}

// checkRestoreSizeLimits 在任何磁盘写入前校验归档声明的规模。
// 返回的错误带上是哪个维度超限、声明值多少、上限多少，便于运维判断
// 「包确实很大」还是「包可疑」——两者的处置完全不同。
func checkRestoreSizeLimits(manifest archive.Manifest) error {
	switch {
	case manifest.DB.SizeBytes <= 0:
		return fmt.Errorf("%w：快照声明大小不合理（%d 字节）", ErrRestoreTooLarge, manifest.DB.SizeBytes)
	case manifest.DB.SizeBytes > maxRestoreDBBytes:
		return fmt.Errorf("%w：快照声明 %d 字节，上限 %d", ErrRestoreTooLarge, manifest.DB.SizeBytes, maxRestoreDBBytes)
	case manifest.Blobs.TotalBytes > maxRestoreBlobBytes:
		return fmt.Errorf("%w：blob 总量声明 %d 字节，上限 %d", ErrRestoreTooLarge, manifest.Blobs.TotalBytes, maxRestoreBlobBytes)
	case manifest.Blobs.Count > maxRestoreBlobCount:
		return fmt.Errorf("%w：blob 条目声明 %d 个，上限 %d", ErrRestoreTooLarge, manifest.Blobs.Count, maxRestoreBlobCount)
	}
	return nil
}

func (s *RestoreService) beginImport() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.importing {
		return false
	}
	s.importing = true
	return true
}

func (s *RestoreService) endImport() {
	s.mu.Lock()
	s.importing = false
	s.mu.Unlock()
}

// Stage 校验并暂存一次恢复导入。任何失败路径都不留半成品（已清理暂存目录与标记）。
//
// 顺序（严格要求）：
//  1. 打开归档并校验 manifest（含 Validate）；增量包直接拒绝，不落任何文件。
//  2. 版本兼容与规模护栏：包 dbSchemaVersion 高于本地则拒绝；声明的 db/blob 规模超上限则拒绝。
//  3. 目标非空（排除内置 anonymous 主体）且未 Overwrite 则拒绝。
//  4. 已存在待生效标记则拒绝叠加。
//  5. 校验归档完整性（deep 控制强度）；不通过不落文件。
//  6. 清空重建暂存目录并解包；blob 已在本地（asset 表或 blobstore）则跳过，否则合并。
//  7. 全部 blob 合并成功后才写标记文件。
func (s *RestoreService) Stage(ctx context.Context, req RestoreRequest) (RestoreResult, error) {
	// 0. 串行化：暂存目录与待生效标记是单例资源，并发导入会互相踩踏。
	if !s.beginImport() {
		return RestoreResult{}, ErrRestoreInProgress
	}
	defer s.endImport()

	// 1. 打开归档 + 校验 manifest。archive.Open 内的 UnmarshalManifest 已含 Validate。
	reader, manifest, err := archive.Open(req.SourcePath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("打开备份包：%w", err)
	}
	// 增量差包：从"一律拒绝"改为"有条件支持"。
	//   - BasePackageID 非空且 Expected 为空 → 形如手工构造的畸形差包，仍拒绝（保旧用例通过）。
	//   - BasePackageID 非空且 Expected 非空 → 用本地 asset 表集合 ∪ 包内索引，与 Expected 比对：
	//       一致则基线已就位，按既有流程继续（Extract 只合并差集 blob，已存在的自然跳过）；
	//       不一致则基线缺失，报错且绝不落文件/标记。
	// 校验在任何磁盘写入之前完成（见下），因此这里只是读归档与本地库，安全。
	if manifest.BasePackageID != "" {
		if manifest.Expected == nil {
			return RestoreResult{}, fmt.Errorf("%w：请先导入基线包（差包缺少 expected 摘要）", ErrRestoreIncrementalUnsupported)
		}
		localIdx, err := s.localBlobIndex()
		if err != nil {
			return RestoreResult{}, err
		}
		pkgIdx, err := reader.BlobIndex()
		if err != nil {
			return RestoreResult{}, fmt.Errorf("读取包内索引：%w", err)
		}
		union := localIdx.Union(pkgIdx)
		if union.Summary() != *manifest.Expected {
			return RestoreResult{}, fmt.Errorf("%w：基线包 %s 未就位（本地 blob 集合与基线差包期望不一致），请先导入基线包 %s",
				ErrRestoreBaseMissing, manifest.BasePackageID, manifest.BasePackageID)
		}
		// 基线就位，继续既有流程：Extract 只会携带差集 blob，已存在于本地/ blobstore 的自然跳过。
	}

	// 2. 版本兼容与规模护栏：本地程序太旧则拒绝；声明规模超上限则拒绝（防解压炸弹 / 磁盘打满）。
	localVersion := s.dbSchemaVersion()
	if manifest.DBSchemaVersion > localVersion {
		return RestoreResult{}, fmt.Errorf("%w（包 %d / 本地 %d）", ErrRestoreIncompatible, manifest.DBSchemaVersion, localVersion)
	}
	if err := checkRestoreSizeLimits(manifest); err != nil {
		return RestoreResult{}, err
	}

	// 3. 目标非空判定：user / repository / asset 任一非空即视为非空；
	//    内置 anonymous 主体已通过 TargetNonEmpty 排除，全新实例不会被判为非空。
	targetNonEmpty, err := s.TargetNonEmpty()
	if err != nil {
		return RestoreResult{}, err
	}
	if targetNonEmpty && !req.Overwrite {
		return RestoreResult{}, fmt.Errorf("%w：请用 --overwrite 覆盖", ErrRestoreTargetNotEmpty)
	}

	// 4. 已存在待生效标记：不允许叠加两次。
	if existing, err := readPendingFile(s.dataDir); err == nil && existing != nil {
		return RestoreResult{}, fmt.Errorf("%w（包 %s 待生效）", ErrRestorePending, existing.PackageID)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return RestoreResult{}, fmt.Errorf("读取待生效标记：%w", err)
	}

	// 5. 校验归档完整性（不通过不落任何文件）。
	if err := reader.Verify(req.Deep); err != nil {
		return RestoreResult{}, fmt.Errorf("校验备份包：%w", err)
	}

	// 6. 取一次本地 blob 集合（asset 表），避免每个回调重查。
	localSet, err := persistence.AssetBlobs(s.dbPath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("读取本地 blob 集合：%w", err)
	}

	importID := newImportID(s.now())
	// 清空并重建暂存目录，保证干净状态（也顺带清掉崩在中间的陈旧暂存）。
	if err := os.RemoveAll(s.stagingDir); err != nil {
		return RestoreResult{}, fmt.Errorf("清理暂存目录：%w", err)
	}
	if err := os.MkdirAll(s.stagingDir, 0o750); err != nil {
		return RestoreResult{}, fmt.Errorf("创建暂存目录：%w", err)
	}

	var merged, skipped int
	extractErr := reader.Extract(s.stagingDir, func(ref archive.BlobRef, size int64, rc io.Reader) error {
		if _, inLocal := localSet[ref.Hash]; inLocal {
			// 已挂在本实例 asset 表：跳过，读干回调流。
			skipped++
			_, _ = io.Copy(io.Discard, rc)
			return nil
		}
		if s.blobs.Exists(ref.Hash) {
			// 已存在于 blobstore（内容寻址去重）：跳过。
			skipped++
			_, _ = io.Copy(io.Discard, rc)
			return nil
		}
		hash, _, _, _, err := s.blobs.Put(rc)
		if err != nil {
			return fmt.Errorf("合并 blob %s：%w", ref.Hash, err)
		}
		if hash != ref.Hash {
			return fmt.Errorf("blob 内容摘要不符：期望 %s 实际 %s", ref.Hash, hash)
		}
		merged++
		return nil
	})
	if extractErr != nil {
		// 任何一步失败：清理暂存目录与标记，不留半成品。
		_ = os.RemoveAll(s.stagingDir)
		_ = os.Remove(s.pendingPath)
		return RestoreResult{}, extractErr
	}

	// 7. 全部 blob 合并成功后才写待生效标记。
	pending := &PendingRestore{
		ImportID:             importID,
		PackageID:            manifest.PackageID,
		Origin:               req.Origin,
		Operator:             req.Operator,
		StagedAt:             s.now(),
		StagedDBRelativePath: filepath.Join(restoreStagingDirName, archive.DBName),
		DBSchemaVersion:      manifest.DBSchemaVersion,
		Counts:               manifest.Counts,
		BlobCount:            manifest.Blobs.Count,
		BlobTotalBytes:       manifest.Blobs.TotalBytes,
		BlobMerged:           merged,
		BlobSkipped:          skipped,
	}
	if err := writePendingFile(s.dataDir, pending); err != nil {
		_ = os.RemoveAll(s.stagingDir)
		return RestoreResult{}, fmt.Errorf("写入待生效标记：%w", err)
	}

	return RestoreResult{
		ImportID:    importID,
		Manifest:    manifest,
		BlobMerged:  merged,
		BlobSkipped: skipped,
		Staged:      true,
		PendingPath: s.pendingPath,
	}, nil
}

// ReconcileStaleStaging 清理崩在中间的暂存目录：暂存目录存在但标记不存在（进程中途崩溃）。
// 与备份的 ReconcileStartup 同理——不能让目录里留下永久幻影。
func (s *RestoreService) ReconcileStaleStaging() error {
	if existing, err := readPendingFile(s.dataDir); err == nil && existing != nil {
		return nil // 标记存在，属正常待生效状态，不动。
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("读取待生效标记：%w", err)
	}
	if _, err := os.Stat(s.stagingDir); err == nil {
		if err := os.RemoveAll(s.stagingDir); err != nil {
			return fmt.Errorf("清理陈旧暂存目录：%w", err)
		}
	}
	return nil
}

// dbSchemaVersion 返回本地已应用迁移版本的整数；读取失败或非整数时退化为 0。
// 读法与 BackupService.dbSchemaVersion 保持一致。
func (s *RestoreService) dbSchemaVersion() int {
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

// localBlobIndex 读取目标实例当前 asset 表引用的 blob 集合（确定性排序）。
// 用于增量差包导入前的基线就绪判定：本地集合 ∪ 包内差集 == Expected 才说明基线已就位。
func (s *RestoreService) localBlobIndex() (archive.BlobIndex, error) {
	m, err := persistence.AssetBlobs(s.dbPath)
	if err != nil {
		return nil, fmt.Errorf("读取本地 blob 集合：%w", err)
	}
	refs := make(archive.BlobIndex, 0, len(m))
	for h, sz := range m {
		refs = append(refs, archive.BlobRef{Hash: h, Size: sz})
	}
	return archive.NewBlobIndex(refs), nil
}

func newImportID(now time.Time) string {
	var raw [3]byte
	_, _ = rand.Read(raw[:])
	return fmt.Sprintf("rs-%s-%s", now.Format("20060102-150405"), hex.EncodeToString(raw[:]))
}

// pendingPath / stagingDir 是与 dataDir 无关的纯路径函数，供本包（含 restore_boot）复用。
func pendingPath(dataDir string) string { return filepath.Join(dataDir, restorePendingFile) }
func stagingDir(dataDir string) string  { return filepath.Join(dataDir, restoreStagingDirName) }

func readPendingFile(dataDir string) (*PendingRestore, error) {
	data, err := os.ReadFile(pendingPath(dataDir))
	if err != nil {
		// 标记文件不存在是**正常状态**（本节点没有待生效恢复），不是错误。
		// 必须在此归一化为 (nil, nil)：调用方（如 BackupImportService.StartURLImport）
		// 把非 nil error 当作内部故障，若把 not-exist 当错误，全新节点将永远
		// 无法发起导入（恒返 500）。
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var p PendingRestore
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("解析待生效标记：%w", err)
	}
	if p.ImportID == "" || p.PackageID == "" {
		return nil, fmt.Errorf("待生效标记字段不完整")
	}
	return &p, nil
}

func writePendingFile(dataDir string, p *PendingRestore) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化待生效标记：%w", err)
	}
	if err := os.WriteFile(pendingPath(dataDir), data, 0o600); err != nil {
		return fmt.Errorf("写入待生效标记：%w", err)
	}
	return nil
}
