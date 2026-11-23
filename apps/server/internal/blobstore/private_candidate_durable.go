package blobstore

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// ErrPrivateCandidateIntegrity 表示在可信存储视图下已证实封存对象缺失或不符合登记值。
var ErrPrivateCandidateIntegrity = errors.New("私有封存字节完整性失败")

func validPrivateCandidateKey(key string) bool {
	if len(key) != 32 {
		return false
	}
	for _, c := range key {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Store) registeredCandidateRoot(create bool) (*os.Root, error) {
	dir := s.root
	for _, part := range []struct {
		name string
		mode os.FileMode
	}{{tmpDirName, 0750}, {privateCandidateDirName, 0700}, {"registered", 0700}} {
		dir = filepath.Join(dir, part.name)
		if create {
			if err := preparePrivateCandidateDir(dir, part.mode); err != nil {
				return nil, err
			}
		} else {
			info, err := os.Lstat(dir)
			if err != nil {
				return nil, privateCandidateFileError("核对候选目录", err)
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (part.mode == 0700 && runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
				return nil, errors.New("候选目录不满足隔离要求")
			}
		}
	}
	root, err := os.OpenRoot(dir)
	return root, privateCandidateFileError("打开候选目录", err)
}

// StageRegisteredPrivateCandidate 只排他创建已经持久登记的服务端文件键，不替换任何对象。
func (s *Store) StageRegisteredPrivateCandidate(ctx context.Context, key string, src io.Reader, limit int64) (*PrivateCandidate, error) {
	if ctx == nil || src == nil || limit < 0 || !validPrivateCandidateKey(key) {
		return nil, ErrInvalidPrivateCandidateInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := s.registeredCandidateRoot(true)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := root.OpenFile(key, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, privateCandidateFileError("创建已登记候选", err)
	}
	candidate, err := sealPrivateCandidate(ctx, src, limit, file, func() error {
		reopened, err := s.registeredCandidateRoot(false)
		if err != nil {
			return err
		}
		defer reopened.Close()
		return reopened.Remove(key)
	})
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		// 同卷目录同步不等于文件与 SQLite 的跨介质原子提交。
		for _, dir := range []string{".", "..", "../.."} {
			f, err := os.Open(filepath.Join(root.Name(), dir))
			if err != nil {
				return nil, privateCandidateFileError("打开候选父目录", err)
			}
			if err := errors.Join(privateCandidateFileError("同步候选目录", f.Sync()), privateCandidateFileError("关闭候选目录", f.Close())); err != nil {
				return nil, err
			}
		}
		f, err := os.Open(s.root)
		if err != nil {
			return nil, privateCandidateFileError("打开文件卷", err)
		}
		if err := errors.Join(privateCandidateFileError("同步文件卷目录", f.Sync()), privateCandidateFileError("关闭文件卷目录", f.Close())); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return candidate, nil
}

// VerifyRegisteredPrivateCandidate 只验证一个已登记对象，从不返回字节或路径。
func (s *Store) VerifyRegisteredPrivateCandidate(ctx context.Context, key, digest string, size int64) error {
	if !validPrivateCandidateKey(key) || len(digest) != 64 || !hashPattern.MatchString(digest) || size < 0 {
		return ErrInvalidPrivateCandidateInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := s.registeredCandidateRoot(false)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(key)
	if errors.Is(err, os.ErrNotExist) {
		return ErrPrivateCandidateIntegrity
	}
	if err != nil {
		return privateCandidateFileError("检查封存对象", err)
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return ErrPrivateCandidateIntegrity
	}
	f, err := root.Open(key)
	if err != nil {
		return privateCandidateFileError("打开封存对象", err)
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return privateCandidateFileError("核对封存对象", err)
	}
	if !actual.Mode().IsRegular() || !os.SameFile(info, actual) {
		return ErrPrivateCandidateIntegrity
	}
	sum, count, err := copyPrivateCandidate(ctx, io.Discard, f, size)
	if errors.Is(err, ErrPrivateCandidateLimit) {
		return ErrPrivateCandidateIntegrity
	}
	if err != nil {
		return err
	}
	if count != size || sum != digest {
		return ErrPrivateCandidateIntegrity
	}
	return nil
}

// NewRegisteredPrivateCandidateKey 编码服务端随机字节；文件键不使用操作号或摘要。
func NewRegisteredPrivateCandidateKey(random [16]byte) string { return hex.EncodeToString(random[:]) }
