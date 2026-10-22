package domain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// 导入上报的 error_code 取值，与契约的错误语义对齐。
//
// 每个错误码都对应一种**用户可理解、且多数可自行处置**的状态。
// 只有真正的未知故障才用 internal——把可解释的失败塞进 internal 会让用户
// 只看到"服务内部错误"而无从下手（这正是 修掉的问题）。
const (
	errCodeFetchFailed      = "fetch_failed"       // 拉取失败（连接 / 传输 / 上游非 200）
	errCodePackageOversize  = "package_oversize"   // 包超过 50 GiB 上限
	errCodeSHA256Mismatch   = "sha256_mismatch"    // 归档整体摘要与期望不符
	errCodeManifestInvalid  = "manifest_invalid"   // 包非法（增量差包不支持等）
	errCodeTargetNotEmpty   = "target_not_empty"   // 目标非空且未 overwrite
	errCodeIncompatible     = "incompatible"       // 包所需 db 版本高于本程序
	errCodeRestorePending   = "restore_pending"    // 已存在待生效恢复
	errCodeBaseMissing      = "base_missing"       // 差包缺少基线（先导入基线包）
	errCodeImportInProgress = "import_in_progress" // 已有导入正在进行（可重试）
	errCodePackageNotReady  = "package_not_ready"  // 包未生成完成或已失败
	errCodeInternal         = "internal"           // 真正的未知故障
)

// maxBackupImportBytes 限制单包下载落盘的上限（防磁盘打满）。备份包本质是用户/运维
// 主动发起的搬迁输入，体积可能极大，但必须有硬上限：边下边写磁盘，不先探测整包大小，
// 故只能用"超限即中止"的护栏。50 GiB 覆盖绝大多数节点；超过即视为异常包。
const maxBackupImportBytes int64 = 50 << 30

// 下载阶段向前端回写进度的频率：每累计 8 MiB 或每 2 秒落一次，兼顾实时性与写入开销。
const (
	importProgressByteStep = 8 << 20
	importProgressInterval = 2 * time.Second
)

// 下载落盘目录（相对 ${JIAN_DATA_DIR}）。
const backupImportDirName = "backup-imports"

// ErrBackupImportInProgress 表示已有导入在运行；RestoreService 内部互斥之外，
// 这里在进程内更早返回明确错误，避免用户连点导致两条导入互相踩踏。
var ErrBackupImportInProgress = errors.New("已有导入正在执行")

// CreateImportOptions 描述一次 URL 拉取导入。
type CreateImportOptions struct {
	SourceURL      string // 备份包归档的 http/https 地址
	Operator       string // 发起者（审计用）
	Overwrite      bool   // 目标非空时是否覆盖
	Deep           bool   // 逐 blob 比对摘要
	ExpectedSHA256 string // 可选：归档整体摘要核对
}

// BackupImportService 编排「从 URL 拉取备份包并导入」的异步流程：
// queued → fetching → staging → pending_restart；任何失败置 failed 并写 error_code。
//
// 落盘文件与 RestoreService 的暂存目录都是单例资源，故导入严格串行。Stage 返回后
// 写 restore.pending 并置 pending_restart；真正替换数据库需重启服务（见 restore_boot）。
type BackupImportService struct {
	repo    *repository.BackupImportRepo
	restore *RestoreService
	dataDir string
	client  *upstream.Client

	mu        sync.Mutex
	importing bool
	now       func() time.Time
}

// NewBackupImportService 装配 URL 拉取导入服务。client 必须执行出站安全策略
// （默认拦截私网 / 回环 / 元数据地址）；下载上限与超时由 client 控制。
func NewBackupImportService(repo *repository.BackupImportRepo, restore *RestoreService, dataDir string, client *upstream.Client) *BackupImportService {
	return &BackupImportService{
		repo:    repo,
		restore: restore,
		dataDir: dataDir,
		client:  client,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// DownloadDir 返回下载落盘目录（供测试与诊断）。
func (s *BackupImportService) DownloadDir() string {
	return filepath.Join(s.dataDir, backupImportDirName)
}

// StartURLImport 立刻登记一条 status=queued 记录并返回（HTTP 层据此回 202），
// 后台 goroutine 推进拉取与暂存。请求结束不中断导入（用 WithoutCancel 隔离上下文）。
func (s *BackupImportService) StartURLImport(ctx context.Context, opts CreateImportOptions) (repository.BackupImport, error) {
	if err := validateImportURL(opts.SourceURL); err != nil {
		return repository.BackupImport{}, err
	}
	// 已有待生效恢复：拒绝叠加，避免用户连点后看到的是旧 pending 而非新导入。
	if existing, err := s.restore.MarkedPending(); err != nil {
		return repository.BackupImport{}, fmt.Errorf("检查待生效恢复：%w", err)
	} else if existing != nil {
		return repository.BackupImport{}, ErrRestorePending
	}
	// 进程内串行化：同一时刻只允许一个导入在跑。
	if !s.beginImport() {
		return repository.BackupImport{}, ErrBackupImportInProgress
	}
	rec := repository.BackupImport{
		ImportID:  newBackupImportID(s.now()),
		Origin:    repository.ImportOriginURL,
		Status:    repository.ImportStatusQueued,
		SourceURL: opts.SourceURL,
		Operator:  opts.Operator,
		Overwrite: opts.Overwrite,
		Deep:      opts.Deep,
	}
	if err := s.repo.Create(rec); err != nil {
		s.endImport()
		return repository.BackupImport{}, err
	}
	go func() {
		// 请求可能随时结束，导入必须继续；隔离取消但保留超时（client 自带）。
		s.process(context.WithoutCancel(ctx), rec.ImportID, opts)
	}()
	return rec, nil
}

// List / Count / Get 转发仓储。
func (s *BackupImportService) List(limit, offset int) ([]repository.BackupImport, error) {
	return s.repo.List(limit, offset)
}
func (s *BackupImportService) Count() (int, error) { return s.repo.Count() }
func (s *BackupImportService) Get(importID string) (*repository.BackupImport, error) {
	rec, err := s.repo.Get(importID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrNotFound
	}
	return rec, err
}

// PendingRestore 返回当前是否存在待重启生效的恢复；不存在时返回 (nil, nil)。
//
// 供界面回答「为什么我现在导入不了」：一旦暂存成功，后续导入一律以 409 restore_pending
// 被拒，直到重启完成替换。此前这个状态只体现在导入记录行的 pending_restart 徽章上，
// 徽章被更近的事件挤出视野后，用户就再也看不出原因。
//
// 标记读不了（属主/权限不对）按"无待生效恢复"对待并交回错误——调用方据此如实说明，
// 但不应因此让整个端点失败：这只是一个提示性查询。
func (s *BackupImportService) PendingRestore() (*PendingRestore, error) {
	return s.restore.MarkedPending()
}

// LocalImportOptions 描述一次本地导入（CLI 直传 / 分片上传组装后的归档）。
// SourcePath 是已落地的归档路径；Origin 标注入口（"upload" | "cli"）。
type LocalImportOptions struct {
	SourcePath string
	Origin     string
	Operator   string
	Overwrite  bool
	Deep       bool
}

// StartLocalImport 立刻登记一条 status=queued 记录并返回，后台 goroutine 推进暂存与收尾。
// 与 StartURLImport 的区别是跳过 fetching 阶段（包已由调用方落盘），直接 queued → staging → pending_restart；
// 复用既有的 beginImport/endImport 串行化、MarkedPending 前置检查，以及把 Stage 错误映射为 error_code 的逻辑
// （见 stageAndFinish，与 URL 拉取路径共享）。注意：不删除调用方给的归档（CLI 场景是用户文件，
// 分片上传场景由 Abort/清理策略负责）。
func (s *BackupImportService) StartLocalImport(ctx context.Context, opts LocalImportOptions) (repository.BackupImport, error) {
	if strings.TrimSpace(opts.SourcePath) == "" {
		return repository.BackupImport{}, fmt.Errorf("%w：sourcePath 不能为空", ErrValidation)
	}
	// 已有待生效恢复：拒绝叠加，避免用户连点后看到的是旧 pending 而非新导入。
	if existing, err := s.restore.MarkedPending(); err != nil {
		return repository.BackupImport{}, fmt.Errorf("检查待生效恢复：%w", err)
	} else if existing != nil {
		return repository.BackupImport{}, ErrRestorePending
	}
	// 进程内串行化：同一时刻只允许一个导入在跑。
	if !s.beginImport() {
		return repository.BackupImport{}, ErrBackupImportInProgress
	}
	rec := repository.BackupImport{
		ImportID:  newBackupImportID(s.now()),
		Origin:    opts.Origin,
		Status:    repository.ImportStatusQueued,
		Operator:  opts.Operator,
		Overwrite: opts.Overwrite,
		Deep:      opts.Deep,
	}
	if err := s.repo.Create(rec); err != nil {
		s.endImport()
		return repository.BackupImport{}, err
	}
	go func() {
		// 请求可能随时结束，导入必须继续；隔离取消但保留超时（client 自带）。
		s.runLocalImport(context.WithoutCancel(ctx), rec.ImportID, opts)
	}()
	return rec, nil
}

// runLocalImport 推进一条本地导入（跳过 fetching，包已落盘）。
func (s *BackupImportService) runLocalImport(ctx context.Context, importID string, opts LocalImportOptions) {
	defer s.endImport()
	s.stageAndFinish(ctx, importID, opts.SourcePath, opts.Origin, opts.Operator, opts.Overwrite, opts.Deep)
	// 本地归档由调用方所有（CLI 用户文件 / 分片上传会话目录），此处不删除，清理交由调用方。
}

func (s *BackupImportService) process(ctx context.Context, importID string, opts CreateImportOptions) {
	defer s.endImport()

	dest := filepath.Join(s.DownloadDir(), importID)
	if err := os.MkdirAll(s.DownloadDir(), 0o750); err != nil {
		failImport(s.repo, importID, errCodeInternal, fmt.Sprintf("创建下载目录：%v", err))
		return
	}
	if err := s.repo.UpdateProgress(importID, repository.ImportStatusFetching, 0, 0); err != nil {
		failImport(s.repo, importID, errCodeInternal, fmt.Sprintf("更新进度：%v", err))
		return
	}

	_, _, _, sum, dlErr := s.download(ctx, importID, dest, opts.SourceURL)
	if dlErr != nil {
		// download 内部已按语义写 failed（fetch_failed / package_oversize）。
		_ = os.Remove(dest)
		return
	}

	// 归档整体摘要核对（在交给 Stage 之前，避免无谓的大文件解包）。
	if opts.ExpectedSHA256 != "" && !strings.EqualFold(sum, opts.ExpectedSHA256) {
		failImport(s.repo, importID, errCodeSHA256Mismatch,
			fmt.Sprintf("归档摘要不符：期望 %s 实际 %s", strings.ToLower(opts.ExpectedSHA256), sum))
		_ = os.Remove(dest)
		return
	}

	// 公共收尾：staging → Stage → pending_restart（或 failed）。
	s.stageAndFinish(ctx, importID, dest, repository.ImportOriginURL, opts.Operator, opts.Overwrite, opts.Deep)
	// 下载中间文件已无用（Stage 在暂存目录留有独立副本），清理以免占盘（仅 URL 拉取场景下 dest 为服务端临时文件）。
	_ = os.Remove(dest)
}

// restoreFailureLogName 是恢复失败的**文件级**留痕（落在数据目录下）。
//
// 为什么不能只写数据库：backup_import 表在数据库里，而恢复会替换整个数据库。
// 于是一次失败的恢复，其失败记录可能被**它自己的恢复动作**覆盖掉——实测丢失过一次，
// 导致完全无法定位。文件在数据目录下、不在快照内，因此不会被覆盖。
const restoreFailureLogName = "restore-failures.log"

// appendRestoreFailureRecord 把一次恢复失败追加到文件日志。
//
// 内容刻意包含 operator / origin / package / errorCode / 原始错误，足以在数据库记录
// 丢失后仍能还原"谁、在什么时候、对哪个包、因为什么失败了"。
// 任何失败都静默忽略：留痕是尽力而为的附带动作，不该反过来影响主流程。
func appendRestoreFailureRecord(dataDir, importID, origin, operator, packageID, code, detail string) {
	if dataDir == "" {
		return
	}
	line := fmt.Sprintf("%s\timport=%s\torigin=%s\toperator=%s\tpackage=%s\tcode=%s\tdetail=%s\n",
		time.Now().UTC().Format(time.RFC3339), importID, origin, operator, packageID, code, detail)
	f, err := os.OpenFile(filepath.Join(dataDir, restoreFailureLogName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(line)
}

// stageAndFinish 是 URL 拉取与本地导入共享的收尾逻辑：置 staging → 调 RestoreService.Stage
// （同步阻塞：校验→合并 blob→暂存 db→写 restore.pending）→ 成功置 pending_restart；任何失败
// 经 failStage 把领域错误映射为明确的 error_code 并写 failed 记录。
func (s *BackupImportService) stageAndFinish(ctx context.Context, importID, sourcePath, origin, operator string, overwrite, deep bool) {
	if err := s.repo.UpdateProgress(importID, repository.ImportStatusStaging, 0, 0); err != nil {
		failImport(s.repo, importID, errCodeInternal, fmt.Sprintf("更新进度：%v", err))
		return
	}
	rr, stageErr := s.restore.Stage(ctx, RestoreRequest{
		SourcePath: sourcePath,
		Origin:     origin,
		Operator:   operator,
		Overwrite:  overwrite,
		Deep:       deep,
	})
	if stageErr != nil {
		failStage(s.repo, importID, stageErr)
		// 同时写到文件：数据库记录可能被后续的恢复动作覆盖掉（见 restoreFailureLogName）。
		appendRestoreFailureRecord(s.dataDir, importID, origin, operator,
			stagePackageHint(sourcePath), stageErrorCode(stageErr), stageErr.Error())
		return
	}

	// 成功：置 pending_restart 并写 restore_pending_at；真正替换数据库需重启。
	rpa := time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.repo.Finish(importID, repository.ImportStatusPendingRestart, "", "",
		rr.Manifest.PackageID, rr.Manifest.Blobs.Count, &rpa); err != nil {
		failImport(s.repo, importID, errCodeInternal, fmt.Sprintf("收尾导入记录：%v", err))
		return
	}
}

// download 流式拉取并落盘：限制大小（防磁盘打满）、累加已下载字节（周期回写进度）、
// 边下边算归档整体 sha256。非 2xx、传输错误与私网拦截统一归为 fetch_failed；超限为 package_oversize。
// 任何失败路径内部已写 failed 记录。
func (s *BackupImportService) download(ctx context.Context, importID, dest, sourceURL string) (ok bool, fetched, total int64, sha256sum string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		failImport(s.repo, importID, errCodeFetchFailed, fmt.Sprintf("构造请求：%v", err))
		return false, 0, 0, "", err
	}
	// Do 在每次拨号前重新校验目标地址（防 DNS 重绑定），并复用 upstream 的 SSRF 防护。
	resp, err := s.client.Do(req)
	if err != nil {
		failImport(s.repo, importID, errCodeFetchFailed, fmt.Sprintf("拉取备份包：%v", err))
		return false, 0, 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		failImport(s.repo, importID, errCodeFetchFailed, fmt.Sprintf("上游返回状态码 %d", resp.StatusCode))
		return false, 0, 0, "", fmt.Errorf("status %d", resp.StatusCode)
	}

	total = resp.ContentLength
	if total < 0 {
		total = 0
	}

	f, err := os.Create(dest)
	if err != nil {
		failImport(s.repo, importID, errCodeInternal, fmt.Sprintf("创建下载文件：%v", err))
		return false, 0, 0, "", err
	}
	defer func() { _ = f.Close() }()

	hasher := sha256.New()
	buf := make([]byte, 64*1024)
	var fetchedBytes int64
	lastWrite := time.Now()
	lastFetched := int64(0)
	for {
		if cerr := ctx.Err(); cerr != nil {
			failImport(s.repo, importID, errCodeFetchFailed, fmt.Sprintf("下载被取消或超时：%v", cerr))
			return false, 0, 0, "", cerr
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			hasher.Write(buf[:n])
			if _, werr := f.Write(buf[:n]); werr != nil {
				failImport(s.repo, importID, errCodeInternal, fmt.Sprintf("写入下载文件：%v", werr))
				return false, 0, 0, "", werr
			}
			fetchedBytes += int64(n)
			if fetchedBytes > maxBackupImportBytes {
				failImport(s.repo, importID, errCodePackageOversize,
					fmt.Sprintf("备份包超过上限 %d 字节", maxBackupImportBytes))
				return false, 0, 0, "", fmt.Errorf("oversize")
			}
			if fetchedBytes-lastFetched >= importProgressByteStep || time.Since(lastWrite) >= importProgressInterval {
				_ = s.repo.UpdateProgress(importID, repository.ImportStatusFetching, fetchedBytes, total)
				lastWrite = time.Now()
				lastFetched = fetchedBytes
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			failImport(s.repo, importID, errCodeFetchFailed, fmt.Sprintf("读取响应体：%v", readErr))
			return false, 0, 0, "", readErr
		}
	}
	_ = s.repo.UpdateProgress(importID, repository.ImportStatusFetching, fetchedBytes, total)
	return true, fetchedBytes, total, hex.EncodeToString(hasher.Sum(nil)), nil
}

func (s *BackupImportService) beginImport() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.importing {
		return false
	}
	s.importing = true
	return true
}

func (s *BackupImportService) endImport() {
	s.mu.Lock()
	s.importing = false
	s.mu.Unlock()
}

// validateImportURL 校验来源必须是 http/https 绝对地址（更细的私网拦截交给 upstream）。
func validateImportURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w：sourceUrl 不能为空", ErrValidation)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return fmt.Errorf("%w：sourceUrl 必须是 http/https 地址", ErrValidation)
	}
	return nil
}

// failImport 收尾一条失败记录（内部错误 / 拉取 / 超容 / 摘要不符）。
func failImport(repo *repository.BackupImportRepo, importID, code, summary string) {
	_ = repo.Finish(importID, repository.ImportStatusFailed, code, summary, "", 0, nil)
}

// failStage 把恢复暂存的领域错误映射为明确的 error_code 并写失败记录。
// failStage 把 Stage 的领域错误映射为明确的 error_code 并写 failed 记录。
//
// # 为什么必须覆盖全部领域错误
//
// 早先这里只认 5 种错误，其余一律归为 internal。后果是用户只看到
// 「服务内部错误，请稍后重试或查看日志」——而真实原因（基线包缺失、已有导入在跑、
// 归档损坏、manifest 非法…）完全被丢弃，日志里也没有对应记录。
// 那正是"恢复一直失败却查不出原因"的来源。
//
// 现在的口径是：
//   - 已知的领域错误 → 各自的明确错误码；
//   - 未知错误 → 保留 internal，但**把原始错误文本写进 error 字段**，
//     让用户与运维至少能看到真实原因，而不是一句套话。
func failStage(repo *repository.BackupImportRepo, importID string, stageErr error) {
	_ = repo.Finish(importID, repository.ImportStatusFailed, stageErrorCode(stageErr),
		stageErr.Error(), "", 0, nil)
}

// stageErrorCode 把 Stage 的领域错误映射为 error_code。
//
// 抽成独立函数是为了让"写数据库"与"写文件留痕"共用同一份映射——
// 两处各写一遍迟早不一致，而它们描述的是同一件事。
func stageErrorCode(stageErr error) string {
	switch {
	case errors.Is(stageErr, ErrRestoreTargetNotEmpty):
		return errCodeTargetNotEmpty
	case errors.Is(stageErr, ErrRestoreIncompatible):
		return errCodeIncompatible
	case errors.Is(stageErr, ErrRestorePending):
		return errCodeRestorePending
	case errors.Is(stageErr, ErrRestoreIncrementalUnsupported):
		return errCodeManifestInvalid
	case errors.Is(stageErr, ErrRestoreTooLarge):
		return errCodePackageOversize
	case errors.Is(stageErr, ErrRestoreBaseMissing):
		// 差包的基线不在本机：这是运维可处置的状态（先导入基线包），不是内部故障。
		return errCodeBaseMissing
	case errors.Is(stageErr, ErrRestoreInProgress):
		// 并发导入是正常的可重试状态，不该伪装成内部错误。
		return errCodeImportInProgress
	case errors.Is(stageErr, ErrRestorePackageNotDone):
		// 包尚未生成完成 / 已失败，不能作为恢复源。
		return errCodePackageNotReady
	}
	return errCodeInternal
}

// stagePackageHint 从源路径里取出便于人读的包标识（失败留痕用）。
//
// 源路径可能是 backup-uploads/<id>/package.tar.gz 或 backups/<id>.tar.gz，
// 两者都取最后一段并剥掉后缀；剥不出有意义的名字时退回完整路径。
func stagePackageHint(sourcePath string) string {
	base := filepath.Base(sourcePath)
	if base == "package.tar.gz" {
		return filepath.Base(filepath.Dir(sourcePath))
	}
	return strings.TrimSuffix(base, ".tar.gz")
}

func newBackupImportID(now time.Time) string {
	var raw [3]byte
	_, _ = rand.Read(raw[:])
	return fmt.Sprintf("imp-%s-%s", now.Format("20060102-150405"), hex.EncodeToString(raw[:]))
}
