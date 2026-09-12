package domain_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

type recordedChange struct {
	entityType string
	entityKey  string
	op         string
	data       any
}

type testChangeRecorder struct{ changes []recordedChange }

func (r *testChangeRecorder) Record(entityType, entityKey, op string, data any) error {
	r.changes = append(r.changes, recordedChange{entityType: entityType, entityKey: entityKey, op: op, data: data})
	return nil
}

// newAssetService 装配一个基于临时目录的 AssetService 及其仓库 Repo。
func newAssetService(t *testing.T) (*domain.AssetService, *repository.RepoRepo) {
	t.Helper()
	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	return domain.NewAssetService(repos, assets, blobs, upstream.NewTestClient(5*time.Second)), repos
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

func TestAssetServiceProxyCacheRecordsReplicationChange(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/example.com/demo/@v/v1.0.0.mod" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("module example.com/demo\n"))
	}))
	t.Cleanup(upstreamServer.Close)

	svc, repos := newAssetService(t)
	if _, err := repos.Create("go-proxy", "gomod", "proxy", "public", `{"remoteUrl":"`+upstreamServer.URL+`"}`); err != nil {
		t.Fatalf("创建代理仓库：%v", err)
	}
	recorder := &testChangeRecorder{}
	svc.SetChangeRecorder(recorder)
	asset, rc, err := svc.Resolve(context.Background(), "go-proxy", "example.com/demo/@v/v1.0.0.mod")
	if err != nil {
		t.Fatalf("代理冷缓存：%v", err)
	}
	_ = rc.Close()
	if len(recorder.changes) != 1 {
		t.Fatalf("代理冷缓存应记录一条复制变更，实际 %d 条", len(recorder.changes))
	}
	change := recorder.changes[0]
	if change.entityType != domain.EntityAsset || change.entityKey != domain.AssetKey("go-proxy", asset.Path) || change.op != domain.OpPut {
		t.Fatalf("代理缓存复制变更不符：%+v", change)
	}
	data, ok := change.data.(domain.AssetChangeData)
	if !ok || data.BlobHash != asset.BlobHash || data.Size != asset.Size || data.Path != asset.Path {
		t.Fatalf("代理缓存复制载荷不符：%+v", change.data)
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

func TestAssetServiceBackfillTimes(t *testing.T) {
	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-hosted", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := svc.Put("raw-hosted", "dir/file.txt", bytes.NewReader([]byte("hello")), "text/plain"); err != nil {
		t.Fatalf("Put：%v", err)
	}

	entries := []domain.AssetTimeEntry{
		{RepoName: "raw-hosted", Path: "dir/file.txt", CreatedAt: "2021-01-01 00:00:00", UpdatedAt: "2022-02-02 03:04:05"},
		{RepoName: "raw-hosted", Path: "no-such-path", CreatedAt: "2021-01-01 00:00:00", UpdatedAt: "2022-02-02 03:04:05"},
		{RepoName: "ghost-repo", Path: "x.txt", CreatedAt: "2021-01-01 00:00:00", UpdatedAt: "2022-02-02 03:04:05"},
	}
	res, err := svc.BackfillTimes(entries, 10)
	if err != nil {
		t.Fatalf("BackfillTimes：%v", err)
	}
	if res.Scanned != 3 || res.Updated != 1 || res.Skipped != 2 {
		t.Fatalf("统计不符：%+v", res)
	}

	asset, rc, err := svc.Get("raw-hosted", "dir/file.txt")
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	defer func() { _ = rc.Close() }()
	if asset.CreatedAt != "2021-01-01 00:00:00" || asset.UpdatedAt != "2022-02-02 03:04:05" {
		t.Fatalf("时间未回填：created=%q updated=%q", asset.CreatedAt, asset.UpdatedAt)
	}
}
