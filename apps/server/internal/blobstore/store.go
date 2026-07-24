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
)

// tmpDirName 是临时写入目录名（相对 root）；rename 落盘前的中间文件存放于此。
const tmpDirName = "tmp"

// hashPattern 校验 sha256 十六进制摘要（64 位小写十六进制）。
var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrInvalidHash 表示传入的哈希不是合法的 sha256 十六进制摘要。
var ErrInvalidHash = errors.New("非法的 blob 哈希")

// Store 是根植于某目录的内容寻址 blob 存储。
type Store struct {
	root string
}

// NewStore 构造根植于 root 的 blob 存储。root 目录由调用方（config）保证存在。
func NewStore(root string) *Store {
	return &Store{root: root}
}

// Put 将 r 的全部内容流式写入存储，返回内容 sha256 摘要、sha1、md5 与字节数。
// 写入过程：临时文件 + 边写边算哈希（sha256/sha1/md5 三路并行）-> 命中已有内容则去重丢弃 -> 否则原子 rename 落盘。
// sha256 仍为内容寻址键，sha1/md5 供 asset 表登记，均在写入时一次算完，不在读路径现算。
func (s *Store) Put(r io.Reader) (hash, sha1sum, md5sum string, size int64, err error) {
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

// pathFor 返回哈希对应的最终存储路径：<root>/<h[0:2]>/<h[2:4]>/<hash>。
func (s *Store) pathFor(hash string) string {
	return filepath.Join(s.root, hash[0:2], hash[2:4], hash)
}
