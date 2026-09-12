package domain

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

func TestPyPIProxySizeReaderRejectsContentBeyondLimit(t *testing.T) {
	reader := &pypiProxySizeReader{reader: bytes.NewReader([]byte("abc")), remaining: 2}
	if _, err := io.ReadAll(reader); !errors.Is(err, errPyPIProxyPackageTooLarge) {
		t.Fatalf("超过限制的 PyPI proxy 内容必须拒绝：%v", err)
	}
}

func TestPyPIPublishAuditFailureDoesNotLeaveProjection(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	metadata := repository.NewFormatMetadataRepo(db)
	if _, err := repos.Create("pypi-audit", "pypi", "hosted", "private", ""); err != nil {
		t.Fatalf("创建 PyPI 仓库：%v", err)
	}
	svc := NewFormatMetadataService(NewAssetService(repos, assets, blobstore.NewStore(t.TempDir()), nil), repos, metadata)
	_, err := svc.PublishPyPIWithAudit("pypi-audit", "demo", "1.0.0", "demo-1.0.0.tar.gz", "", "", bytes.NewReader([]byte("demo")), AssetOperationAudit{
		Commit: func(string, []repository.AssetMutationItem) repository.MutationCompletionHook {
			return func(*sqlx.Tx) error { return errors.New("拒绝 PyPI 审计") }
		},
	})
	if err == nil {
		t.Fatal("审计提交失败必须中止 PyPI 发布")
	}
	repo, err := repos.GetByName("pypi-audit")
	if err != nil {
		t.Fatal(err)
	}
	if count, err := assets.CountByRepo(repo.ID, ""); err != nil || count != 0 {
		t.Fatalf("审计失败不得可见资产：count=%d err=%v", count, err)
	}
	if files, err := metadata.List(repo.ID, "pypi", "demo"); err != nil || len(files) != 0 {
		t.Fatalf("审计失败不得可见 PyPI 元数据：files=%+v err=%v", files, err)
	}
}

func TestFormatPublishMetadataFailureDoesNotLeaveAsset(t *testing.T) {
	for _, tc := range []struct {
		name, format, filename, path string
		publish                      func(*FormatMetadataService) error
	}{
		{
			name: "pypi", format: "pypi", filename: "fail-1.0.0.tar.gz", path: "pypi/packages/demo/fail-1.0.0.tar.gz",
			publish: func(s *FormatMetadataService) error {
				_, err := s.PublishPyPI("pypi", "demo", "1.0.0", "fail-1.0.0.tar.gz", "", "", bytes.NewReader([]byte("pypi")))
				return err
			},
		},
		{
			name: "nuget", format: "nuget", filename: "fail.1.0.0.nupkg", path: "nuget/demo/1.0.0/fail.1.0.0.nupkg",
			publish: func(s *FormatMetadataService) error {
				_, err := s.PublishNuGet("nuget", "Demo", "1.0.0", "fail.1.0.0.nupkg", "{}", bytes.NewReader([]byte("nuget")))
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := mutationTestDB(t)
			repos := repository.NewRepoRepo(db)
			assets := repository.NewAssetRepo(db)
			metadata := repository.NewFormatMetadataRepo(db)
			blobs := blobstore.NewStore(t.TempDir())
			repoID, err := repos.Create(tc.format, tc.format, "hosted", "private", "")
			if err != nil {
				t.Fatalf("创建仓库：%v", err)
			}
			if _, err := db.Exec(`CREATE TRIGGER reject_format_metadata BEFORE INSERT ON format_metadata BEGIN SELECT RAISE(ABORT, '注入元数据失败'); END`); err != nil {
				t.Fatalf("创建失败注入：%v", err)
			}
			mutator, err := NewAssetMutationCoordinator(db, blobs)
			if err != nil {
				t.Fatalf("创建资产协调器：%v", err)
			}
			mutator.finalize = func(blobstore.QuarantineEntry) error { return errors.New("注入回收失败") }
			assetSvc := NewAssetService(repos, assets, blobs, nil)
			assetSvc.SetMutationCoordinator(mutator)
			service := NewFormatMetadataService(assetSvc, repos, metadata)

			if err := tc.publish(service); err == nil {
				t.Fatal("元数据写入失败必须返回错误")
			}
			if _, err := assets.GetByPath(repoID, tc.path); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("元数据失败后不得残留资产：%v", err)
			}
			if _, err := metadata.Get(repoID, tc.format, "demo", tc.filename); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("元数据失败后不得残留索引：%v", err)
			}
		})
	}
}

func TestPyPIProxyMetadataFailureDoesNotLeaveCachedPackage(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	metadata := repository.NewFormatMetadataRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	payloads := map[string][]byte{
		"demo-1.0.0.whl": []byte("pypi-proxy-package-one"),
		"demo-2.0.0.whl": []byte("pypi-proxy-package-two"),
	}
	hashes := make(map[string]string, len(payloads))
	for filename, payload := range payloads {
		hashes[filename] = fmt.Sprintf("%x", sha256.Sum256(payload))
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/demo/":
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			_, _ = fmt.Fprintf(w, `{"files":[{"filename":"demo-1.0.0.whl","url":"%s/files/demo-1.0.0.whl","hashes":{"sha256":"%s"}},{"filename":"demo-2.0.0.whl","url":"%s/files/demo-2.0.0.whl","hashes":{"sha256":"%s"}}]}`,
				server.URL, hashes["demo-1.0.0.whl"], server.URL, hashes["demo-2.0.0.whl"])
		case "/files/demo-1.0.0.whl", "/files/demo-2.0.0.whl":
			_, _ = w.Write(payloads[r.URL.Path[len("/files/"):]])
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	config, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{RemoteURL: server.URL})
	if err != nil {
		t.Fatalf("编码 proxy 配置：%v", err)
	}
	repoID, err := repos.Create("pypi-proxy", "pypi", "proxy", "private", config)
	if err != nil {
		t.Fatalf("创建 proxy 仓库：%v", err)
	}
	mutator, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建资产协调器：%v", err)
	}
	assetService := NewAssetService(repos, assets, blobs, upstream.NewTestClient(time.Second))
	assetService.SetMutationCoordinator(mutator)
	service := NewFormatMetadataService(assetService, repos, metadata, upstream.NewTestClient(time.Second))
	if _, err := db.Exec(`CREATE TRIGGER reject_pypi_metadata BEFORE INSERT ON format_metadata WHEN NEW.filename='demo-2.0.0.whl' BEGIN SELECT RAISE(ABORT, '注入 PyPI 元数据失败'); END`); err != nil {
		t.Fatalf("创建失败注入：%v", err)
	}
	if _, err := service.PyPIFiles("pypi-proxy", "demo"); err == nil {
		t.Fatal("PyPI proxy 元数据失败必须返回错误")
	}
	for filename, hash := range hashes {
		if _, err := assets.GetByPath(repoID, "pypi/packages/demo/"+filename); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("PyPI proxy 元数据批次失败不得留下包资产：%v", err)
		}
		if blobs.Exists(hash) {
			t.Fatal("PyPI proxy 元数据批次失败不得留下无引用 blob")
		}
	}
}

func TestNuGetProxyMetadataFailureDoesNotLeavePartialMetadata(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	metadata := repository.NewFormatMetadataRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	repoID, err := repos.Create("nuget-proxy", "nuget", "proxy", "private", "")
	if err != nil {
		t.Fatalf("创建 proxy 仓库：%v", err)
	}
	index := []byte(`{"versions":["1.0.0","2.0.0"]}`)
	hash, sha1sum, md5sum, size, err := blobs.Put(bytes.NewReader(index))
	if err != nil {
		t.Fatalf("写入 NuGet 索引 blob：%v", err)
	}
	if err := assets.Upsert(repoID, "v3-flatcontainer/demo/index.json", hash, size, "application/json", sha1sum, md5sum); err != nil {
		t.Fatalf("写入 NuGet 索引资产：%v", err)
	}
	assetService := NewAssetService(repos, assets, blobs, nil)
	service := NewFormatMetadataService(assetService, repos, metadata)
	if _, err := db.Exec(`CREATE TRIGGER reject_second_nuget_metadata BEFORE INSERT ON format_metadata WHEN NEW.version_normalized='2.0.0' BEGIN SELECT RAISE(ABORT, '注入 NuGet 元数据失败'); END`); err != nil {
		t.Fatalf("创建失败注入：%v", err)
	}
	if _, err := service.NuGetPackages("nuget-proxy", "Demo"); err == nil {
		t.Fatal("NuGet proxy 第二条元数据失败必须返回错误")
	}
	items, err := metadata.List(repoID, "nuget", "demo")
	if err != nil {
		t.Fatalf("读取 NuGet 元数据：%v", err)
	}
	if len(items) != 0 {
		t.Fatalf("NuGet proxy 元数据批次失败不得部分可见：%d", len(items))
	}
}

func TestNuGetProxyDiscoversV3ResourcesAndCachesLocalMetadata(t *testing.T) {
	var serviceIndexHits, flatIndexHits, registrationHits, packageHits int
	proxyPackage := nugetProxyTestPackage(t, "Demo.Package", "1.0.0")
	var upstreamServer *httptest.Server
	upstreamServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/index.json":
			serviceIndexHits++
			_ = json.NewEncoder(w).Encode(map[string]any{"resources": []map[string]string{
				{"@id": upstreamServer.URL + "/package-base/", "@type": "PackageBaseAddress/3.0.0"},
				{"@id": upstreamServer.URL + "/registration/", "@type": "RegistrationsBaseUrl/3.6.0"},
			}})
		case "/package-base/demo.package/index.json":
			flatIndexHits++
			_, _ = w.Write([]byte(`{"versions":["1.0.0"]}`))
		case "/registration/demo.package/index.json":
			registrationHits++
			_, _ = w.Write([]byte(`{"items":[{"catalogEntry":{"id":"Demo.Package","version":"1.0.0","dependencyGroups":[{"targetFramework":"net8.0","dependencies":[{"id":"Child.Package","range":"[2.0.0]"}]}]}}]}`))
		case "/package-base/demo.package/1.0.0/demo.package.1.0.0.nupkg":
			packageHits++
			_, _ = w.Write(proxyPackage)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstreamServer.Close()

	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	metadata := repository.NewFormatMetadataRepo(db)
	config, err := json.Marshal(map[string]string{"remoteUrl": upstreamServer.URL + "/v3/index.json"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repos.Create("nuget-proxy-discovery", "nuget", "proxy", "private", string(config)); err != nil {
		t.Fatalf("创建 NuGet proxy：%v", err)
	}
	assetService := NewAssetService(repos, assets, blobstore.NewStore(t.TempDir()), upstream.NewTestClient(time.Second))
	service := NewFormatMetadataService(assetService, repos, metadata, upstream.NewTestClient(time.Second))

	packages, err := service.NuGetPackages("nuget-proxy-discovery", "Demo.Package")
	if err != nil || len(packages) != 1 {
		t.Fatalf("发现 NuGet proxy 元数据失败：packages=%+v err=%v", packages, err)
	}
	if !bytes.Contains([]byte(packages[0].MetadataJSON), []byte(`"Child.Package"`)) {
		t.Fatalf("registration 依赖未进入本地元数据：%s", packages[0].MetadataJSON)
	}
	if _, rc, err := service.ResolveNuGet(context.Background(), "nuget-proxy-discovery", "Demo.Package", "1.0.0", packages[0].Filename); err != nil {
		t.Fatalf("缓存 NuGet proxy 包失败：%v", err)
	} else {
		_ = rc.Close()
	}
	if serviceIndexHits != 1 || flatIndexHits != 1 || registrationHits != 1 || packageHits != 1 {
		t.Fatalf("NuGet V3 资源发现或缓存次数错误：service=%d flat=%d registration=%d package=%d", serviceIndexHits, flatIndexHits, registrationHits, packageHits)
	}
}

func nugetProxyTestPackage(t *testing.T, id, version string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create(id + ".nuspec")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("<package><metadata><id>" + id + "</id><version>" + version + "</version></metadata></package>")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestNuGetProxySourceRejectsCredentialAndUnsafeURLForms(t *testing.T) {
	for _, source := range []string{
		`{"source":{"packageUrl":"https://user:secret@example.invalid/package.nupkg"}}`,
		`{"source":{"packageUrl":"https://example.invalid/package.nupkg?token=secret"}}`,
		`{"source":{"packageUrl":"file:///package.nupkg"}}`,
	} {
		if _, err := nugetProxySourceURL(source); !errors.Is(err, ErrUpstream) {
			t.Fatalf("不安全 NuGet 上游地址必须拒绝：source=%s err=%v", source, err)
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatalf("上游拒绝错误不得泄露凭据：%v", err)
		}
	}
}

func TestNuGetProxyRejectsPrivateUpstreamWithoutCredentialLeak(t *testing.T) {
	var hits int
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer upstreamServer.Close()
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_PRIVATE_NUGET", "test-value")
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	metadata := repository.NewFormatMetadataRepo(db)
	config, err := json.Marshal(map[string]string{"remoteUrl": upstreamServer.URL + "/v3/index.json", "credentialRef": "PRIVATE_NUGET"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repos.Create("nuget-private-upstream", "nuget", "proxy", "private", string(config)); err != nil {
		t.Fatalf("创建 NuGet proxy：%v", err)
	}
	service := NewFormatMetadataService(NewAssetService(repos, assets, blobstore.NewStore(t.TempDir()), upstream.NewClient(time.Second)), repos, metadata, upstream.NewClient(time.Second))
	if _, err := service.NuGetPackages("nuget-private-upstream", "Demo.Package"); err == nil {
		t.Fatal("私网 NuGet 上游必须拒绝")
	} else if strings.Contains(err.Error(), "test-value") {
		t.Fatalf("私网上游拒绝错误不得泄露凭据：%v", err)
	}
	if hits != 0 {
		t.Fatalf("私网 NuGet 上游不得被实际访问：hits=%d", hits)
	}
}

func TestNuGetProxyDoesNotForwardCredentialToCrossOriginResource(t *testing.T) {
	var mirrorAuthorization string
	packageBody := nugetProxyTestPackage(t, "Demo.Package", "1.0.0")
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorAuthorization = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/flat/demo.package/index.json":
			_, _ = w.Write([]byte(`{"versions":["1.0.0"]}`))
		case "/registration/demo.package/index.json":
			_, _ = w.Write([]byte(`{"items":[{"catalogEntry":{"version":"1.0.0"}}]}`))
		case "/flat/demo.package/1.0.0/demo.package.1.0.0.nupkg":
			_, _ = w.Write(packageBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mirror.Close()
	var originAuthorization string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originAuthorization = r.Header.Get("Authorization")
		if r.URL.Path != "/v3/index.json" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": []map[string]string{
			{"@id": mirror.URL + "/flat/", "@type": "PackageBaseAddress/3.0.0"},
			{"@id": mirror.URL + "/registration/", "@type": "RegistrationsBaseUrl/3.6.0"},
		}})
	}))
	defer origin.Close()

	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_NUGET_PRIVATE", "test-value")
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	metadata := repository.NewFormatMetadataRepo(db)
	config, err := json.Marshal(map[string]string{"remoteUrl": origin.URL + "/v3/index.json", "credentialRef": "NUGET_PRIVATE"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repos.Create("nuget-cross-origin", "nuget", "proxy", "private", string(config)); err != nil {
		t.Fatalf("创建 NuGet proxy：%v", err)
	}
	client := upstream.NewTestClient(time.Second)
	service := NewFormatMetadataService(NewAssetService(repos, assets, blobstore.NewStore(t.TempDir()), client), repos, metadata, client)
	packages, err := service.NuGetPackages("nuget-cross-origin", "Demo.Package")
	if err != nil || len(packages) != 1 {
		t.Fatalf("加载 NuGet proxy 元数据失败：packages=%+v err=%v", packages, err)
	}
	if _, rc, err := service.ResolveNuGet(context.Background(), "nuget-cross-origin", "Demo.Package", "1.0.0", packages[0].Filename); err != nil {
		t.Fatalf("缓存 NuGet proxy 包失败：%v", err)
	} else {
		_ = rc.Close()
	}
	if originAuthorization == "" {
		t.Fatal("配置源必须收到凭据")
	}
	if mirrorAuthorization != "" {
		t.Fatalf("跨 origin NuGet 资源不得收到凭据：%q", mirrorAuthorization)
	}
	if strings.Contains(packages[0].MetadataJSON, "test-value") {
		t.Fatalf("NuGet proxy 本地元数据不得保存凭据：%s", packages[0].MetadataJSON)
	}
}
