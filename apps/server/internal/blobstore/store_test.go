package blobstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPutAndOpenRoundtrip(t *testing.T) {
	s := NewStore(t.TempDir())
	content := []byte("hello raw artifact")
	want := sha256.Sum256(content)
	wantHash := hex.EncodeToString(want[:])

	hash, _, _, size, err := s.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put 失败：%v", err)
	}
	if hash != wantHash {
		t.Fatalf("哈希不符：got %s want %s", hash, wantHash)
	}
	if size != int64(len(content)) {
		t.Fatalf("字节数不符：got %d want %d", size, len(content))
	}
	if !s.Exists(hash) {
		t.Fatalf("Exists 应为 true")
	}

	rc, rsize, err := s.Open(hash)
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	defer func() { _ = rc.Close() }()
	if rsize != int64(len(content)) {
		t.Fatalf("Open 字节数不符：got %d want %d", rsize, len(content))
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("内容不符：got %q want %q", got, content)
	}
}

func TestPutDedup(t *testing.T) {
	s := NewStore(t.TempDir())
	content := []byte("dedup me")

	h1, _, _, _, err := s.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("首次 Put 失败：%v", err)
	}
	h2, _, _, _, err := s.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("再次 Put 失败：%v", err)
	}
	if h1 != h2 {
		t.Fatalf("相同内容哈希应一致：%s vs %s", h1, h2)
	}
	// 临时目录不应残留中间文件。
	entries, err := os.ReadDir(filepath.Join(s.root, tmpDirName))
	if err == nil && len(entries) != 0 {
		t.Fatalf("临时目录应为空，残留 %d 项", len(entries))
	}
}

func TestOpenMissing(t *testing.T) {
	s := NewStore(t.TempDir())
	missing := hex.EncodeToString(sha256.New().Sum(nil))
	_, _, err := s.Open(missing)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("应返回 os.ErrNotExist，实际：%v", err)
	}
}

func TestInvalidHash(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, _, err := s.Open("not-a-hash"); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("Open 非法哈希应返回 ErrInvalidHash，实际：%v", err)
	}
	if s.Exists("XYZ") {
		t.Fatalf("Exists 非法哈希应为 false")
	}
}

func TestPutConcurrentSameContent(t *testing.T) {
	s := NewStore(t.TempDir())
	content := []byte("concurrent identical content")
	want := sha256.Sum256(content)
	wantHash := hex.EncodeToString(want[:])

	const n = 8
	var wg sync.WaitGroup
	hashes := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			h, _, _, _, err := s.Put(bytes.NewReader(content))
			hashes[idx] = h
			errs[idx] = err
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("并发 Put[%d] 失败：%v", i, errs[i])
		}
		if hashes[i] != wantHash {
			t.Fatalf("并发 Put[%d] 哈希不符：%s", i, hashes[i])
		}
	}
	if !s.Exists(wantHash) {
		t.Fatalf("并发写后 blob 应存在")
	}
}
