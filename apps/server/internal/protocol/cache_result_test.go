package protocol

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// cacheResultCapture 记录请求结束后上下文里的缓存来源标记。
type cacheResultCapture struct{ value any }

// newProtocolTestEnv 打开临时 SQLite、迁移，并装配资产/仓库服务与协议路由。
func newProtocolTestEnv(t *testing.T) (*repository.RepoRepo, *domain.AssetService, *domain.RepositoryService) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "protocol.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	assetSvc := domain.NewAssetService(repos, assets, blobstore.NewStore(t.TempDir()), upstream.NewTestClient(5*time.Second))
	repoSvc := domain.NewRepositoryService(repos, repository.NewAclRepo(db), assets,
		domain.NewSettingService(repository.NewSettingRepo(db)), repository.NewUserRepo(db))
	return repos, assetSvc, repoSvc
}

// newProxyProtocolEnv 在临时库上建一个指向 httptest 上游的 proxy 仓库与协议路由，
// 返回可对其发起请求的 gin 引擎与缓存来源捕获器。
func newProxyProtocolEnv(t *testing.T, format, name, remoteURL string) (*gin.Engine, *cacheResultCapture) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	repos, assetSvc, repoSvc := newProtocolTestEnv(t)
	cfg, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{RemoteURL: remoteURL})
	if err != nil {
		t.Fatalf("编码 proxy 配置：%v", err)
	}
	if _, err := repos.Create(name, format, "proxy", "public", cfg); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	capture := &cacheResultCapture{}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Next()
		capture.value, _ = c.Get(CacheResultContextKey)
	})
	raw := NewRawHandler(assetSvc, repoSvc)
	switch format {
	case "maven":
		RegisterRoutes(r, NewMavenHandler(raw))
	case "npm":
		RegisterNpmRoutes(r, NewNpmHandler(raw, nil, nil, ""))
	default:
		t.Fatalf("未支持的测试格式：%s", format)
	}
	return r, capture
}

func performGet(t *testing.T, r *gin.Engine, path string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// TestMavenProxyGetMarksCacheResult 校验 maven proxy 的 GET 依次写入 miss/hit 到上下文。
func TestMavenProxyGetMarksCacheResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("jar bytes"))
	}))
	defer srv.Close()

	r, capture := newProxyProtocolEnv(t, "maven", "mvn-proxy", srv.URL)
	path := "/repository/mvn-proxy/io/demo/demo/1.0.0/demo-1.0.0.jar"

	if code := performGet(t, r, path); code != http.StatusOK {
		t.Fatalf("首次 GET 状态码 = %d，期望 200", code)
	}
	if capture.value != "miss" {
		t.Fatalf("首次 GET 应写入 miss，得 %v", capture.value)
	}

	if code := performGet(t, r, path); code != http.StatusOK {
		t.Fatalf("二次 GET 状态码 = %d，期望 200", code)
	}
	if capture.value != "hit" {
		t.Fatalf("二次 GET 应写入 hit，得 %v", capture.value)
	}
}

// TestNpmProxyTarballGetMarksCacheResult 校验 npm proxy 的 tarball GET 依次写入 miss/hit 到上下文。
func TestNpmProxyTarballGetMarksCacheResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tarball bytes"))
	}))
	defer srv.Close()

	r, capture := newProxyProtocolEnv(t, "npm", "npm-proxy", srv.URL)
	path := "/npm/npm-proxy/demo-pkg/-/demo-pkg-1.0.0.tgz"

	if code := performGet(t, r, path); code != http.StatusOK {
		t.Fatalf("首次 GET 状态码 = %d，期望 200", code)
	}
	if capture.value != "miss" {
		t.Fatalf("首次 GET 应写入 miss，得 %v", capture.value)
	}

	if code := performGet(t, r, path); code != http.StatusOK {
		t.Fatalf("二次 GET 状态码 = %d，期望 200", code)
	}
	if capture.value != "hit" {
		t.Fatalf("二次 GET 应写入 hit，得 %v", capture.value)
	}
}

// TestHostedGetLeavesCacheResultUnset 校验 hosted 仓库读路径不写入缓存来源（保持缺省）。
func TestHostedGetLeavesCacheResultUnset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repos, assetSvc, repoSvc := newProtocolTestEnv(t)
	if _, err := repos.Create("raw-hosted", "raw", "hosted", "public", ""); err != nil {
		t.Fatalf("建 hosted 仓库：%v", err)
	}
	if _, err := assetSvc.Put("raw-hosted", "a/b.txt", strings.NewReader("content"), "text/plain"); err != nil {
		t.Fatalf("写制品：%v", err)
	}

	capture := &cacheResultCapture{}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Next()
		capture.value, _ = c.Get(CacheResultContextKey)
	})
	RegisterRoutes(r, NewRawHandler(assetSvc, repoSvc))

	if code := performGet(t, r, "/repository/raw-hosted/a/b.txt"); code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d，期望 200", code)
	}
	if capture.value != nil {
		t.Fatalf("hosted 读路径不应写入缓存来源，得 %v", capture.value)
	}
}
