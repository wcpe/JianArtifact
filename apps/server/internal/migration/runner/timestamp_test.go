package runner

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// stubAssetWriter 是 AssetWriter 的最小桩，记录传入的时间戳以断言透传。
type stubAssetWriter struct {
	putTS     time.Time
	putCalled bool
	putWithTS bool
}

func (s *stubAssetWriter) Put(repoName, path string, r io.Reader, contentType string) (*repository.Asset, error) {
	s.putCalled = true
	return &repository.Asset{RepositoryID: 1, Path: path}, nil
}

func (s *stubAssetWriter) PutWithTimestamps(repoName, path string, r io.Reader, contentType string, sourceModified time.Time) (*repository.Asset, error) {
	s.putWithTS = true
	s.putTS = sourceModified
	return &repository.Asset{RepositoryID: 1, Path: path}, nil
}

func (s *stubAssetWriter) Exists(repoName, path string) (bool, error) { return false, nil }
func (s *stubAssetWriter) LoadPathSet(repoName string) (map[string]bool, error) {
	return nil, nil
}
func (s *stubAssetWriter) ImmutableRelease(repoName string) (bool, error) { return false, nil }

// TestImportItemPassesSourceTimestamp 断言在线迁移装配点把 sourceItem 的源端时间透传给 writer。
func TestImportItemPassesSourceTimestamp(t *testing.T) {
	stub := &stubAssetWriter{}
	r := &Runner{assets: stub}

	srcTime := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	item := sourceItem{
		Repo:           "maven-hosted",
		Path:           "com/example/lib/1.0/lib-1.0.jar",
		Format:         "raw", // 走 default 分支，触发 PutWithTimestamps
		SourceModified: srcTime,
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("blob-bytes")), nil
		},
	}
	if err := r.importItem(item); err != nil {
		t.Fatalf("importItem：%v", err)
	}
	if !stub.putWithTS {
		t.Fatal("应调用 PutWithTimestamps 而非 Put")
	}
	if !stub.putTS.Equal(srcTime) {
		t.Fatalf("源端时间戳未透传：got %v want %v", stub.putTS, srcTime)
	}

	// 零值时间戳同样透传（回退语义在 domain 层处理）。
	stub2 := &stubAssetWriter{}
	r2 := &Runner{assets: stub2}
	zeroItem := sourceItem{
		Repo:   "maven-hosted",
		Path:   "a/b.jar",
		Format: "raw",
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("x")), nil
		},
	}
	if err := r2.importItem(zeroItem); err != nil {
		t.Fatalf("importItem(零值)：%v", err)
	}
	if !stub2.putWithTS {
		t.Fatal("零值时间戳也应调用 PutWithTimestamps")
	}
	if !stub2.putTS.IsZero() {
		t.Fatalf("零值时间戳应原样透传：got %v", stub2.putTS)
	}
}
