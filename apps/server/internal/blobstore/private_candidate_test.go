package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type candidateReaderFunc func([]byte) (int, error)

func (fn candidateReaderFunc) Read(p []byte) (int, error) { return fn(p) }

func TestPrivateCandidateStreamAndLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  string
		limit int64
		want  error
	}{
		{"空流", "", 0, nil},
		{"零上限非空", "a", 0, ErrPrivateCandidateLimit},
		{"恰好上限", "甲乙", 6, nil},
		{"超限一字节", "甲乙丙", 6, ErrPrivateCandidateLimit},
		{"大上限不溢出", "甲乙丙", math.MaxInt64, nil},
		{"负上限", "", -1, ErrInvalidPrivateCandidateInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(t.TempDir())
			remaining := []byte(tc.data)
			consumed := 0
			src := candidateReaderFunc(func(p []byte) (int, error) {
				n := copy(p, remaining[:min(len(remaining), 2)])
				remaining = remaining[n:]
				consumed += n
				if len(remaining) == 0 {
					return n, io.EOF
				}
				return n, nil
			})
			candidate, err := store.StagePrivateCandidate(t.Context(), src, tc.limit)
			if tc.want != nil {
				if candidate != nil || !errors.Is(err, tc.want) {
					t.Fatalf("期望失败 %v，得到 %v；句柄是否为空 %v", tc.want, err, candidate == nil)
				}
				if tc.limit >= 0 && consumed != int(tc.limit)+1 {
					t.Fatalf("实际读取 %d；超限仅允许多读一字节", consumed)
				}
				assertPrivateCandidateFiles(t, store, 0)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if candidate.Size() != int64(len(tc.data)) || candidate.SHA256() != fmt.Sprintf("%x", sha256.Sum256([]byte(tc.data))) {
				t.Fatal("接收值与真实字节不匹配")
			}
			path := assertPrivateCandidateFiles(t, store, 1)[0]
			stored, err := os.ReadFile(path)
			if err != nil || string(stored) != tc.data {
				t.Fatalf("读取私有字节失败：%v", err)
			}
			if runtime.GOOS != "windows" {
				for _, p := range []string{path, filepath.Dir(path)} {
					info, err := os.Stat(p)
					if err != nil || info.Mode().Perm()&0o077 != 0 {
						t.Fatalf("私有文件或目录授权过宽：%v", err)
					}
				}
			}
			encoded, err := json.Marshal(candidate)
			if err != nil || string(encoded) != "{}" {
				t.Fatalf("句柄不应序列化内部字段：%s，%v", encoded, err)
			}
			for _, formatted := range []string{fmt.Sprint(candidate), fmt.Sprintf("%+v", candidate), fmt.Sprintf("%#v", candidate), fmt.Sprintf("%#v", *candidate)} {
				if strings.Contains(formatted, filepath.Base(path)) {
					t.Fatal("句柄格式化泄露私有文件名")
				}
			}
			if err := candidate.Discard(); err != nil {
				t.Fatal(err)
			}
			if err := candidate.Discard(); err != nil {
				t.Fatal(err)
			}
			assertPrivateCandidateFiles(t, store, 0)
		})
	}

	t.Run("固定缓冲大流", func(t *testing.T) {
		const total = 2 << 20
		want := sha256.New()
		left := total
		candidate, err := NewStore(t.TempDir()).StagePrivateCandidate(t.Context(), candidateReaderFunc(func(p []byte) (int, error) {
			if len(p) > 64<<10 {
				t.Fatalf("单次读缓冲过大：%d", len(p))
			}
			n := min(left, len(p))
			for i := range p[:n] {
				p[i] = byte((total - left + i) % 127)
			}
			_, _ = want.Write(p[:n])
			left -= n
			if left == 0 {
				return n, io.EOF
			}
			return n, nil
		}), total)
		if err != nil || candidate.Size() != total || candidate.SHA256() != fmt.Sprintf("%x", want.Sum(nil)) {
			t.Fatalf("大流接收结果不符：%v", err)
		}
	})
}

func TestPrivateCandidateCancellationAndReadErrors(t *testing.T) {
	readErr := errors.New("测试读错误")
	for _, tc := range []struct {
		name         string
		read         candidateReaderFunc
		cancelBefore bool
		cancelDuring bool
		want         error
	}{
		{"预先取消", nil, true, false, context.Canceled},
		{"读取中取消", nil, false, true, context.Canceled},
		{"部分读取出错", func(p []byte) (int, error) { p[0] = 'x'; return 1, readErr }, false, false, readErr},
		{"负计数", func([]byte) (int, error) { return -1, nil }, false, false, errInvalidPrivateCandidateRead},
		{"计数超缓冲", func(p []byte) (int, error) { return len(p) + 1, nil }, false, false, errInvalidPrivateCandidateRead},
		{"无进度", func([]byte) (int, error) { return 0, nil }, false, false, io.ErrNoProgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(t.TempDir())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := tc.read
			if tc.cancelBefore {
				cancel()
				reader = func([]byte) (int, error) { t.Fatal("取消后不应读流"); return 0, nil }
			}
			if tc.cancelDuring {
				reader = func(p []byte) (int, error) { p[0] = 'x'; cancel(); return 1, io.EOF }
			}
			got, err := store.StagePrivateCandidate(ctx, reader, 9)
			if got != nil || !errors.Is(err, tc.want) {
				t.Fatalf("期望失败 %v，得到 %v", tc.want, err)
			}
			assertPrivateCandidateFiles(t, store, 0)
		})
	}

	store := NewStore(t.TempDir())
	if err := os.WriteFile(filepath.Join(store.root, "tmp"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.StagePrivateCandidate(t.Context(), strings.NewReader("普通字节"), 99)
	if got != nil || err == nil || strings.Contains(err.Error(), store.root) {
		t.Fatalf("目录初始化应失败，且错误不暴露路径：%v", err)
	}
}

type candidateFaultFile struct {
	bytes.Buffer
	short             bool
	syncErr, closeErr error
	beforeClose       func()
	closed            bool
}

func (f *candidateFaultFile) Write(p []byte) (int, error) {
	if f.short {
		return 0, nil
	}
	return f.Buffer.Write(p)
}
func (f *candidateFaultFile) Sync() error { return f.syncErr }
func (f *candidateFaultFile) Close() error {
	f.closed = true
	if f.beforeClose != nil {
		f.beforeClose()
	}
	return f.closeErr
}

func TestPrivateCandidateFinalizeErrorsAndRetry(t *testing.T) {
	fail := errors.New("测试落盘错误")
	removeErr := errors.New("测试删除错误")
	for _, name := range []string{"短写", "同步", "关闭", "关闭时取消"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f := &candidateFaultFile{}
			want := fail
			switch name {
			case "短写":
				f.short, want = true, io.ErrShortWrite
			case "同步":
				f.syncErr = fail
			case "关闭":
				f.closeErr = fail
			case "关闭时取消":
				f.beforeClose, want = cancel, context.Canceled
			}
			calls := 0
			got, err := sealPrivateCandidate(ctx, strings.NewReader("普通字节"), 100, f, func() error {
				if !f.closed {
					t.Fatal("必须先关闭再清理")
				}
				calls++
				return removeErr
			})
			if got != nil || !errors.Is(err, want) || !errors.Is(err, removeErr) || calls != 1 {
				t.Fatalf("应同时保留主因 %v 和清理失败，实际 %v，次数 %d", want, err, calls)
			}
		})
	}
	calls := 0
	candidate, err := sealPrivateCandidate(t.Context(), strings.NewReader("数据"), 100, &candidateFaultFile{}, func() error {
		calls++
		if calls == 1 {
			return removeErr
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := candidate.Discard(); !errors.Is(err, removeErr) {
		t.Fatalf("首次清理错误应可见：%v", err)
	}
	for range 2 {
		if err := candidate.Discard(); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("清理应仅失败时重试，实际 %d 次", calls)
	}
}

func TestPrivateCandidateIsolationAndConcurrency(t *testing.T) {
	store := NewStore(t.TempDir())
	const body = "同样的私有字节"
	const other = "活动阳性对照"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	visible, _, _, _, err := store.Put(strings.NewReader(other))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 2)
	resume := make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	type outcome struct {
		candidate *PrivateCandidate
		err       error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			got, err := store.StagePrivateCandidate(t.Context(), candidateReaderFunc(func(p []byte) (int, error) {
				started <- struct{}{}
				<-resume
				return copy(p, body), io.EOF
			}), 100)
			results <- outcome{got, err}
		}()
	}
	for range 2 {
		select {
		case <-started:
		case out := <-results:
			t.Fatalf("写入未进入读阶段：%v", out.err)
		case <-time.After(5 * time.Second):
			t.Fatal("等待私有接收握手超时")
		}
	}
	assertHiddenCandidateHash(t, store, hash, visible)
	release()
	var candidates [2]*PrivateCandidate
	for i := range candidates {
		out := <-results
		if out.err != nil || out.candidate.SHA256() != hash {
			t.Fatalf("并行接收失败：%v", out.err)
		}
		candidates[i] = out.candidate
		t.Cleanup(func() {
			if err := out.candidate.Discard(); err != nil {
				t.Error(err)
			}
		})
	}
	assertPrivateCandidateFiles(t, store, 2)
	assertHiddenCandidateHash(t, store, hash, visible)
	if err := store.CleanupOCIUploadTemps(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CleanupOCIUploadTempsBefore(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertPrivateCandidateFiles(t, store, 2)
	active, _, _, _, err := store.Put(strings.NewReader(body))
	if err != nil || active != hash {
		t.Fatalf("写入活动同摘要阳性对照：%v", err)
	}
	discarded := make(chan error, 2)
	for range 2 {
		go func() { discarded <- candidates[0].Discard() }()
	}
	for range 2 {
		if err := <-discarded; err != nil {
			t.Fatal(err)
		}
	}
	assertPrivateCandidateFiles(t, store, 1)
	if !store.Exists(hash) {
		t.Fatal("清理私有句柄不能触及同摘要活动对象")
	}
}

func assertPrivateCandidateFiles(t *testing.T, store *Store, count int) []string {
	t.Helper()
	dir := filepath.Join(store.root, "tmp", "admission-candidates")
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != count {
		t.Fatalf("私有文件数为 %d，期望 %d", len(entries), count)
	}
	paths := make([]string, 0, count)
	for _, entry := range entries {
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	return paths
}

func assertHiddenCandidateHash(t *testing.T, store *Store, hidden, visible string) {
	t.Helper()
	if rc, _, err := store.Open(hidden); !errors.Is(err, os.ErrNotExist) {
		if rc != nil {
			_ = rc.Close()
		}
		t.Fatalf("私有摘要不应被活动 Open 打开：%v", err)
	}
	if store.Exists(hidden) || !store.Exists(visible) {
		t.Fatal("活动存在性与预期不符")
	}
	seen := 0
	if err := store.WalkActive(func(hash string) error {
		if hash != visible {
			t.Errorf("活动枚举出现意外摘要：%s", hash)
		}
		seen++
		return nil
	}); err != nil || seen != 1 {
		t.Fatalf("活动枚举应命中唯一阳性对照：%d，%v", seen, err)
	}
}
