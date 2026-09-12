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

// 导入（URL 拉取）上报的 error_code 取值，与契约的错误语义对齐。
const (
	errCodeFetchFailed     = "fetch_failed"     // 拉取失败（连接 / 传输 / 上游非 200）
	errCodePackageOversize = "package_oversize" // 包超过 50 GiB 上限
	errCodeSHA256Mismatch  = "sha256_mismatch"  // 归档整体摘要与期望不符
	errCodeManifestInvalid = "manifest_invalid" // 包非法（增量差包不支持等）
	errCodeTargetNotEmpty  = "target_not_empty" // 目标非空且未 overwrite
	errCodeIncompatible    = "incompatible"     // 包所需 db 版本高于本程序
	errCodeRestorePending  = "restore_pending"  // 已存在待生效恢复
	errCodeInternal        = "internal"
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
func failStage(repo *repository.BackupImportRepo, importID string, stageErr error) {
	code := errCodeInternal
	switch {
	case errors.Is(stageErr, ErrRestoreTargetNotEmpty):
		code = errCodeTargetNotEmpty
	case errors.Is(stageErr, ErrRestoreIncompatible):
		code = errCodeIncompatible
	case errors.Is(stageErr, ErrRestorePending):
		code = errCodeRestorePending
	case errors.Is(stageErr, ErrRestoreIncrementalUnsupported):
		code = errCodeManifestInvalid
	case errors.Is(stageErr, ErrRestoreTooLarge):
		code = errCodePackageOversize
	}
	_ = repo.Finish(importID, repository.ImportStatusFailed, code, stageErr.Error(), "", 0, nil)
}

func newBackupImportID(now time.Time) string {
	var raw [3]byte
	_, _ = rand.Read(raw[:])
	return fmt.Sprintf("imp-%s-%s", now.Format("20060102-150405"), hex.EncodeToString(raw[:]))
}
