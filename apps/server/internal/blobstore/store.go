// Package blobstore 提供文件系统上的内容寻址 blob 存储。
//
// 分层（见 internal/doc.go）：位于依赖链底部，被 domain 层编排使用，
// 与 persistence 平级——元数据落 SQLite，制品内容落此处。对齐
// docs/adr/0002-sqlite-filesystem-storage.md 与 docs/ARCHITECTURE.md §4-5。
//
// 内容真源即文件系统：按内容 sha256 分片目录寻址（<root>/ab/cd/<hash>），
// 相同内容天然去重。写入经临时文件 + 原子 rename 落盘，边写边算哈希，
// 全程流式不整体入内存（性能不变量）。
package blobstore

import (
	"archive/tar"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// tmpDirName 是临时写入目录名（相对 root）；rename 落盘前的中间文件存放于此。
const tmpDirName = "tmp"

const quarantineDirName = "quarantine"

const ociUploadTmpDirName = "oci-upload"

// hashPattern 校验 sha256 十六进制摘要（64 位小写十六进制）。
var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrInvalidHash 表示传入的哈希不是合法的 sha256 十六进制摘要。
var ErrInvalidHash = errors.New("非法的 blob 哈希")

// Store 是根植于某目录的内容寻址 blob 存储。
type Store struct {
	root       string
	stagedMu   sync.Mutex
	stagedCond *sync.Cond
	staged     map[string]stagedState
}

type stagedState struct {
	references int
	deleting   bool
}

// QuarantineEntry 标识一个已从活动 blobstore 移入隔离区的内容。
type QuarantineEntry struct {
	OperationID string
	Hash        string
	Path        string
}

// RollbackSnapshot 保存一个资产操作尚未完成物理回收时的临时字节副本。
// 成功完成后必须立即删除；它仅用于把物理删除失败恢复为完整失败。
type RollbackSnapshot struct {
	OperationID string
	Path        string
}

// NewStore 构造根植于 root 的 blob 存储。root 目录由调用方（config）保证存在。
func NewStore(root string) *Store {
	store := &Store{root: root, staged: make(map[string]stagedState)}
	store.stagedCond = sync.NewCond(&store.stagedMu)
	return store
}

// Put 将 r 的全部内容流式写入存储，返回内容 sha256 摘要、sha1、md5 与字节数。
// 写入过程：临时文件 + 边写边算哈希（sha256/sha1/md5 三路并行）-> 命中已有内容则去重丢弃 -> 否则原子 rename 落盘。
// sha256 仍为内容寻址键，sha1/md5 供 asset 表登记，均在写入时一次算完，不在读路径现算。
func (s *Store) Put(r io.Reader) (hash, sha1sum, md5sum string, size int64, err error) {
	return s.put(r, false)
}

// PutStaged 将内容流式写入活动存储，并在内容对活动视图可见前登记暂存保护。
// 调用方必须在资产引用成功落库或明确放弃该内容后调用 ReleaseStaged；GC 会跳过
// 仍受保护的 hash，避免“blob 已落盘但元数据尚未提交”期间被删除。
func (s *Store) PutStaged(r io.Reader) (hash, sha1sum, md5sum string, size int64, err error) {
	return s.put(r, true)
}

func (s *Store) put(r io.Reader, staged bool) (hash, sha1sum, md5sum string, size int64, err error) {
	tmpDir := filepath.Join(s.root, tmpDirName)
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return "", "", "", 0, fmt.Errorf("创建临时目录：%w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "blob-*")
	if err != nil {
		return "", "", "", 0, fmt.Errorf("创建临时文件：%w", err)
	}
	tmpName := tmp.Name()
	// 失败路径统一清理临时文件；成功 rename 后临时文件已不存在，Remove 无害。
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	sha256Hasher := sha256.New()
	sha1Hasher := sha1.New()
	md5Hasher := md5.New()
	size, err = io.Copy(io.MultiWriter(tmp, sha256Hasher, sha1Hasher, md5Hasher), r)
	if err != nil {
		_ = tmp.Close()
		return "", "", "", 0, fmt.Errorf("写入临时 blob：%w", err)
	}
	if err = tmp.Close(); err != nil {
		return "", "", "", 0, fmt.Errorf("关闭临时 blob：%w", err)
	}

	hash = hex.EncodeToString(sha256Hasher.Sum(nil))
	sha1sum = hex.EncodeToString(sha1Hasher.Sum(nil))
	md5sum = hex.EncodeToString(md5Hasher.Sum(nil))
	if staged {
		s.acquireStaged(hash)
		defer func() {
			if err != nil {
				s.ReleaseStaged(hash)
			}
		}()
	}
	final := s.pathFor(hash)

	// 内容已存在则去重：删除临时文件，直接复用既有 blob。
	if _, statErr := os.Stat(final); statErr == nil {
		_ = os.Remove(tmpName)
		return hash, sha1sum, md5sum, size, nil
	}

	if err = os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return "", "", "", 0, fmt.Errorf("创建 blob 分片目录：%w", err)
	}
	if err = os.Rename(tmpName, final); err != nil {
		// 并发写入同一内容时，另一写者可能已抢先落盘。Windows 上 rename 到已存在
		// 目标会失败，此时若最终路径已存在即视为去重成功，清理本次临时文件。
		if _, statErr := os.Stat(final); statErr == nil {
			_ = os.Remove(tmpName)
			err = nil
			return hash, sha1sum, md5sum, size, nil
		}
		return "", "", "", 0, fmt.Errorf("落盘 blob：%w", err)
	}
	return hash, sha1sum, md5sum, size, nil
}

// ReleaseStaged 释放一次暂存保护。已落库的资产仍由引用计数保护，未落库内容
// 将在失败清理或后续定时 GC 中回收。
func (s *Store) ReleaseStaged(hash string) {
	if !hashPattern.MatchString(hash) {
		return
	}
	s.stagedMu.Lock()
	defer s.stagedMu.Unlock()
	state := s.staged[hash]
	if state.references <= 1 {
		state.references = 0
		if !state.deleting {
			delete(s.staged, hash)
		} else {
			s.staged[hash] = state
		}
		return
	}
	state.references--
	s.staged[hash] = state
}

func (s *Store) acquireStaged(hash string) {
	s.stagedMu.Lock()
	defer s.stagedMu.Unlock()
	for s.staged[hash].deleting {
		s.stagedCond.Wait()
	}
	state := s.staged[hash]
	state.references++
	s.staged[hash] = state
}

// BeginUnreferencedRemoval 为一次孤立 blob 回收取得独占预约。预约期间新的
// PutStaged 会保留其临时文件并等待，待本次删除完成后再重新落盘，避免删掉
// 已被并发发布复用的同 hash 内容。返回的释放函数必须调用且不得在锁内执行 IO。
func (s *Store) BeginUnreferencedRemoval(hash string) (func(), bool) {
	if !hashPattern.MatchString(hash) {
		return nil, false
	}
	s.stagedMu.Lock()
	state := s.staged[hash]
	if state.references > 0 || state.deleting {
		s.stagedMu.Unlock()
		return nil, false
	}
	state.deleting = true
	s.staged[hash] = state
	s.stagedMu.Unlock()
	return func() {
		s.stagedMu.Lock()
		state := s.staged[hash]
		state.deleting = false
		if state.references == 0 {
			delete(s.staged, hash)
		} else {
			s.staged[hash] = state
		}
		s.stagedCond.Broadcast()
		s.stagedMu.Unlock()
	}, true
}

// CreateOCIUploadTemp 创建 OCI 分块上传专用暂存文件。文件始终位于当前 blob 根目录，
// 进程重启时可仅清理该专用目录，不会误删系统临时目录或其它协议的文件。
func (s *Store) CreateOCIUploadTemp() (*os.File, error) {
	dir := filepath.Join(s.root, tmpDirName, ociUploadTmpDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("创建 OCI 上传暂存目录：%w", err)
	}
	file, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		return nil, fmt.Errorf("创建 OCI 上传暂存文件：%w", err)
	}
	return file, nil
}

// CleanupOCIUploadTemps 清理当前 blob 根目录上次进程遗留的 OCI 上传会话文件。
// 仅应在服务尚未接收请求的启动阶段调用。
func (s *Store) CleanupOCIUploadTemps() error {
	err := os.RemoveAll(filepath.Join(s.root, tmpDirName, ociUploadTmpDirName))
	if err != nil {
		return fmt.Errorf("清理 OCI 上传暂存目录：%w", err)
	}
	return nil
}

// WalkActive 逐项遍历活动内容寻址 blob。临时目录和隔离区均不属于活动视图，
// 因而不会交给调用方处理。
func (s *Store) WalkActive(fn func(hash string) error) error {
	err := filepath.WalkDir(s.root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == s.root || entry.IsDir() {
			if entry.IsDir() && (entry.Name() == tmpDirName || entry.Name() == quarantineDirName) {
				return filepath.SkipDir
			}
			return nil
		}
		hash := entry.Name()
		if !entry.Type().IsRegular() || !hashPattern.MatchString(hash) || filepath.Clean(filePath) != filepath.Clean(s.pathFor(hash)) {
			return nil
		}
		return fn(hash)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Open 打开指定哈希的 blob，返回可读流与字节数；不存在返回 os.ErrNotExist。
func (s *Store) Open(hash string) (io.ReadCloser, int64, error) {
	if !hashPattern.MatchString(hash) {
		return nil, 0, ErrInvalidHash
	}
	f, err := os.Open(s.pathFor(hash))
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// Checksums 对已落盘 blob 流式计算 sha1 与 md5（并复验 sha256 与寻址键一致）。
// 供历史资产回填使用；不改变内容寻址布局。
func (s *Store) Checksums(hash string) (sha1sum, md5sum string, err error) {
	rc, _, err := s.Open(hash)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = rc.Close() }()

	sha256Hasher := sha256.New()
	sha1Hasher := sha1.New()
	md5Hasher := md5.New()
	if _, err = io.Copy(io.MultiWriter(sha256Hasher, sha1Hasher, md5Hasher), rc); err != nil {
		return "", "", fmt.Errorf("读取 blob 计算校验和：%w", err)
	}
	got := hex.EncodeToString(sha256Hasher.Sum(nil))
	if got != hash {
		return "", "", fmt.Errorf("blob 内容 sha256 与寻址键不一致：期望 %s 实际 %s", hash, got)
	}
	return hex.EncodeToString(sha1Hasher.Sum(nil)), hex.EncodeToString(md5Hasher.Sum(nil)), nil
}

// Exists 报告指定哈希的 blob 是否已落盘。非法哈希恒返回 false。
func (s *Store) Exists(hash string) bool {
	if !hashPattern.MatchString(hash) {
		return false
	}
	_, err := os.Stat(s.pathFor(hash))
	return err == nil
}

// Stage 将活动 blob 在同一文件系统内原子移动到持久隔离区。
// 隔离区不属于任何正常读取路径，调用方应在同一操作 intent 中持久化返回的路径。
func (s *Store) Stage(hash, operationID string) (QuarantineEntry, error) {
	if !hashPattern.MatchString(hash) {
		return QuarantineEntry{}, ErrInvalidHash
	}
	if !validOperationID(operationID) {
		return QuarantineEntry{}, errors.New("非法的资产操作标识")
	}
	entry := QuarantineEntry{OperationID: operationID, Hash: hash, Path: filepath.Join(s.root, quarantineDirName, operationID, hash)}
	if err := os.MkdirAll(filepath.Dir(entry.Path), 0o750); err != nil {
		return QuarantineEntry{}, fmt.Errorf("创建 blob 隔离目录：%w", err)
	}
	if err := os.Rename(s.pathFor(hash), entry.Path); err != nil {
		return QuarantineEntry{}, fmt.Errorf("隔离 blob：%w", err)
	}
	return entry, nil
}

// OpenQuarantine 仅供恢复流程读取隔离区中的 blob；协议读路径不得调用此方法。
func (s *Store) OpenQuarantine(entry QuarantineEntry) (io.ReadCloser, int64, error) {
	if !hashPattern.MatchString(entry.Hash) || !validOperationID(entry.OperationID) {
		return nil, 0, ErrInvalidHash
	}
	f, err := os.Open(s.quarantinePath(entry.OperationID, entry.Hash))
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// Restore 将隔离 blob 恢复到活动路径。目标已被并发写入时保留活动内容并清理隔离副本。
func (s *Store) Restore(entry QuarantineEntry) error {
	if !hashPattern.MatchString(entry.Hash) || !validOperationID(entry.OperationID) {
		return ErrInvalidHash
	}
	quarantined := s.quarantinePath(entry.OperationID, entry.Hash)
	if _, err := os.Stat(quarantined); err != nil {
		if errors.Is(err, os.ErrNotExist) && s.Exists(entry.Hash) {
			return nil
		}
		return err
	}
	if s.Exists(entry.Hash) {
		return os.Remove(quarantined)
	}
	if err := os.MkdirAll(filepath.Dir(s.pathFor(entry.Hash)), 0o750); err != nil {
		return err
	}
	return os.Rename(quarantined, s.pathFor(entry.Hash))
}

// Finalize 永久删除隔离 blob。失败由上层记录为可重试 GC，不影响已完成业务事务。
func (s *Store) Finalize(entry QuarantineEntry) error {
	if !hashPattern.MatchString(entry.Hash) || !validOperationID(entry.OperationID) {
		return ErrInvalidHash
	}
	err := os.Remove(s.quarantinePath(entry.OperationID, entry.Hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// CreateRollbackSnapshot 将同一操作的隔离内容流式封装为单一临时快照。
// 单一快照使批量物理删除失败后仍可恢复所有字节，且其最终删除是一个文件操作。
func (s *Store) CreateRollbackSnapshot(entries []QuarantineEntry) (RollbackSnapshot, error) {
	if len(entries) == 0 {
		return RollbackSnapshot{}, nil
	}
	operationID := entries[0].OperationID
	if !validOperationID(operationID) {
		return RollbackSnapshot{}, errors.New("非法的资产操作标识")
	}
	snapshot := s.RollbackSnapshot(operationID)
	file, err := os.Create(snapshot.Path)
	if err != nil {
		return RollbackSnapshot{}, fmt.Errorf("创建回滚快照：%w", err)
	}
	writer := tar.NewWriter(file)
	closed := false
	defer func() {
		if !closed {
			_ = writer.Close()
			_ = file.Close()
		}
	}()
	for _, entry := range entries {
		if entry.OperationID != operationID || !hashPattern.MatchString(entry.Hash) {
			_ = os.Remove(snapshot.Path)
			return RollbackSnapshot{}, errors.New("回滚快照条目非法")
		}
		info, err := os.Stat(s.quarantinePath(entry.OperationID, entry.Hash))
		if err != nil {
			_ = os.Remove(snapshot.Path)
			return RollbackSnapshot{}, fmt.Errorf("读取隔离 blob：%w", err)
		}
		if err := writer.WriteHeader(&tar.Header{Name: entry.Hash, Mode: 0o600, Size: info.Size()}); err != nil {
			_ = os.Remove(snapshot.Path)
			return RollbackSnapshot{}, fmt.Errorf("写入回滚快照头：%w", err)
		}
		input, err := os.Open(s.quarantinePath(entry.OperationID, entry.Hash))
		if err != nil {
			_ = os.Remove(snapshot.Path)
			return RollbackSnapshot{}, fmt.Errorf("打开隔离 blob：%w", err)
		}
		_, copyErr := io.Copy(writer, input)
		_ = input.Close()
		if copyErr != nil {
			_ = os.Remove(snapshot.Path)
			return RollbackSnapshot{}, fmt.Errorf("写入回滚快照内容：%w", copyErr)
		}
	}
	if err := writer.Close(); err != nil {
		_ = os.Remove(snapshot.Path)
		return RollbackSnapshot{}, fmt.Errorf("关闭回滚快照：%w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(snapshot.Path)
		return RollbackSnapshot{}, fmt.Errorf("关闭回滚快照文件：%w", err)
	}
	closed = true
	return snapshot, nil
}

// RollbackSnapshot 返回指定操作的固定临时快照位置。
func (s *Store) RollbackSnapshot(operationID string) RollbackSnapshot {
	return RollbackSnapshot{OperationID: operationID, Path: filepath.Join(s.root, quarantineDirName, operationID, "rollback.tar")}
}

// RestoreRollbackSnapshot 将临时快照恢复到活动 blob 存储；当前已有同 hash 内容时保留活动副本。
func (s *Store) RestoreRollbackSnapshot(snapshot RollbackSnapshot) error {
	if snapshot.OperationID == "" || !validOperationID(snapshot.OperationID) || snapshot.Path != filepath.Join(s.root, quarantineDirName, snapshot.OperationID, "rollback.tar") {
		return errors.New("非法的回滚快照")
	}
	file, err := os.Open(snapshot.Path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取回滚快照：%w", err)
		}
		if header.Typeflag != tar.TypeReg || !hashPattern.MatchString(header.Name) || header.Size < 0 {
			return errors.New("回滚快照内容非法")
		}
		if s.Exists(header.Name) {
			if _, err := io.Copy(io.Discard, reader); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(s.pathFor(header.Name)), 0o750); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(s.root, tmpDirName), 0o750); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Join(s.root, tmpDirName), "restore-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		_, copyErr := io.Copy(tmp, reader)
		closeErr := tmp.Close()
		if copyErr != nil || closeErr != nil {
			_ = os.Remove(tmpName)
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}
		if err := os.Rename(tmpName, s.pathFor(header.Name)); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
	}
}

// FinalizeRollbackSnapshot 删除已完成操作的临时快照。
func (s *Store) FinalizeRollbackSnapshot(snapshot RollbackSnapshot) error {
	if snapshot.OperationID == "" || !validOperationID(snapshot.OperationID) || snapshot.Path != filepath.Join(s.root, quarantineDirName, snapshot.OperationID, "rollback.tar") {
		return errors.New("非法的回滚快照")
	}
	err := os.Remove(snapshot.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Remove 删除未被元数据引用的活动 blob；引用计数由 domain 协调器在调用前校验。
func (s *Store) Remove(hash string) error {
	if !hashPattern.MatchString(hash) {
		return ErrInvalidHash
	}
	err := os.Remove(s.pathFor(hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) quarantinePath(operationID, hash string) string {
	return filepath.Join(s.root, quarantineDirName, operationID, hash)
}

func validOperationID(operationID string) bool {
	return operationID != "" && filepath.Base(operationID) == operationID && operationID != "." && operationID != ".."
}

// pathFor 返回哈希对应的最终存储路径：<root>/<h[0:2]>/<h[2:4]>/<hash>。
func (s *Store) pathFor(hash string) string {
	return filepath.Join(s.root, hash[0:2], hash[2:4], hash)
}
