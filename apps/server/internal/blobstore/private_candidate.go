package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
)

const (
	privateCandidateDirName    = "admission-candidates"
	privateCandidateBuffer     = 32 << 10
	privateCandidateEmptyReads = 100
)

// ErrPrivateCandidateLimit 表示实际输入字节超过单对象上限。
var ErrPrivateCandidateLimit = errors.New("私有候选超过字节上限")

// ErrInvalidPrivateCandidateInput 表示接收参数不合法。
var ErrInvalidPrivateCandidateInput = errors.New("私有候选接收参数无效")

var errInvalidPrivateCandidateRead = errors.New("私有候选读取返回无效字节数")

// PrivateCandidate 是仅供本进程使用的已接收字节句柄，不代表持久候选或发布批准。
// 私有路径仅由清理闭包持有，不出现在句柄字段的格式化或序列化结果中。
type PrivateCandidate struct {
	digest  string
	size    int64
	discard func() error
}

// SHA256 返回实际接收字节的 SHA-256 小写十六进制摘要。
func (c *PrivateCandidate) SHA256() string { return c.digest }

// Size 返回实际接收字节数。
func (c *PrivateCandidate) Size() int64 { return c.size }

// Discard 删除本句柄的私有字节。成功后幂等，失败可以重试，绝不操作活动摘要路径。
func (c *PrivateCandidate) Discard() error {
	if c == nil || c.discard == nil {
		return nil
	}
	return c.discard()
}

// StagePrivateCandidate 在服务自身控制的 blob 根下独立接收流，不登记活动 blob 或任何公开引用。
// 上限按实际字节计，零仅允许空流。取消在读取与写入之间检查，不会中断阻塞中的调用，
// 也不会关闭调用方的 src。成功后由调用方负责 Discard；没有崩溃恢复或后台清理承诺。
func (s *Store) StagePrivateCandidate(ctx context.Context, src io.Reader, maxBytes int64) (*PrivateCandidate, error) {
	if ctx == nil || src == nil || maxBytes < 0 {
		return nil, ErrInvalidPrivateCandidateInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tmp := filepath.Join(s.root, tmpDirName)
	if err := preparePrivateCandidateDir(tmp, 0o750); err != nil {
		return nil, err
	}
	dir := filepath.Join(tmp, privateCandidateDirName)
	if err := preparePrivateCandidateDir(dir, 0o700); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, "candidate-*")
	if err != nil {
		return nil, privateCandidateFileError("创建私有候选文件", err)
	}
	path := file.Name()
	return sealPrivateCandidate(ctx, src, maxBytes, file, func() error { return os.Remove(path) })
}

func preparePrivateCandidateDir(path string, mode os.FileMode) error {
	if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return privateCandidateFileError("创建私有候选目录", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return privateCandidateFileError("检查私有候选目录", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("私有候选目录不是实际目录")
	}
	// Windows 的访问控制继承服务目录 ACL，不以 POSIX mode 推断权限。
	if mode == 0o700 && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return errors.New("私有候选目录权限过宽")
	}
	return nil
}

type privateCandidateFile interface {
	io.Writer
	Sync() error
	Close() error
}

func sealPrivateCandidate(ctx context.Context, src io.Reader, maxBytes int64, file privateCandidateFile, remove func() error) (*PrivateCandidate, error) {
	digest, size, err := copyPrivateCandidate(ctx, file, src, maxBytes)
	if err == nil {
		if err = ctx.Err(); err == nil {
			err = privateCandidateFileError("同步私有候选文件", file.Sync())
		}
	}
	closeErr := privateCandidateFileError("关闭私有候选文件", file.Close())
	// 最后一次检查决定是否把这个已关闭对象交给调用方。
	if err = errors.Join(err, closeErr, ctx.Err()); err != nil {
		return nil, errors.Join(err, removePrivateCandidate(remove))
	}
	return &PrivateCandidate{digest: digest, size: size, discard: privateCandidateDiscard(remove)}, nil
}

func copyPrivateCandidate(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (string, int64, error) {
	var buf [privateCandidateBuffer]byte
	hash := sha256.New()
	var size int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		remaining := limit - size
		chunk := buf[:]
		if remaining < int64(len(chunk)) {
			// 仅在余量小于缓冲长度时加一，既探测真实 EOF，又不会溢出。
			chunk = chunk[:remaining+1]
		}
		n, readErr := src.Read(chunk)
		if n < 0 || n > len(chunk) {
			return "", 0, errInvalidPrivateCandidateRead
		}
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		if readErr != nil && readErr != io.EOF {
			return "", 0, fmt.Errorf("读取私有候选字节：%w", readErr)
		}
		if int64(n) > remaining {
			return "", 0, ErrPrivateCandidateLimit
		}
		if n > 0 {
			emptyReads = 0
			written, err := dst.Write(chunk[:n])
			if err != nil {
				return "", 0, privateCandidateFileError("写入私有候选文件", err)
			}
			if written != n {
				return "", 0, io.ErrShortWrite
			}
			_, _ = hash.Write(chunk[:n])
			size += int64(n)
		}
		if readErr == io.EOF {
			return hex.EncodeToString(hash.Sum(nil)), size, nil
		}
		if n == 0 {
			emptyReads++
			if emptyReads >= privateCandidateEmptyReads {
				return "", 0, io.ErrNoProgress
			}
		}
	}
}

func privateCandidateDiscard(remove func() error) func() error {
	type attempt struct {
		done chan struct{}
		err  error
	}
	var active atomic.Pointer[attempt]
	return func() error {
		for {
			if current := active.Load(); current != nil {
				<-current.done
				return current.err
			}
			current := &attempt{done: make(chan struct{})}
			if !active.CompareAndSwap(nil, current) {
				continue
			}
			// 同一文件并发清理共享结果，但不持锁进行磁盘操作。
			current.err = removePrivateCandidate(remove)
			close(current.done)
			if current.err != nil {
				active.Store(nil)
			}
			return current.err
		}
	}
}

func removePrivateCandidate(remove func() error) error {
	err := remove()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return privateCandidateFileError("删除私有候选文件", err)
}

func privateCandidateFileError(action string, err error) error {
	if err == nil {
		return nil
	}
	var fileErr *os.PathError
	if errors.As(err, &fileErr) {
		err = fileErr.Err
	}
	return fmt.Errorf("%s：%w", action, err)
}
