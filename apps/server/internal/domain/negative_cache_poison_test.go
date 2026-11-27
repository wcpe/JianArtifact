package domain_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// TestMavenSiblingPomPreventsJarNegativeCachePoisoning 复现并验证「有 pom 无 jar」毒缓存防护：
// 当 com.mojang:logging 或 ink.ptms.core:v12004 的 .pom 已缓存（非 packaging=pom），
// 随后 .jar / -universal.jar 遭遇上游瞬时 404 时，绝不写入 404 负缓存；上游恢复后立即拉取成功。
// 同时验证 -sources.jar 与 packaging=pom 的模块仍正常负缓存，不劣化 IDE 探测延迟。
func TestMavenSiblingPomPreventsJarNegativeCachePoisoning(t *testing.T) {
	t.Parallel()
	var jarReady atomic.Bool
	var jarHits, universalHits, sourcesHits, pomPkgJarHits atomic.Int32

	const jarPom = `<project><modelVersion>4.0.0</modelVersion><groupId>com.mojang</groupId><artifactId>logging</artifactId><version>1.2.7</version></project>`
	const bomPom = `<project><modelVersion>4.0.0</modelVersion><groupId>com.example</groupId><artifactId>bom</artifactId><version>1.0.0</version><packaging>pom</packaging></project>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/com/mojang/logging/1.2.7/logging-1.2.7.pom":
			_, _ = w.Write([]byte(jarPom))
		case "/com/mojang/logging/1.2.7/logging-1.2.7.jar":
			jarHits.Add(1)
			if !jarReady.Load() {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte("REAL-MOJANG-JAR"))
		case "/com/mojang/logging/1.2.7/logging-1.2.7-universal.jar":
			universalHits.Add(1)
			if !jarReady.Load() {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte("REAL-UNIVERSAL-JAR"))
		case "/com/mojang/logging/1.2.7/logging-1.2.7-sources.jar":
			sourcesHits.Add(1)
			http.NotFound(w, r)
		case "/com/example/bom/1.0.0/bom-1.0.0.pom":
			_, _ = w.Write([]byte(bomPom))
		case "/com/example/bom/1.0.0/bom-1.0.0.jar":
			pomPkgJarHits.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	svc.SetNegativeCacheTTL(time.Hour)
	if _, err := repos.Create("mc-proxy", "maven", "proxy", "public", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("创建 proxy 仓库：%v", err)
	}
	if _, err := repos.Create("maven-public", "maven", "group", "public", groupConfigJSON(t, "mc-proxy")); err != nil {
		t.Fatalf("创建 group 仓库：%v", err)
	}

	// 1. 先解析 POM 并落盘缓存（模拟 Gradle 先拉到 POM）。
	_, rc, err := svc.Resolve(context.Background(), "maven-public", "com/mojang/logging/1.2.7/logging-1.2.7.pom")
	if err != nil {
		t.Fatalf("拉取 POM 失败：%v", err)
	}
	_ = rc.Close()

	// 2. 上游瞬时 404：主 JAR 与运行时分类 JAR（-universal.jar）均返回 404，但不得写入负缓存。
	if _, _, err := svc.Resolve(context.Background(), "maven-public", "com/mojang/logging/1.2.7/logging-1.2.7.jar"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("首次 JAR 应返回 ErrNotFound，得 %v", err)
	}
	if _, _, err := svc.Resolve(context.Background(), "maven-public", "com/mojang/logging/1.2.7/logging-1.2.7-universal.jar"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("首次 universal JAR 应返回 ErrNotFound，得 %v", err)
	}

	// 3. 上游恢复后无需等 1 小时 TTL，立即回源成功！
	jarReady.Store(true)
	_, rc, err = svc.Resolve(context.Background(), "maven-public", "com/mojang/logging/1.2.7/logging-1.2.7.jar")
	if err != nil {
		t.Fatalf("上游恢复后主 JAR 应立即成功，不应被负缓存锁死：%v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "REAL-MOJANG-JAR" || jarHits.Load() != 2 {
		t.Fatalf("主 JAR 内容=%q hits=%d，期望 REAL-MOJANG-JAR / 2", string(body), jarHits.Load())
	}

	_, rc, err = svc.Resolve(context.Background(), "maven-public", "com/mojang/logging/1.2.7/logging-1.2.7-universal.jar")
	if err != nil {
		t.Fatalf("上游恢复后 universal JAR 应立即成功：%v", err)
	}
	_ = rc.Close()
	if universalHits.Load() != 2 {
		t.Fatalf("universal JAR hits=%d，期望 2", universalHits.Load())
	}

	// 4. 可选附件 -sources.jar 仍正常命中负缓存（两次请求只打一次上游）。
	for i := 0; i < 2; i++ {
		if _, _, err := svc.Resolve(context.Background(), "maven-public", "com/mojang/logging/1.2.7/logging-1.2.7-sources.jar"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("sources.jar 应返回 ErrNotFound，得 %v", err)
		}
	}
	if sourcesHits.Load() != 1 {
		t.Fatalf("sources.jar 应命中负缓存只回源 1 次，实际 %d", sourcesHits.Load())
	}

	// 5. packaging=pom 的模块本就无 jar，其 jar 404 允许正常负缓存。
	_, rc, err = svc.Resolve(context.Background(), "maven-public", "com/example/bom/1.0.0/bom-1.0.0.pom")
	if err != nil {
		t.Fatalf("拉取 bom POM 失败：%v", err)
	}
	_ = rc.Close()
	for i := 0; i < 2; i++ {
		if _, _, err := svc.Resolve(context.Background(), "maven-public", "com/example/bom/1.0.0/bom-1.0.0.jar"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("bom.jar 应返回 ErrNotFound，得 %v", err)
		}
	}
	if pomPkgJarHits.Load() != 1 {
		t.Fatalf("packaging=pom 的 jar 应命中负缓存只回源 1 次，实际 %d", pomPkgJarHits.Load())
	}
}

// TestGroupNegativeCacheCascadingInvalidationAndNestedGroup 验证：
//  1. 成员上传/清理负缓存时级联失效父级 group 的 404 负缓存；
//  2. 嵌套 group 成员（group -> sub-group -> proxy）可正确解析，不再错误 404 + 毒缓存；
//  3. proxy 仓库级 negativeCacheTtlSeconds=0 同时禁止自身与父级 group 写入负缓存；
//  4. WithBypassNegativeCache（Cache-Control: no-cache）可穿透负缓存直连上游。
func TestGroupNegativeCacheCascadingInvalidationAndNestedGroup(t *testing.T) {
	t.Parallel()
	var ready atomic.Bool
	var upstreamHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if !ready.Load() {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("FRESH-ARTIFACT"))
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	svc.SetNegativeCacheTTL(time.Hour)

	if _, err := repos.Create("hosted-a", "raw", "hosted", "public", "{}"); err != nil {
		t.Fatalf("创建 hosted：%v", err)
	}
	proxyID, err := repos.Create("proxy-a", "raw", "proxy", "public", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("创建 proxy：%v", err)
	}
	if _, err := repos.Create("sub-group", "raw", "group", "public", groupConfigJSON(t, "proxy-a")); err != nil {
		t.Fatalf("创建 sub-group：%v", err)
	}
	if _, err := repos.Create("top-group", "raw", "group", "public", groupConfigJSON(t, "hosted-a", "sub-group")); err != nil {
		t.Fatalf("创建 top-group：%v", err)
	}

	// 1. 首次经嵌套 top-group -> sub-group -> proxy-a 确认 404（写负缓存）。
	if _, _, err := svc.Resolve(context.Background(), "top-group", "a/b.bin"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，得 %v", err)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("嵌套 group 应穿透到 proxy-a，上游命中应为 1，实际 %d", upstreamHits.Load())
	}

	// 2. 往 hosted-a 上传同名文件后，父级 top-group 的负缓存必须立即级联失效！
	if _, err := svc.Put("hosted-a", "a/b.bin", bytes.NewReader([]byte("FROM-HOSTED")), "application/octet-stream"); err != nil {
		t.Fatalf("上传 hosted 失败：%v", err)
	}
	_, rc, err := svc.Resolve(context.Background(), "top-group", "a/b.bin")
	if err != nil {
		t.Fatalf("成员上传后父级 group 负缓存未失效：%v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "FROM-HOSTED" {
		t.Fatalf("内容 = %q，期望 FROM-HOSTED", string(got))
	}

	// 3. 另一路径先 404 进负缓存，随后按 proxy-a 清理负缓存 -> 父级 sub-group/top-group 也级联失效！
	if _, _, err := svc.Resolve(context.Background(), "top-group", "c/d.bin"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，得 %v", err)
	}
	ready.Store(true)
	if removed := svc.PurgeNegativeCache(domain.NegativeCacheFilter{RepositoryID: proxyID}); removed < 2 {
		t.Fatalf("按成员清负缓存应级联清理父级 group，实际删除 %d 条", removed)
	}
	_, rc, err = svc.Resolve(context.Background(), "top-group", "c/d.bin")
	if err != nil {
		t.Fatalf("级联清负缓存后嵌套 group 应立即回源成功：%v", err)
	}
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "FRESH-ARTIFACT" {
		t.Fatalf("内容 = %q，期望 FRESH-ARTIFACT", string(got))
	}

	// 4. WithBypassNegativeCache 穿透负缓存。
	ready.Store(false)
	if _, _, err := svc.Resolve(context.Background(), "proxy-a", "bypass.bin"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，得 %v", err)
	}
	ready.Store(true)
	_, rc, err = svc.Resolve(domain.WithBypassNegativeCache(context.Background()), "proxy-a", "bypass.bin")
	if err != nil {
		t.Fatalf("WithBypassNegativeCache 应穿透负缓存并回源成功：%v", err)
	}
	_ = rc.Close()

	// 5. 仓库级 negativeCacheTtlSeconds=0 禁止写负缓存（含父级 group）。
	zero := 0
	cfgZero, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{
		RemoteURL:               srv.URL,
		NegativeCacheTTLSeconds: &zero,
	})
	if err != nil {
		t.Fatalf("编码配置失败：%v", err)
	}
	if _, err := repos.Create("proxy-no-neg", "raw", "proxy", "public", cfgZero); err != nil {
		t.Fatalf("创建 proxy-no-neg：%v", err)
	}
	if _, err := repos.Create("group-no-neg", "raw", "group", "public", groupConfigJSON(t, "proxy-no-neg")); err != nil {
		t.Fatalf("创建 group-no-neg：%v", err)
	}
	ready.Store(false)
	before := upstreamHits.Load()
	for i := 0; i < 2; i++ {
		if _, _, err := svc.Resolve(context.Background(), "group-no-neg", "always-probe.bin"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("期望 ErrNotFound，得 %v", err)
		}
	}
	if upstreamHits.Load()-before != 2 {
		t.Fatalf("negativeCacheTtlSeconds=0 下连续两次请求都应回源，实际回源 %d 次", upstreamHits.Load()-before)
	}
}

// TestOutboundProxyServiceAndPerRepoRouting 验证全局出站代理 CRUD、密文落库、引用删除保护与 proxy 仓库挂载回源。
func TestOutboundProxyServiceAndPerRepoRouting(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	proxies := repository.NewOutboundProxyRepo(db)

	key := bytes.Repeat([]byte{0x42}, 32)
	mihomoMgr := upstream.NewMihomoManager("", t.TempDir())
	proxySvc, err := domain.NewOutboundProxyService(proxies, repos, key, mihomoMgr)
	if err != nil {
		t.Fatalf("构造 OutboundProxyService 失败：%v", err)
	}
	defer proxySvc.Close()

	// 模拟代理服务器（拦截所有通过代理的请求并返回固定载荷）。
	var proxyHits atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("PROXIED:" + r.URL.Path))
	}))
	defer proxySrv.Close()

	// 1. 创建 HTTP 出站代理（含认证口令，验证密文不含明文且预览脱敏）。
	created, err := proxySvc.Create(domain.CreateOutboundProxyInput{
		Name:        "hk-http",
		Kind:        repository.OutboundProxyKindHTTP,
		Description: "MC 海外代理",
		Config:      proxySrv.URL,
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("创建出站代理失败：%v", err)
	}
	if created.EndpointPreview == "" || created.Kind != "http" {
		t.Fatalf("代理视图异常：%+v", created)
	}

	// 2. 创建 Mihomo 节点（VLESS Reality 分享链接），验证密文不含公钥/UUID、预览脱敏。
	mihomoView, err := proxySvc.Create(domain.CreateOutboundProxyInput{
		Name:    "hk-vless",
		Kind:    repository.OutboundProxyKindMihomo,
		Config:  "vless://aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee@hk.example.com:443?security=reality&sni=www.microsoft.com&pbk=SECRET_PBK_VALUE#HK",
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("创建 Mihomo 代理失败：%v", err)
	}
	if mihomoView.EndpointPreview != "vless://hk.example.com:443 (reality)" {
		t.Fatalf("Mihomo 预览异常 = %q", mihomoView.EndpointPreview)
	}
	rawRow, err := proxies.GetByID(mihomoView.ID)
	if err != nil {
		t.Fatalf("查库失败：%v", err)
	}
	if bytes.Contains(rawRow.ConfigCiphertext, []byte("SECRET_PBK_VALUE")) ||
		bytes.Contains(rawRow.ConfigCiphertext, []byte("aaaaaaaa-bbbb")) {
		t.Fatal("数据库密文列不得包含明文密钥或 UUID")
	}

	// 3. 挂载到 proxy 仓库并通过 AssetService 回源，验证流量经由该出站代理转发。
	upClient := upstream.NewClient(5 * time.Second)
	upClient.SetProxyResolver(proxySvc.ResolveTransport)
	assetSvc := domain.NewAssetService(repos, assets, blobs, upClient)

	repoCfg, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{
		RemoteURL:       "http://libraries.minecraft.example",
		OutboundProxyID: created.ID,
	})
	if err != nil {
		t.Fatalf("编码仓库配置失败：%v", err)
	}
	if _, err := repos.Create("mc-proxied", "maven", "proxy", "public", repoCfg); err != nil {
		t.Fatalf("创建挂载代理的 proxy 仓库失败：%v", err)
	}

	_, rc, err := assetSvc.Resolve(context.Background(), "mc-proxied", "com/mojang/logging/1.2.7/logging-1.2.7.jar")
	if err != nil {
		t.Fatalf("经出站代理拉取制品失败：%v", err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if proxyHits.Load() != 1 || string(data) != "PROXIED:/com/mojang/logging/1.2.7/logging-1.2.7.jar" {
		t.Fatalf("代理未生效：hits=%d body=%q", proxyHits.Load(), string(data))
	}

	// 4. 仍有仓库引用时禁止删除（ErrConflict）。
	if err := proxySvc.Delete(created.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("被仓库引用时删除应返回 ErrConflict，得 %v", err)
	}

	// 5. 连通性测试复用同一代理链路。
	res, err := proxySvc.TestConnectivity(context.Background(), created.ID, "http://libraries.minecraft.example/", upClient)
	if err != nil || !res.OK || res.StatusCode != 200 {
		t.Fatalf("连通性测试失败：res=%+v err=%v", res, err)
	}
}
