package domain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 分片上传（FR-137 第三通道）的常量护栏。
const (
	// uploadChunkSize 是服务端决定的分片大小（8 MiB），在 Init 响应里返回给前端，
	// 前端据此切分；服务端不再信任客户端自报的块大小。
	uploadChunkSize = 8 << 20
	// uploadTTL 是上传会话的存活时长；到期未完成的会话由 ReconcileExpired 清理。
	uploadTTL = 24 * time.Hour
	// maxUploadPackageBytes 限制单包上传落盘的上限（防磁盘打满），与 URL 拉取的 50 GiB 护栏一致。
	maxUploadPackageBytes = int64(50) << 30
)

// sha256HexPattern 校验声明的整体摘要：64 位小写 hex。
var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// 上传会话落盘根目录（相对 ${JIAN_DATA_DIR}）。
const backupUploadDirName = "backup-uploads"

// InitUploadOptions 描述一次分片上传会话的初始化参数。
type InitUploadOptions struct {
	FileName   string // 备份包文件名（仅供审计 / 前端展示）
	TotalBytes int64  // 声明的总字节数
	SHA256     string // 可选：归档整体摘要（64 位小写 hex）
	Operator   string // 发起者（审计用）
}

// InitUploadResult 是 Init 的返回；除会话记录外一并带回服务端决定的分片大小，
// 避免调用方硬编码块大小（若将来调参，契约无需改动）。
type InitUploadResult struct {
	Upload    repository.BackupUpload
	ChunkSize int64
}

// UploadView 是 Get 的返回：会话元信息 + 服务端分片大小 + 已落盘分片序号列表。
// UploadedChunks 由磁盘上真实存在的分片推导（而非仅信数据库），这样"可续传"的
// 语义才正确——前端据此知道还缺哪些片。
type UploadView struct {
	Upload         repository.BackupUpload
	ChunkSize      int64
	UploadedChunks []int
}

// BackupUploadService 编排「Web 分片上传 → 组装归档 → 本地导入」的流程：
// initialized → receiving → completed（取消置 aborted）。组装完成后交给
// BackupImportService.StartLocalImport 走导入状态机（queued → staging → pending_restart）。
type BackupUploadService struct {
	repo    *repository.BackupUploadRepo
	imports *BackupImportService
	dataDir string
	now     func() time.Time
}

// NewBackupUploadService 装配分片上传服务。imports 用于组装完成后发起本地导入。
func NewBackupUploadService(repo *repository.BackupUploadRepo, imports *BackupImportService, dataDir string) *BackupUploadService {
	return &BackupUploadService{
		repo:    repo,
		imports: imports,
		dataDir: dataDir,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// 会话相关路径：所有落盘都在 ${JIAN_DATA_DIR}/backup-uploads/<uploadId>/ 之下。
func (s *BackupUploadService) sessionDir(uploadID string) string {
	return filepath.Join(s.dataDir, backupUploadDirName, uploadID)
}

// chunkSize 返回磁盘上某序号分片的既有字节数；不存在返回 0（重传判定用）。
func (s *BackupUploadService) chunkSize(uploadID string, index int) (int64, error) {
	info, err := os.Stat(s.chunkPath(uploadID, index))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("读取分片 %d：%w", index, err)
	}
	return info.Size(), nil
}

func (s *BackupUploadService) chunksDir(uploadID string) string {
	return filepath.Join(s.sessionDir(uploadID), "chunks")
}
func (s *BackupUploadService) chunkPath(uploadID string, index int) string {
	return filepath.Join(s.chunksDir(uploadID), strconv.Itoa(index))
}
func (s *BackupUploadService) packagePath(uploadID string) string {
	return filepath.Join(s.sessionDir(uploadID), "package.tar.gz")
}

func (s *BackupUploadService) clock() time.Time { return s.now() }

// Init 校验参数、生成会话 ID 并登记；返回时把服务端分片大小一并带回。
func (s *BackupUploadService) Init(ctx context.Context, opts InitUploadOptions) (InitUploadResult, error) {
	if strings.TrimSpace(opts.FileName) == "" {
		return InitUploadResult{}, fmt.Errorf("%w：fileName 不能为空", ErrValidation)
	}
	if opts.TotalBytes <= 0 || opts.TotalBytes > maxUploadPackageBytes {
		return InitUploadResult{}, fmt.Errorf("%w：totalBytes 必须 > 0 且 <= %d", ErrValidation, maxUploadPackageBytes)
	}
	if opts.SHA256 != "" && !sha256HexPattern.MatchString(opts.SHA256) {
		return InitUploadResult{}, fmt.Errorf("%w：sha256 须为 64 位小写 hex", ErrValidation)
	}

	now := s.clock()
	rec := repository.BackupUpload{
		UploadID:   newBackupUploadID(now),
		FileName:   opts.FileName,
		TotalBytes: opts.TotalBytes,
		ChunkSize:  uploadChunkSize,
		Status:     repository.UploadStatusInitialized,
		SHA256:     opts.SHA256,
		Operator:   opts.Operator,
		CreatedAt:  now.Format(time.RFC3339Nano),
		UpdatedAt:  now.Format(time.RFC3339Nano),
		ExpiresAt:  now.Add(uploadTTL).Format(time.RFC3339Nano),
	}
	if err := s.repo.Create(rec); err != nil {
		return InitUploadResult{}, err
	}
	return InitUploadResult{Upload: rec, ChunkSize: uploadChunkSize}, nil
}

// PutChunk 落盘一个分片：先写临时文件再 rename（不留半个分片）；校验序号、大小与累计字节。
// 首次成功落盘后把状态从 initialized 推进到 receiving。
func (s *BackupUploadService) PutChunk(ctx context.Context, uploadID string, index int, r io.Reader, size int64) (repository.BackupUpload, error) {
	rec, err := s.repo.Get(uploadID)
	if errors.Is(err, repository.ErrNotFound) {
		return repository.BackupUpload{}, ErrNotFound
	}
	if err != nil {
		return repository.BackupUpload{}, err
	}
	// 过期会话不再接收（避免无限期占用磁盘）。
	if isExpired(rec.ExpiresAt, s.clock()) {
		return repository.BackupUpload{}, fmt.Errorf("%w：上传会话已过期", ErrValidation)
	}
	// 终态（completed / aborted）不再接收分片。
	if rec.Status != repository.UploadStatusInitialized && rec.Status != repository.UploadStatusReceiving {
		return repository.BackupUpload{}, fmt.Errorf("%w：上传状态不可接收分片（%s）", ErrValidation, rec.Status)
	}
	// 序号越界：必须在 [0, ceil(total/chunkSize))。
	expectedChunks := int((rec.TotalBytes + rec.ChunkSize - 1) / rec.ChunkSize)
	if index < 0 || index >= expectedChunks {
		return repository.BackupUpload{}, fmt.Errorf("%w：分片序号越界（%d），应在 [0, %d)", ErrValidation, index, expectedChunks)
	}
	// 单片大小：0 < size <= chunkSize。
	if size <= 0 || size > rec.ChunkSize {
		return repository.BackupUpload{}, fmt.Errorf("%w：分片大小必须 > 0 且 <= %d，实际 %d", ErrValidation, rec.ChunkSize, size)
	}
	// 累计已上传字节 + 本片 不得超过声明总字节（防超额灌盘打满磁盘）。
	// 重传同一序号是覆盖语义：先扣除该序号在磁盘上的既有字节，否则任何重试都会被
	// "累计超额"误拒（契约要求「重复片幂等覆盖」）。
	uploaded, err := s.sumChunkBytes(uploadID)
	if err != nil {
		return repository.BackupUpload{}, err
	}
	existing, err := s.chunkSize(uploadID, index)
	if err != nil {
		return repository.BackupUpload{}, err
	}
	uploaded -= existing
	if uploaded+size > rec.TotalBytes {
		return repository.BackupUpload{}, fmt.Errorf("%w：累计分片 %d + 本次 %d 超过声明总字节 %d", ErrValidation, uploaded, size, rec.TotalBytes)
	}

	// 落盘：先写 .tmp 再原子 rename，确保不会留下半截分片。
	if err := os.MkdirAll(s.chunksDir(uploadID), 0o750); err != nil {
		return repository.BackupUpload{}, fmt.Errorf("创建分片目录：%w", err)
	}
	tmp := s.chunkPath(uploadID, index) + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return repository.BackupUpload{}, fmt.Errorf("创建分片临时文件：%w", err)
	}
	written, cerr := io.Copy(f, io.LimitReader(r, size))
	if e := f.Close(); e != nil && cerr == nil {
		cerr = e
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return repository.BackupUpload{}, fmt.Errorf("写入分片：%w", cerr)
	}
	if written != size {
		_ = os.Remove(tmp)
		return repository.BackupUpload{}, fmt.Errorf("%w：分片实际写入 %d 字节，声明 %d", ErrValidation, written, size)
	}
	final := s.chunkPath(uploadID, index)
	_ = os.Remove(final) // 重传同一片：覆盖旧文件
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return repository.BackupUpload{}, fmt.Errorf("重命名分片：%w", err)
	}

	// 记分片元数据（幂等覆盖）。
	if err := s.repo.AddChunk(repository.BackupUploadChunk{
		UploadID:   uploadID,
		ChunkIndex: index,
		Size:       size,
	}); err != nil {
		return repository.BackupUpload{}, err
	}
	// 首次成功落盘：initialized → receiving。
	if rec.Status == repository.UploadStatusInitialized {
		if err := s.repo.UpdateStatus(uploadID, repository.UploadStatusReceiving); err != nil {
			return repository.BackupUpload{}, err
		}
		rec.Status = repository.UploadStatusReceiving
	}
	return *rec, nil
}

// Get 返回会话视图；UploadedChunks 由磁盘上真实存在的分片推导。
func (s *BackupUploadService) Get(ctx context.Context, uploadID string) (UploadView, error) {
	rec, err := s.repo.Get(uploadID)
	if errors.Is(err, repository.ErrNotFound) {
		return UploadView{}, ErrNotFound
	}
	if err != nil {
		return UploadView{}, err
	}
	indices, err := s.uploadedChunkIndices(uploadID)
	if err != nil {
		return UploadView{}, err
	}
	return UploadView{Upload: *rec, ChunkSize: rec.ChunkSize, UploadedChunks: indices}, nil
}

// Complete 校验分片齐全后按序流拼成归档（同时算 sha256），核对摘要与字节数，
// 再交给 BackupImportService.StartLocalImport 走导入状态机；成功后状态置 completed。
//
// overwrite/deep 透传给本地导入：目标实例非空时需 overwrite=true 才能覆盖（内置 anonymous 主体
// 不计入「非空」）；deep=true 让导入逐 blob 比对内容摘要（耗时与包体积同阶）。归档文件由
// 调用方/清理策略负责，本方法在成功交接后不删除组装包（CLI 直传亦同此约定）。
func (s *BackupUploadService) Complete(ctx context.Context, uploadID, clientSHA256 string, overwrite, deep bool) (repository.BackupImport, error) {
	rec, err := s.repo.Get(uploadID)
	if errors.Is(err, repository.ErrNotFound) {
		return repository.BackupImport{}, ErrNotFound
	}
	if err != nil {
		return repository.BackupImport{}, err
	}
	if rec.Status != repository.UploadStatusReceiving && rec.Status != repository.UploadStatusInitialized {
		return repository.BackupImport{}, fmt.Errorf("%w：上传状态不可完成（%s）", ErrValidation, rec.Status)
	}

	// 分片齐全校验：0..n-1 必须全在（以磁盘为准）。
	indices, err := s.uploadedChunkIndices(uploadID)
	if err != nil {
		return repository.BackupImport{}, err
	}
	expectedChunks := int((rec.TotalBytes + rec.ChunkSize - 1) / rec.ChunkSize)
	if len(indices) != expectedChunks {
		missing := missingIndices(indices, expectedChunks)
		return repository.BackupImport{}, fmt.Errorf("%w：分片不完整，缺失序号 %v", ErrValidation, missing)
	}

	// 按序流式拼装为归档，同时算整体 sha256。
	sum, totalBytes, err := s.assemble(uploadID, expectedChunks, s.packagePath(uploadID))
	if err != nil {
		return repository.BackupImport{}, err
	}
	// 总字节数必须与声明一致。
	if totalBytes != rec.TotalBytes {
		_ = os.Remove(s.packagePath(uploadID))
		return repository.BackupImport{}, fmt.Errorf("%w：组装包 %d 字节与声明 %d 不符", ErrValidation, totalBytes, rec.TotalBytes)
	}
	// 声明 sha256 核对（init 时提供）。
	if rec.SHA256 != "" && sum != rec.SHA256 {
		_ = os.Remove(s.packagePath(uploadID))
		return repository.BackupImport{}, fmt.Errorf("%w：包摘要与声明 sha256 不符（期望 %s 实际 %s）", ErrValidation, rec.SHA256, sum)
	}
	// 客户端 sha256 核对（complete 时提供）。
	if clientSHA256 != "" && !strings.EqualFold(sum, clientSHA256) {
		_ = os.Remove(s.packagePath(uploadID))
		return repository.BackupImport{}, fmt.Errorf("%w：包摘要与客户端 sha256 不符（期望 %s 实际 %s）", ErrValidation, strings.ToLower(clientSHA256), sum)
	}

	// 交接给本地导入（跳过 fetching，直接 staging）。
	imp, err := s.imports.StartLocalImport(ctx, LocalImportOptions{
		SourcePath: s.packagePath(uploadID),
		Origin:     repository.ImportOriginUpload,
		Operator:   rec.Operator,
		Overwrite:  overwrite,
		Deep:       deep,
	})
	if err != nil {
		return repository.BackupImport{}, err
	}
	if err := s.repo.UpdateStatus(uploadID, repository.UploadStatusCompleted); err != nil {
		return repository.BackupImport{}, err
	}
	return imp, nil
}

// Abort 取消会话：置 aborted 并删除会话磁盘目录（归档与分片一并清除）。
func (s *BackupUploadService) Abort(ctx context.Context, uploadID string) error {
	_, err := s.repo.Get(uploadID)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := s.repo.UpdateStatus(uploadID, repository.UploadStatusAborted); err != nil {
		return err
	}
	_ = os.RemoveAll(s.sessionDir(uploadID))
	return nil
}

// ReconcileExpired 删除过期会话的磁盘目录与记录，供启动期调用。返回清理的会话数。
func (s *BackupUploadService) ReconcileExpired(now time.Time) (int, error) {
	nowStr := now.UTC().Format(time.RFC3339Nano)
	expired, err := s.repo.ListExpired(nowStr)
	if err != nil {
		return 0, err
	}
	for _, rec := range expired {
		_ = os.RemoveAll(s.sessionDir(rec.UploadID))
	}
	if _, err := s.repo.DeleteExpired(nowStr); err != nil {
		return len(expired), err
	}
	return len(expired), nil
}

// sumChunkBytes 统计某会话已落盘分片的总字节（磁盘为真实来源）。
func (s *BackupUploadService) sumChunkBytes(uploadID string) (int64, error) {
	entries, err := os.ReadDir(s.chunksDir(uploadID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

// uploadedChunkIndices 读取磁盘分片目录，返回已落盘且序号可解析的分片序号（升序）。
func (s *BackupUploadService) uploadedChunkIndices(uploadID string) ([]int, error) {
	entries, err := os.ReadDir(s.chunksDir(uploadID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	idx := make([]int, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			// 非序号文件（如临时残留）不计为分片。
			continue
		}
		idx = append(idx, n)
	}
	sortInts(idx)
	return idx, nil
}

// assemble 按序号升序把分片拼成 dest 归档，返回整体 sha256 与总字节数。
func (s *BackupUploadService) assemble(uploadID string, expectedChunks int, dest string) (sum string, total int64, err error) {
	f, err := os.Create(dest)
	if err != nil {
		return "", 0, fmt.Errorf("创建组装包：%w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("关闭组装包：%w", cerr)
		}
	}()
	h := sha256.New()
	w := io.MultiWriter(f, h)
	for i := 0; i < expectedChunks; i++ {
		cf, oerr := os.Open(s.chunkPath(uploadID, i))
		if oerr != nil {
			return "", 0, fmt.Errorf("读取分片 %d：%w", i, oerr)
		}
		n, werr := io.Copy(w, cf)
		_ = cf.Close()
		if werr != nil {
			return "", 0, fmt.Errorf("拼接分片 %d：%w", i, werr)
		}
		total += n
	}
	return hex.EncodeToString(h.Sum(nil)), total, nil
}

func missingIndices(have []int, expected int) []int {
	set := make(map[int]bool, len(have))
	for _, i := range have {
		set[i] = true
	}
	missing := make([]int, 0)
	for i := 0; i < expected; i++ {
		if !set[i] {
			missing = append(missing, i)
		}
	}
	return missing
}

func sortInts(s []int) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func isExpired(expiresAt string, now time.Time) bool {
	exp, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return false
	}
	return exp.Before(now)
}

func newBackupUploadID(now time.Time) string {
	var raw [3]byte
	_, _ = rand.Read(raw[:])
	return fmt.Sprintf("up-%s-%s", now.Format("20060102-150405"), hex.EncodeToString(raw[:]))
}
