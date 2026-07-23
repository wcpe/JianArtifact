package domain_test

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// newAssetService 装配一个基于临时目录的 AssetService 及其仓库 Repo。
func newAssetService(t *testing.T) (*domain.AssetService, *repository.RepoRepo) {
	t.Helper()
	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	return domain.NewAssetService(repos, assets, blobs, upstream.NewClient(5*time.Second)), repos
}

func TestAssetServicePutGetRoundtrip(t *testing.T) {
	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-hosted", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}

	payload := []byte("hello raw hosted")
	got, err := svc.Put("raw-hosted", "dir/file.txt", bytes.NewReader(payload), "text/plain")
	if err != nil {
		t.Fatalf("Put：%v", err)
	}
	if got.Size != int64(len(payload)) || got.ContentType != "text/plain" {
		t.Fatalf("Put 返回元数据不符：%+v", got)
	}

	asset, rc, err := svc.Get("raw-hosted", "dir/file.txt")
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读取内容：%v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("拉取内容与写入不一致：%q", body)
	}
	if asset.BlobHash != got.BlobHash {
		t.Fatalf("blob 哈希不一致：%q != %q", asset.BlobHash, got.BlobHash)
	}
}

func TestAssetServicePutRejectsNonHosted(t *testing.T) {
	svc, repos := newAssetService(t)
	// proxy 类型（非 hosted）应被拒绝。
	if _, err := repos.Create("raw-proxy", "raw", "proxy", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := svc.Put("raw-proxy", "a.txt", bytes.NewReader([]byte("x")), ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("非 hosted 应返回 ErrConflict，实际：%v", err)
	}

	// maven hosted 仓库现由协议层按 format 分派，Put 只按 hosted 判定，应成功存字节。
	if _, err := repos.Create("maven-hosted", "maven", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := svc.Put("maven-hosted", "a.jar", bytes.NewReader([]byte("x")), ""); err != nil {
		t.Fatalf("maven hosted Put 应成功，实际：%v", err)
	}
}

func TestAssetServicePutRepoNotFound(t *testing.T) {
	svc, _ := newAssetService(t)
	if _, err := svc.Put("ghost", "a.txt", bytes.NewReader([]byte("x")), ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("仓库不存在应返回 ErrNotFound，实际：%v", err)
	}
}

func TestAssetServiceGetNotFound(t *testing.T) {
	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-hosted", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, _, err := svc.Get("raw-hosted", "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("路径不存在应返回 ErrNotFound，实际：%v", err)
	}
}

func TestAssetServicePutOverwrite(t *testing.T) {
	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-hosted", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := svc.Put("raw-hosted", "f.txt", bytes.NewReader([]byte("v1")), "text/plain"); err != nil {
		t.Fatalf("首次 Put：%v", err)
	}
	if _, err := svc.Put("raw-hosted", "f.txt", bytes.NewReader([]byte("version-2")), "text/plain"); err != nil {
		t.Fatalf("覆盖 Put：%v", err)
	}
	_, rc, err := svc.Get("raw-hosted", "f.txt")
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	defer func() { _ = rc.Close() }()
	body, _ := io.ReadAll(rc)
	if string(body) != "version-2" {
		t.Fatalf("覆盖写未生效：%q", body)
	}
}
