package domain_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// proxyConfigJSON 构造仅含 remoteUrl 的仓库配置 JSON。
func proxyConfigJSON(t *testing.T, remoteURL string) string {
	t.Helper()
	s, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{RemoteURL: remoteURL})
	if err != nil {
		t.Fatalf("编码 proxy 配置：%v", err)
	}
	return s
}

// proxyConfigWithCredentialJSON 构造带运行时凭据引用的 proxy 配置。
func proxyConfigWithCredentialJSON(t *testing.T, remoteURL, credentialRef string) string {
	t.Helper()
	s, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{
		RemoteURL: remoteURL, CredentialRef: credentialRef,
	})
	if err != nil {
		t.Fatalf("编码带凭据的 proxy 配置：%v", err)
	}
	return s
}

// groupConfigJSON 构造仅含 members 的仓库配置 JSON。
func groupConfigJSON(t *testing.T, members ...string) string {
	t.Helper()
	s, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{Members: members})
	if err != nil {
		t.Fatalf("编码 group 配置：%v", err)
	}
	return s
}

// readClose 读取并关闭一个可读流，返回其全部字节。
func readClose(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读取内容：%v", err)
	}
	return b
}

func TestResolveProxyCacheMissThenHit(t *testing.T) {
	t.Parallel()
	var hits int32
	payload := []byte("upstream artifact bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/java-archive")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	// 首次：未命中 → 回源。
	asset, rc, err := svc.Resolve(context.Background(), "raw-proxy", "lib/foo.jar")
	if err != nil {
		t.Fatalf("首次 Resolve：%v", err)
	}
	if got := readClose(t, rc); !bytes.Equal(got, payload) {
		t.Fatalf("回源内容不符：%q", got)
	}
	if asset.ContentType != "application/java-archive" {
		t.Fatalf("Content-Type 未透传：%q", asset.ContentType)
	}

	// 再次：本地缓存命中，不应再回源。
	_, rc2, err := svc.Resolve(context.Background(), "raw-proxy", "lib/foo.jar")
	if err != nil {
		t.Fatalf("二次 Resolve：%v", err)
	}
	if got := readClose(t, rc2); !bytes.Equal(got, payload) {
		t.Fatalf("缓存命中内容不符：%q", got)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("期望仅回源 1 次，实际 %d 次", n)
	}
}

func TestResolveProxyUsesCredentialRefWithoutLeakage(t *testing.T) {
	const credentialRef = "PRIVATE_ARTIFACTS"
	const credential = "release-user:very-secret-password"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		user, password, ok := r.BasicAuth()
		if !ok || user != "release-user" || password != "very-secret-password" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("private artifact"))
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("private-proxy", "raw", "proxy", "private", proxyConfigWithCredentialJSON(t, srv.URL, credentialRef)); err != nil {
		t.Fatalf("建私有 proxy 仓库：%v", err)
	}
	_, rc, err := svc.Resolve(context.Background(), "private-proxy", "secure.jar")
	if err != nil {
		t.Fatalf("带 credentialRef 的回源应成功：%v", err)
	}
	if got := string(readClose(t, rc)); got != "private artifact" {
		t.Fatalf("私有上游内容 = %q", got)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("私有上游请求次数 = %d，期望 1", got)
	}
}

func TestResolveProxyRejectsUnknownCredentialRefWithoutLeakage(t *testing.T) {
	t.Parallel()
	const missingRef = "MISSING_PRIVATE_ARTIFACTS"
	const secret = "must-not-appear-in-error"
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(secret))
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("missing-credential-proxy", "raw", "proxy", "private", proxyConfigWithCredentialJSON(t, srv.URL, missingRef)); err != nil {
		t.Fatalf("建带未知凭据引用的 proxy 仓库：%v", err)
	}
	_, _, err := svc.Resolve(context.Background(), "missing-credential-proxy", "secure.jar")
	if !errors.Is(err, domain.ErrUpstream) {
		t.Fatalf("未知 credentialRef 应安全回源失败，实际：%v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误不得泄露凭据：%v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("未知 credentialRef 不得访问上游，实际请求 %d 次", got)
	}
}

// 目录形路径（空串或以 / 结尾）不是制品：proxy 不得回源，否则上游返回的 HTML
// 目录索引页会被当成制品缓存（path 以 / 结尾、text/html），前端文件树出现空白名假文件。
func TestResolveRejectsDirectoryLikePath(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>directory index</html>"))
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}
	for _, p := range []string{"", "com/alibaba/druid/", "com/alibaba/druid/1.2.9/"} {
		if _, _, err := svc.Resolve(context.Background(), "raw-proxy", p); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("目录形路径 %q 应返回 ErrNotFound，实际：%v", p, err)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("目录形路径不应回源，实际回源 %d 次", n)
	}
}

func TestResolveProxyUpstreamNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}
	if _, _, err := svc.Resolve(context.Background(), "raw-proxy", "missing.jar"); err != domain.ErrNotFound {
		t.Fatalf("上游 404 应返回 ErrNotFound，实际：%v", err)
	}
}

func TestResolveProxyUpstreamError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}
	if _, _, err := svc.Resolve(context.Background(), "raw-proxy", "boom.jar"); !errorsIsUpstream(err) {
		t.Fatalf("上游 5xx 应返回 ErrUpstream，实际：%v", err)
	}
}

func TestResolveGroupOrderedHit(t *testing.T) {
	t.Parallel()
	svc, repos := newAssetService(t)
	for _, name := range []string{"raw-a", "raw-b"} {
		if _, err := repos.Create(name, "raw", "hosted", "private", ""); err != nil {
			t.Fatalf("建成员仓库 %s：%v", name, err)
		}
	}
	// dup.txt 两成员皆有，内容不同：group 应返回首成员（raw-a）的内容。
	if _, err := svc.Put("raw-a", "dup.txt", bytes.NewReader([]byte("from-a")), "text/plain"); err != nil {
		t.Fatalf("Put raw-a dup：%v", err)
	}
	if _, err := svc.Put("raw-b", "dup.txt", bytes.NewReader([]byte("from-b")), "text/plain"); err != nil {
		t.Fatalf("Put raw-b dup：%v", err)
	}
	// only-b.txt 仅次成员有：group 应回退命中 raw-b。
	if _, err := svc.Put("raw-b", "only-b.txt", bytes.NewReader([]byte("only-b")), "text/plain"); err != nil {
		t.Fatalf("Put raw-b only：%v", err)
	}
	if _, err := repos.Create("raw-group", "raw", "group", "private", groupConfigJSON(t, "raw-a", "raw-b")); err != nil {
		t.Fatalf("建 group 仓库：%v", err)
	}

	_, rc, err := svc.Resolve(context.Background(), "raw-group", "dup.txt")
	if err != nil {
		t.Fatalf("Resolve dup：%v", err)
	}
	if got := readClose(t, rc); string(got) != "from-a" {
		t.Fatalf("group 有序命中应取首成员，实际：%q", got)
	}

	_, rc2, err := svc.Resolve(context.Background(), "raw-group", "only-b.txt")
	if err != nil {
		t.Fatalf("Resolve only-b：%v", err)
	}
	if got := readClose(t, rc2); string(got) != "only-b" {
		t.Fatalf("group 应回退命中次成员，实际：%q", got)
	}

	if _, _, err := svc.Resolve(context.Background(), "raw-group", "nope.txt"); err != domain.ErrNotFound {
		t.Fatalf("group 全未命中应返回 ErrNotFound，实际：%v", err)
	}
}

func TestResolveGroupDuplicateMemberReusesSnapshotOrder(t *testing.T) {
	t.Parallel()
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("group-member"))
	}))
	defer server.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("snapshot-proxy", "raw", "proxy", "private", proxyConfigJSON(t, server.URL)); err != nil {
		t.Fatalf("建 proxy 成员：%v", err)
	}
	if _, err := repos.Create("snapshot-group", "raw", "group", "private", groupConfigJSON(t, "snapshot-proxy", "snapshot-proxy")); err != nil {
		t.Fatalf("建重复成员 group：%v", err)
	}
	_, rc, err := svc.Resolve(context.Background(), "snapshot-group", "a.bin")
	if err != nil {
		t.Fatalf("group 解析：%v", err)
	}
	if got := string(readClose(t, rc)); got != "group-member" {
		t.Fatalf("group 内容不符：%q", got)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("重复成员应复用单次回源，实际 %d 次", got)
	}
}

func TestResolveProxySingleFlight(t *testing.T) {
	t.Parallel()
	var hits int32
	payload := []byte("single-flight payload")
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release // 阻塞回源，制造并发窗口。
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	bodies := make([][]byte, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, rc, err := svc.Resolve(context.Background(), "raw-proxy", "concurrent.jar")
			if err != nil {
				errs[idx] = err
				return
			}
			bodies[idx], _ = io.ReadAll(rc)
			_ = rc.Close()
		}(i)
	}
	close(start)
	// 给所有 goroutine 进入 single-flight 临界区留出时间，再放行上游响应。
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发 Resolve[%d] 失败：%v", i, err)
		}
		if !bytes.Equal(bodies[i], payload) {
			t.Fatalf("并发 Resolve[%d] 内容不符：%q", i, bodies[i])
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("single-flight 应仅回源 1 次，实际 %d 次", got)
	}
}

// errorsIsUpstream 报告 err 是否为 ErrUpstream（含包装）。
func errorsIsUpstream(err error) bool {
	return err != nil && errors.Is(err, domain.ErrUpstream)
}

// TestResolveGroupFast404WithHangingMember 验证 group 并行回源：一个 proxy 成员上游挂起
// （不响应直到超时），另一个快速 404；整体应在秒级返回，而非串行等 2 个超时。
//
// 返回码口径：FIX-1 起，成员超数属「不可判定」且是超时，整体返回 ErrUpstreamTimeout（对外 504），
// 不再折叠成 ErrNotFound——上游只是慢并不证明制品不存在（旧断言 err == ErrNotFound 正是本次要修的缺陷）。
// 本用例守护的是「并行 + 秒级返回」，超时成员依旧计入不可判定，因此断言同时要求「不是 404」。
func TestResolveGroupFast404WithHangingMember(t *testing.T) {
	t.Parallel()
	hangSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hangSrv.Close()
	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer fastSrv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("hang-proxy", "raw", "proxy", "private", proxyConfigJSON(t, hangSrv.URL)); err != nil {
		t.Fatalf("建 hang-proxy：%v", err)
	}
	if _, err := repos.Create("fast-proxy", "raw", "proxy", "private", proxyConfigJSON(t, fastSrv.URL)); err != nil {
		t.Fatalf("建 fast-proxy：%v", err)
	}
	if _, err := repos.Create("g-fast", "raw", "group", "private", groupConfigJSON(t, "hang-proxy", "fast-proxy")); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	start := time.Now()
	_, _, err := svc.Resolve(context.Background(), "g-fast", "missing.jar")
	elapsed := time.Since(start)
	if !errors.Is(err, domain.ErrUpstreamTimeout) {
		t.Fatalf("挂起成员应判为超时（ErrUpstreamTimeout），实际：%v（耗时 %s）", err, elapsed)
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("不可判定的超时不得折叠成 ErrNotFound，实际：%v（耗时 %s）", err, elapsed)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("group 缺失应快速失败，实际耗时 %s（并行未生效？）", elapsed)
	}
}

// TestResolveGroupFuseSkipsUnreachableMember 验证熔断：proxy 上游 5xx 失败后进入短窗熔断，
// 后续 group 请求直接跳过该成员（不再请求上游），快速失败。
//
// 返回码口径：FIX-1 起，成员回源 5xx 属「不可判定（非超时）」，整体返回 ErrUpstream（对外 502），
// 不再是 ErrNotFound；本用例守护的是「5xx 后熔断、窗口内零连接」。
func TestResolveGroupFuseSkipsUnreachableMember(t *testing.T) {
	t.Parallel()
	var hits int32
	boomSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer boomSrv.Close()
	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer fastSrv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("boom-proxy", "raw", "proxy", "private", proxyConfigJSON(t, boomSrv.URL)); err != nil {
		t.Fatalf("建 boom-proxy：%v", err)
	}
	if _, err := repos.Create("fast-proxy", "raw", "proxy", "private", proxyConfigJSON(t, fastSrv.URL)); err != nil {
		t.Fatalf("建 fast-proxy：%v", err)
	}
	if _, err := repos.Create("g-fuse", "raw", "group", "private", groupConfigJSON(t, "boom-proxy", "fast-proxy")); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "g-fuse", "x.jar"); !errorsIsUpstream(err) {
		t.Fatalf("首次应回源失败（ErrUpstream），实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("首次应回源 boom 上游 1 次，实际 %d", n)
	}
	if _, _, err := svc.Resolve(context.Background(), "g-fuse", "x.jar"); !errorsIsUpstream(err) {
		t.Fatalf("二次应仍为 ErrUpstream（仍不可判定），实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("熔断后不应再请求 boom 上游，实际命中 %d 次", n)
	}
}

// TestResolveGroupParallelHitFromLaterMember 验证并行回源：后一个 proxy 成员能命中并返回内容。
func TestResolveGroupParallelHitFromLaterMember(t *testing.T) {
	t.Parallel()
	payload := []byte("hit-from-fast")
	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/java-archive")
		_, _ = w.Write(payload)
	}))
	defer fastSrv.Close()
	hangSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hangSrv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("hang-proxy", "raw", "proxy", "private", proxyConfigJSON(t, hangSrv.URL)); err != nil {
		t.Fatalf("建 hang-proxy：%v", err)
	}
	if _, err := repos.Create("fast-proxy", "raw", "proxy", "private", proxyConfigJSON(t, fastSrv.URL)); err != nil {
		t.Fatalf("建 fast-proxy：%v", err)
	}
	if _, err := repos.Create("g-hit", "raw", "group", "private", groupConfigJSON(t, "hang-proxy", "fast-proxy")); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	start := time.Now()
	_, rc, err := svc.Resolve(context.Background(), "g-hit", "lib/hit.jar")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("应命中，实际：%v", err)
	}
	defer func() { _ = rc.Close() }()
	if got := readClose(t, rc); !bytes.Equal(got, payload) {
		t.Fatalf("内容不符：%q", got)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("后成员命中不应等挂起者超时，实际耗时 %s", elapsed)
	}
}

// ----------：proxy 上游 auto-block 主动探测恢复 ----------

// waitForStatus 轮询等待仓库上游状态变为 want（后台探测恢复验证用）。
func waitForStatus(t *testing.T, svc *domain.AssetService, repoID int64, want domain.RemoteStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if svc.Status(repoID).Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待上游状态 %s 超时，当前为 %s", want, svc.Status(repoID).Status)
}

// TestResolveProxyAutoBlockZeroConnection 验证 auto-block：上游 5xx 失败后进入 AUTO_BLOCKED，
// 阻止窗口内 group 请求对该成员零连接、快速失败。
// 返回码口径由 FIX-1 定为 ErrUpstream（502）：被熔断跳过的成员属不可判定，不再折叠成 404。
func TestResolveProxyAutoBlockZeroConnection(t *testing.T) {
	t.Parallel()
	var hits int32
	boomSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer boomSrv.Close()
	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer fastSrv.Close()

	svc, repos := newAssetService(t)
	svc.SetAutoBlockBase(10 * time.Second) // 窗口足够长，测试期间不失效
	boomID, err := repos.Create("boom-proxy", "raw", "proxy", "private", proxyConfigJSON(t, boomSrv.URL))
	if err != nil {
		t.Fatalf("建 boom-proxy：%v", err)
	}
	fastID, err := repos.Create("fast-proxy", "raw", "proxy", "private", proxyConfigJSON(t, fastSrv.URL))
	if err != nil {
		t.Fatalf("建 fast-proxy：%v", err)
	}
	if _, err := repos.Create("g-auto", "raw", "group", "private", groupConfigJSON(t, "boom-proxy", "fast-proxy")); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	// 首次：boom 回源失败 → AUTO_BLOCKED；fast 404 → group 不可判定（非超时）→ ErrUpstream（FIX-1）。
	if _, _, err := svc.Resolve(context.Background(), "g-auto", "x.jar"); !errorsIsUpstream(err) {
		t.Fatalf("首次应 ErrUpstream，实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("首次应回源 boom 上游 1 次，实际 %d", n)
	}
	if got := svc.Status(boomID).Status; got != domain.StatusAutoBlocked {
		t.Fatalf("boom 上游应 AUTO_BLOCKED，实际 %s", got)
	}
	if got := svc.Status(fastID).Status; got != domain.StatusReady {
		t.Fatalf("fast 上游应保持 READY，实际 %s", got)
	}

	// 二次（阻止窗口内）：零连接、快速失败（被熔断跳过的成员属不可判定 → ErrUpstream）。
	start := time.Now()
	if _, _, err := svc.Resolve(context.Background(), "g-auto", "x.jar"); !errorsIsUpstream(err) {
		t.Fatalf("二次应 ErrUpstream，实际：%v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("阻止窗口内应快速失败，实际耗时 %s", elapsed)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("阻止窗口内不应再请求 boom 上游，实际命中 %d 次", n)
	}
}

// TestResolveProxyAutoBlockRecovers 验证后台探测恢复：上游恢复后，后台 HEAD 探测成功，
// 状态自动回到 AVAILABLE，后续请求正常回源。
func TestResolveProxyAutoBlockRecovers(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	fail.Store(true)
	payload := []byte("recovered artifact bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/java-archive")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	svc.SetAutoBlockBase(100 * time.Millisecond) // 缩短退避，便于快速探测恢复
	repoID, err := repos.Create("rec-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 rec-proxy：%v", err)
	}

	// 首次回源失败 → AUTO_BLOCKED。
	if _, _, err := svc.Resolve(context.Background(), "rec-proxy", "lib/a.jar"); !errorsIsUpstream(err) {
		t.Fatalf("首次应回源失败（ErrUpstream），实际：%v", err)
	}
	if got := svc.Status(repoID).Status; got != domain.StatusAutoBlocked {
		t.Fatalf("首次失败后应 AUTO_BLOCKED，实际 %s", got)
	}

	// 上游恢复，等待后台探测自动恢复 AVAILABLE。
	fail.Store(false)
	waitForStatus(t, svc, repoID, domain.StatusAvailable, 3*time.Second)

	// 恢复后正常回源。
	_, rc, err := svc.Resolve(context.Background(), "rec-proxy", "lib/a.jar")
	if err != nil {
		t.Fatalf("恢复后 Resolve 应成功，实际：%v", err)
	}
	if got := readClose(t, rc); !bytes.Equal(got, payload) {
		t.Fatalf("恢复后内容不符：%q", got)
	}
}

// TestResolveProxyAutoBlockBackoffIncrements 验证退避递增：持续失败时阻止窗口按
// 起始档翻倍递增（默认 40s→80s→160s…），此处用缩短的起始档验证倍数关系。
func TestResolveProxyAutoBlockBackoffIncrements(t *testing.T) {
	t.Parallel()
	boomSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer boomSrv.Close()

	svc, repos := newAssetService(t)
	svc.SetAutoBlockBase(150 * time.Millisecond)
	repoID, err := repos.Create("bf-proxy", "raw", "proxy", "private", proxyConfigJSON(t, boomSrv.URL))
	if err != nil {
		t.Fatalf("建 bf-proxy：%v", err)
	}

	// 首次失败 → 第一档窗口。
	if _, _, err := svc.Resolve(context.Background(), "bf-proxy", "x.jar"); !errorsIsUpstream(err) {
		t.Fatalf("首次应回源失败，实际：%v", err)
	}
	first := svc.Status(repoID).BlockedFor
	if first <= 0 {
		t.Fatalf("首次失败后应有阻止窗口，BlockedFor=%v", first)
	}

	// 后台探测持续失败，窗口应递增；轮询捕获每次延长后的窗口档位。
	deadline := time.Now().Add(3 * time.Second)
	var windows []time.Duration
	last := first
	for time.Now().Before(deadline) {
		cur := svc.Status(repoID).BlockedFor
		if cur != last {
			windows = append(windows, cur)
			last = cur
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(windows) < 2 {
		t.Fatalf("应观察到至少 2 次窗口递增，实际 %d 次：%v", len(windows), windows)
	}
	for i := 1; i < len(windows); i++ {
		if windows[i] <= windows[i-1] {
			t.Fatalf("窗口应递增，实际 %v", windows)
		}
	}
}

// TestResolveProxyAutoBlockConcurrent 验证并发安全：多 goroutine 同时 group 读，
// 状态机并发更新无数据竞争（-race 验证），并发失败收敛为进入 AUTO_BLOCKED。
// 返回码口径由 FIX-1 定为 ErrUpstream（502）——并发下不应有任何一个请求被误报成 404。
func TestResolveProxyAutoBlockConcurrent(t *testing.T) {
	t.Parallel()
	var hits int32
	boomSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer boomSrv.Close()
	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer fastSrv.Close()

	svc, repos := newAssetService(t)
	svc.SetAutoBlockBase(time.Second) // 窗口足够长，避免测试期间恢复
	boomID, err := repos.Create("boom-proxy", "raw", "proxy", "private", proxyConfigJSON(t, boomSrv.URL))
	if err != nil {
		t.Fatalf("建 boom-proxy：%v", err)
	}
	if _, err := repos.Create("fast-proxy", "raw", "proxy", "private", proxyConfigJSON(t, fastSrv.URL)); err != nil {
		t.Fatalf("建 fast-proxy：%v", err)
	}
	if _, err := repos.Create("g-conc", "raw", "group", "private", groupConfigJSON(t, "boom-proxy", "fast-proxy")); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, err := svc.Resolve(context.Background(), "g-conc", "x.jar")
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		// FIX-1：成员回源 5xx / 被熔断跳过都是不可判定（非超时）→ ErrUpstream。
		if !errorsIsUpstream(err) {
			t.Fatalf("并发 Resolve[%d] 应 ErrUpstream，实际：%v", i, err)
		}
	}
	// 并发失败后进入 AUTO_BLOCKED（-race 验证并发安全）。
	if got := svc.Status(boomID).Status; got != domain.StatusAutoBlocked {
		t.Fatalf("并发失败后应 AUTO_BLOCKED，实际 %s", got)
	}
}

// ----------：404 负缓存 ----------

// TestNegativeCacheProxyNotFoundCached 验证 proxy 明确 404 写负缓存：
// 首次回源得到 404 后，TTL 内同路径再次请求直接 404，不再回源。
func TestNegativeCacheProxyNotFoundCached(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "raw-proxy", "missing.jar"); err != domain.ErrNotFound {
		t.Fatalf("首次应 ErrNotFound，实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("首次应回源 1 次，实际 %d", n)
	}
	// 负缓存命中：不再回源，秒级 404。
	start := time.Now()
	if _, _, err := svc.Resolve(context.Background(), "raw-proxy", "missing.jar"); err != domain.ErrNotFound {
		t.Fatalf("二次应 ErrNotFound，实际：%v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("负缓存命中应快速 404，实际耗时 %s", elapsed)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("负缓存命中后不应再回源，实际 %d 次", n)
	}
}

// TestNegativeCacheProxyErrorNotCached 验证上游故障（5xx）不写负缓存：
// 上游恢复后同路径立即重试成功；若误缓存，恢复后仍会返回缓存的 404。
func TestNegativeCacheProxyErrorNotCached(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	var hits int32
	fail.Store(true)
	payload := []byte("recovered after error")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 后台 HEAD 探测不计入回源次数（恢复探测）。
		if r.Method == http.MethodGet {
			atomic.AddInt32(&hits, 1)
		}
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/java-archive")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	svc.SetAutoBlockBase(100 * time.Millisecond) // 缩短退避便于快速恢复
	repoID, err := repos.Create("err-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "err-proxy", "lib/a.jar"); !errorsIsUpstream(err) {
		t.Fatalf("首次应回源失败（ErrUpstream），实际：%v", err)
	}
	// 上游恢复，等待后台探测恢复 AVAILABLE。
	fail.Store(false)
	waitForStatus(t, svc, repoID, domain.StatusAvailable, 3*time.Second)

	// 若 5xx 被误缓存成 404，此处会直接 ErrNotFound；正确行为是重新回源成功。
	_, rc, err := svc.Resolve(context.Background(), "err-proxy", "lib/a.jar")
	if err != nil {
		t.Fatalf("上游恢复后应重新回源成功，实际：%v", err)
	}
	if got := readClose(t, rc); !bytes.Equal(got, payload) {
		t.Fatalf("恢复后内容不符：%q", got)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("上游故障不应写负缓存，恢复后应重新回源（共 2 次），实际 %d 次", n)
	}
}

// TestNegativeCacheGroupConfirmedNotFoundCached 验证 group 全成员确认不存在时写负缓存：
// TTL 内再次请求 group 直接 404，不再探测成员（即使成员后来有了该制品）。
func TestNegativeCacheGroupConfirmedNotFoundCached(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("mem-hosted", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建 hosted 成员：%v", err)
	}
	if _, err := repos.Create("mem-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 成员：%v", err)
	}
	if _, err := repos.Create("g-confirm", "raw", "group", "private", groupConfigJSON(t, "mem-hosted", "mem-proxy")); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	// 首次：hosted 成员本地无 + proxy 成员 404 → 全确认不存在 → group 写负缓存。
	if _, _, err := svc.Resolve(context.Background(), "g-confirm", "x.jar"); err != domain.ErrNotFound {
		t.Fatalf("首次应 ErrNotFound，实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("首次应回源 proxy 成员 1 次，实际 %d", n)
	}

	// 成员后来上传了该制品（不失效 group 层负缓存键，权衡见 docs/specs/0.7.1-404-negative-cache.md）。
	if _, err := svc.Put("mem-hosted", "x.jar", bytes.NewReader([]byte("late-arrival")), "text/plain"); err != nil {
		t.Fatalf("成员上传：%v", err)
	}
	// 二次：group 负缓存命中 → 直接 404，不探测成员（proxy 成员不再被请求）。
	if _, _, err := svc.Resolve(context.Background(), "g-confirm", "x.jar"); err != domain.ErrNotFound {
		t.Fatalf("二次应命中 group 负缓存返回 ErrNotFound，实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("group 负缓存命中后不应再探测成员，实际 %d 次", n)
	}
}

// TestNegativeCacheGroupSkippedMemberNotCached 验证 M-1 不可判定不写负缓存：
// group 有 offline 成员被跳过时整体不可判定，返回 ErrUpstream（FIX-1：不再是 404）且不写负缓存；
// 成员恢复并上传后同路径立即重试成功。
func TestNegativeCacheGroupSkippedMemberNotCached(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("mem-hosted", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建 hosted 成员：%v", err)
	}
	if _, err := repos.Create("mem-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 成员：%v", err)
	}
	if _, err := repos.Create("g-offline", "raw", "group", "private", groupConfigJSON(t, "mem-hosted", "mem-proxy")); err != nil {
		t.Fatalf("建 group：%v", err)
	}
	// 置 hosted 成员离线：该成员被跳过，整体结论不可判定。
	if err := repos.SetOnline("mem-hosted", false); err != nil {
		t.Fatalf("置离线：%v", err)
	}

	// FIX-1：offline 成员属「不可判定（非超时）」，对外 502 而不是 404。
	if _, _, err := svc.Resolve(context.Background(), "g-offline", "x.jar"); !errorsIsUpstream(err) {
		t.Fatalf("成员离线时应为 ErrUpstream，实际：%v", err)
	}
	// 恢复成员并上传制品。
	if err := repos.SetOnline("mem-hosted", true); err != nil {
		t.Fatalf("恢复在线：%v", err)
	}
	payload := []byte("available-after-recovery")
	if _, err := svc.Put("mem-hosted", "x.jar", bytes.NewReader(payload), "text/plain"); err != nil {
		t.Fatalf("成员上传：%v", err)
	}
	// 若首次失败被误写负缓存，此处会直接 404；正确行为是重新探测命中。
	_, rc, err := svc.Resolve(context.Background(), "g-offline", "x.jar")
	if err != nil {
		t.Fatalf("成员恢复后应重试成功，实际：%v", err)
	}
	if got := readClose(t, rc); !bytes.Equal(got, payload) {
		t.Fatalf("恢复后内容不符：%q", got)
	}
}

// TestNegativeCacheGroupAutoBlockSkippedNotCached 验证 M-1 的另一路径：
// proxy 成员被 auto-block 阻止跳过时，group 不写负缓存（上游恢复后立即重试成功）。
//
// 返回码口径：FIX-1 起「成员被熔断跳过」属不可判定（非超时）→ ErrUpstream（502）；
// 待上游恢复、成员重新探测到上游 404（确认不存在）后，整体才回到 ErrNotFound（404）并写负缓存。
func TestNegativeCacheGroupAutoBlockSkippedNotCached(t *testing.T) {
	t.Parallel()
	var boomHits int32
	var fail atomic.Bool
	fail.Store(true)
	boomSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 后台 HEAD 探测不计入回源次数（恢复探测）。
		if r.Method == http.MethodGet {
			atomic.AddInt32(&boomHits, 1)
		}
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, nil)
	}))
	defer boomSrv.Close()
	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer fastSrv.Close()

	svc, repos := newAssetService(t)
	svc.SetAutoBlockBase(100 * time.Millisecond)
	boomID, err := repos.Create("boom-proxy", "raw", "proxy", "private", proxyConfigJSON(t, boomSrv.URL))
	if err != nil {
		t.Fatalf("建 boom-proxy：%v", err)
	}
	if _, err := repos.Create("fast-proxy", "raw", "proxy", "private", proxyConfigJSON(t, fastSrv.URL)); err != nil {
		t.Fatalf("建 fast-proxy：%v", err)
	}
	if _, err := repos.Create("g-blocked", "raw", "group", "private", groupConfigJSON(t, "boom-proxy", "fast-proxy")); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	// 首次：boom 5xx 失败进入 AUTO_BLOCKED，fast 404 → 整体不可判定（非超时）→ ErrUpstream。
	if _, _, err := svc.Resolve(context.Background(), "g-blocked", "x.jar"); !errorsIsUpstream(err) {
		t.Fatalf("首次应 ErrUpstream，实际：%v", err)
	}
	if got := svc.Status(boomID).Status; got != domain.StatusAutoBlocked {
		t.Fatalf("boom 应 AUTO_BLOCKED，实际 %s", got)
	}
	// 阻止窗口内再次请求：boom 被跳过（不可判定），fast 命中自身负缓存 → 仍是 ErrUpstream。
	if _, _, err := svc.Resolve(context.Background(), "g-blocked", "x.jar"); !errorsIsUpstream(err) {
		t.Fatalf("二次应 ErrUpstream，实际：%v", err)
	}
	if n := atomic.LoadInt32(&boomHits); n != 1 {
		t.Fatalf("阻止窗口内不应再请求 boom 上游，实际 %d 次", n)
	}

	// 上游恢复，等待自动恢复 AVAILABLE；group 负缓存未写，恢复后重新探测成员。
	fail.Store(false)
	waitForStatus(t, svc, boomID, domain.StatusAvailable, 3*time.Second)
	// 此时两个成员都从上游得到确认的 404 → 全确认不存在 → 回到 ErrNotFound。
	if _, _, err := svc.Resolve(context.Background(), "g-blocked", "x.jar"); err != domain.ErrNotFound {
		t.Fatalf("恢复后应 ErrNotFound，实际：%v", err)
	}
	// 恢复后 boom 被重新探测（GET 404），证明 group 结果未被误写负缓存：
	// 若写入了，此处 group 直接命中负缓存，boom 不会被探测。
	if n := atomic.LoadInt32(&boomHits); n != 2 {
		t.Fatalf("恢复后应重新探测 boom（共 2 次），实际 %d 次", n)
	}
}

// ----------：FIX-1 不可判定失败不再伪装成 404 ----------

// TestResolveGroupMemberTimeoutNotFoldedInto404 验证 FIX-1：group 全未命中时若某个 proxy 成员
// 回源超时（大制品 3s 下不完 / 上游不响应），整体必须返回 ErrUpstreamTimeout（对外 504），
// 而不是 ErrNotFound（404）。这是本次缺陷的直接回归点：上游「慢」不等于制品「不存在」。
func TestResolveGroupMemberTimeoutNotFoldedInto404(t *testing.T) {
	t.Parallel()
	hangSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hangSrv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("slow-proxy", "raw", "proxy", "private", proxyConfigJSON(t, hangSrv.URL)); err != nil {
		t.Fatalf("建 slow-proxy：%v", err)
	}
	// 单成员即可复现：没有任何成员确认不存在，整体结论只能是「不可判定 + 超时」。
	if _, err := repos.Create("g-slow", "raw", "group", "private", groupConfigJSON(t, "slow-proxy")); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	_, _, err := svc.Resolve(context.Background(), "g-slow", "15mb.jar")
	if !errors.Is(err, domain.ErrUpstreamTimeout) {
		t.Fatalf("成员超时应返回 ErrUpstreamTimeout（对外 504），实际：%v", err)
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("超时不得折叠成 ErrNotFound（假 404 正是本次要修的缺陷），实际：%v", err)
	}
	// 必须精确落 504：ErrUpstream 会让协议层回 502，把「慢」报成「上游故障」。
	if errors.Is(err, domain.ErrUpstream) {
		t.Fatalf("超时不应同时满足 ErrUpstream（会被映射成 502），实际：%v", err)
	}
}

// TestResolveGroupMemberUpstreamErrorNotFoldedInto404 验证 FIX-1：group 全未命中时若某个 proxy 成员
// 回源返回 5xx（非超时），整体必须返回 ErrUpstream（对外 502），而不是 ErrNotFound（404）。
func TestResolveGroupMemberUpstreamErrorNotFoldedInto404(t *testing.T) {
	t.Parallel()
	var boomHits int32
	boomSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&boomHits, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer boomSrv.Close()
	notFoundSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notFoundSrv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("err-proxy", "raw", "proxy", "private", proxyConfigJSON(t, boomSrv.URL)); err != nil {
		t.Fatalf("建 err-proxy：%v", err)
	}
	if _, err := repos.Create("nf-proxy", "raw", "proxy", "private", proxyConfigJSON(t, notFoundSrv.URL)); err != nil {
		t.Fatalf("建 nf-proxy：%v", err)
	}
	if _, err := repos.Create("g-err", "raw", "group", "private", groupConfigJSON(t, "err-proxy", "nf-proxy")); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	_, _, err := svc.Resolve(context.Background(), "g-err", "x.jar")
	if !errors.Is(err, domain.ErrUpstream) {
		t.Fatalf("成员回源 5xx 应返回 ErrUpstream（对外 502），实际：%v", err)
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("上游故障不得折叠成 ErrNotFound，实际：%v", err)
	}
	// 不是超时：必须精确落 502 而非 504。
	if errors.Is(err, domain.ErrUpstreamTimeout) {
		t.Fatalf("5xx 不应被当成超时（会被映射成 504），实际：%v", err)
	}
	if n := atomic.LoadInt32(&boomHits); n != 1 {
		t.Fatalf("失败成员应被回源 1 次，实际 %d 次", n)
	}
}

// TestResolveGroupOfflineMemberNotFoldedInto404 验证 FIX-1：group 有 offline 成员被跳过时，
// 「全未命中」属不可判定（非超时）→ ErrUpstream（502），且不写负缓存——
// 同路径再次请求仍重新探测，不会因一次不可判定而永久 404。
func TestResolveGroupOfflineMemberNotFoldedInto404(t *testing.T) {
	t.Parallel()
	var nfHits int32
	notFoundSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&nfHits, 1)
		http.NotFound(w, r)
	}))
	defer notFoundSrv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("off-hosted", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建 hosted 成员：%v", err)
	}
	if _, err := repos.Create("off-proxy", "raw", "proxy", "private", proxyConfigJSON(t, notFoundSrv.URL)); err != nil {
		t.Fatalf("建 proxy 成员：%v", err)
	}
	if _, err := repos.Create("g-off", "raw", "group", "private", groupConfigJSON(t, "off-hosted", "off-proxy")); err != nil {
		t.Fatalf("建 group：%v", err)
	}
	if err := repos.SetOnline("off-hosted", false); err != nil {
		t.Fatalf("置离线：%v", err)
	}

	// 两次都必须是 ErrUpstream：若 group 把不可判定写进了负缓存，第二次会直接 404。
	// （proxy 成员自身的 404 负缓存是正当的——它确实从上游确认了 404，与 group 的整体结论无关。）
	for round := 1; round <= 2; round++ {
		_, _, err := svc.Resolve(context.Background(), "g-off", "y.jar")
		if !errorsIsUpstream(err) {
			t.Fatalf("第 %d 次应为 ErrUpstream（离线成员不可判定），实际：%v", round, err)
		}
		if errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("第 %d 次不得返回 ErrNotFound（不可判定被写进了负缓存？），实际：%v", round, err)
		}
	}
	if got := atomic.LoadInt32(&nfHits); got != 1 {
		t.Fatalf("proxy 成员应只回源 1 次（成员级 404 负缓存收敛），实际 %d 次", got)
	}
}

// TestResolveGroupAllConfirmedStays404 验证 FIX-1 未改动的另一侧：所有成员都确认不存在时，
// 整体仍是 ErrNotFound（404），并照常写 group 级负缓存（同路径第二次请求不再探测成员）。
func TestResolveGroupAllConfirmedStays404(t *testing.T) {
	t.Parallel()
	var nfHits int32
	notFoundSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&nfHits, 1)
		http.NotFound(w, r)
	}))
	defer notFoundSrv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("ok-hosted", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建 hosted 成员：%v", err)
	}
	if _, err := repos.Create("ok-proxy", "raw", "proxy", "private", proxyConfigJSON(t, notFoundSrv.URL)); err != nil {
		t.Fatalf("建 proxy 成员：%v", err)
	}
	if _, err := repos.Create("g-ok", "raw", "group", "private", groupConfigJSON(t, "ok-hosted", "ok-proxy")); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	_, _, err := svc.Resolve(context.Background(), "g-ok", "z.jar")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("全成员确认不存在应返回 ErrNotFound，实际：%v", err)
	}
	if errors.Is(err, domain.ErrUpstream) || errors.Is(err, domain.ErrUpstreamTimeout) {
		t.Fatalf("确认不存在不应被判成上游故障，实际：%v", err)
	}
	if got := atomic.LoadInt32(&nfHits); got != 1 {
		t.Fatalf("首次应探测 proxy 成员 1 次，实际 %d 次", got)
	}
	// 负缓存已写：第二次直接 404，不再探测成员。
	if _, _, err := svc.Resolve(context.Background(), "g-ok", "z.jar"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("二次应命中 group 负缓存返回 ErrNotFound，实际：%v", err)
	}
	if got := atomic.LoadInt32(&nfHits); got != 1 {
		t.Fatalf("group 负缓存命中后不应再探测成员，实际 %d 次", got)
	}
}

// ----------：上游「慢」不再被当作「不可达」 ----------

// blockEventSpy 是 domain.UpstreamBlockRecorder 的测试替身（并发安全），
// 用于断言断路器判定事件的 reason 口径。
type blockEventSpy struct {
	mu   sync.Mutex
	seen []string
}

func (s *blockEventSpy) UpstreamBlockEvent(reason string) {
	s.mu.Lock()
	s.seen = append(s.seen, reason)
	s.mu.Unlock()
}

func (s *blockEventSpy) reasons() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// refuseServer 构造「可连但立刻断开」的上游夹具：每次连接被接受后立即关闭，
// 使回源在连接层失败（非超时）。返回上游地址与被接受的连接数。
//
// 统计连接数是为了让「阻断窗口内零连接」成为直接观测，而不是靠耗时推断——
// 环回口上的连接拒绝几乎是瞬时的，耗时上界不足以证明没有发起连接。
func refuseServer(t *testing.T) (string, *int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听本地端口：%v", err)
	}
	var conns int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			atomic.AddInt32(&conns, 1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return "http://" + ln.Addr().String(), &conns
}

// TestResolveProxySlowArtifactDoesNotBlockSiblingPath 是  的核心验收：
// 一个大制品回源超时（拖延发生在响应头之前，预算耗尽）之后——
//   - 该次请求仍返回 ErrUpstreamTimeout（对外 504，对外状态码语义不变）；
//   - 该 proxy 仓库**不进入** AUTO_BLOCKED / HALF_OPEN；
//   - 同仓库的小制品仍能正常拉取，内容正确，且确实打到了上游（夹具计数 +1，
//     证明不是被阻断放行的假成功）；
//   - 事件口径记 upstream_timeout，未被误记为 upstream_error。
func TestResolveProxySlowArtifactDoesNotBlockSiblingPath(t *testing.T) {
	t.Parallel()
	smallPayload := []byte("small-jar-payload")
	var bigHits, smallHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/big.jar") {
			atomic.AddInt32(&bigHits, 1)
			// 响应头之前拖延：预算耗尽即返回。这正是今日会误触仓库级阻断的路径
			// （正文慢速今日已不触发阻断，因为 recordSuccess 在读完正文前就已调用）。
			<-r.Context().Done()
			return
		}
		atomic.AddInt32(&smallHits, 1)
		w.Header().Set("Content-Type", "application/java-archive")
		_, _ = w.Write(smallPayload)
	}))
	defer srv.Close()

	svc, repos := newAssetServiceWithUpstreamTimeout(t, 300*time.Millisecond)
	spy := &blockEventSpy{}
	svc.SetUpstreamBlockRecorder(spy)
	repoID, err := repos.Create("slow-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 slow-proxy：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "slow-proxy", "big.jar"); !errors.Is(err, domain.ErrUpstreamTimeout) {
		t.Fatalf("大制品回源超时应返回 ErrUpstreamTimeout（对外 504），实际：%v", err)
	}
	if n := atomic.LoadInt32(&bigHits); n != 1 {
		t.Fatalf("大制品应回源 1 次，实际 %d 次", n)
	}
	if got := svc.Status(repoID); got.Status == domain.StatusAutoBlocked || got.Status == domain.StatusHalfOpen {
		t.Fatalf("回源超时不得让整个仓库进入阻断，实际 %s（窗口 %s）", got.Status, got.BlockedFor)
	}

	// 核心判据：同仓库其它路径不受牵连，且真正打到了上游。
	_, rc, err := svc.Resolve(context.Background(), "slow-proxy", "small.jar")
	if err != nil {
		t.Fatalf("超时之后同仓库小制品仍应可拉取，实际：%v", err)
	}
	if got := readClose(t, rc); !bytes.Equal(got, smallPayload) {
		t.Fatalf("小制品内容不符：%q", got)
	}
	if n := atomic.LoadInt32(&smallHits); n != 1 {
		t.Fatalf("小制品应真正回源 1 次（不是被阻断放行），实际 %d 次", n)
	}

	// 事件口径：只记 upstream_timeout。
	if got := spy.reasons(); len(got) != 1 || got[0] != "upstream_timeout" {
		t.Fatalf("应只记一次 upstream_timeout，实际 %v", got)
	}
}

// TestResolveProxyUnreachableStillAutoBlocks 验证真实不可达仍按既有退避阻断（不回退）：
// 上游可连但立刻断开属连接层失败（非超时）→ AUTO_BLOCKED 且窗口非零，
// 窗口内第二次请求零连接快速失败，并记 upstream_error。
func TestResolveProxyUnreachableStillAutoBlocks(t *testing.T) {
	t.Parallel()
	remoteURL, conns := refuseServer(t)
	svc, repos := newAssetService(t)
	svc.SetAutoBlockBase(10 * time.Second) // 窗口足够长，测试期间不失效
	spy := &blockEventSpy{}
	svc.SetUpstreamBlockRecorder(spy)
	repoID, err := repos.Create("dead-proxy", "raw", "proxy", "private", proxyConfigJSON(t, remoteURL))
	if err != nil {
		t.Fatalf("建 dead-proxy：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "dead-proxy", "a.jar"); !errorsIsUpstream(err) {
		t.Fatalf("连接失败应返回 ErrUpstream（对外 502），实际：%v", err)
	}
	if n := atomic.LoadInt32(conns); n == 0 {
		t.Fatal("首次请求应向不可达上游发起连接（否则不是连接失败场景）")
	}
	got := svc.Status(repoID)
	if got.Status != domain.StatusAutoBlocked || got.BlockedUntil.IsZero() {
		t.Fatalf("连接失败应进入 AUTO_BLOCKED 且窗口非零，实际 %+v", got)
	}

	accepted := atomic.LoadInt32(conns)
	start := time.Now()
	if _, _, err := svc.Resolve(context.Background(), "dead-proxy", "a.jar"); !errorsIsUpstream(err) {
		t.Fatalf("阻止窗口内应快速失败返回 ErrUpstream，实际：%v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("阻止窗口内应快速失败，实际耗时 %s", elapsed)
	}
	if n := atomic.LoadInt32(conns); n != accepted {
		t.Fatalf("阻止窗口内不应再建立上游连接，新增 %d 个", n-accepted)
	}
	if reasons := spy.reasons(); len(reasons) != 1 || reasons[0] != "upstream_error" {
		t.Fatalf("应只记一次 upstream_error，实际 %v", reasons)
	}
}

// TestResolveProxyUpstreamGoneNotBlocked 钉住既有 410 豁免：资源永久下架不代表上游不可达，
// 仓库状态保持非阻断、负缓存不写入（下次仍回源），且不记任何判定事件。
func TestResolveProxyUpstreamGoneNotBlocked(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	spy := &blockEventSpy{}
	svc.SetUpstreamBlockRecorder(spy)
	repoID, err := repos.Create("gone-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 gone-proxy：%v", err)
	}

	// 410 的错误身份维持既有映射：ErrUpstream + ErrUpstreamGone。
	if _, _, err := svc.Resolve(context.Background(), "gone-proxy", "old.jar"); !errorsIsUpstream(err) || !errors.Is(err, domain.ErrUpstreamGone) {
		t.Fatalf("410 应映射为 ErrUpstream + ErrUpstreamGone，实际：%v", err)
	}
	got := svc.Status(repoID)
	if got.Status != domain.StatusReady || !got.BlockedUntil.IsZero() || got.BlockedFor != 0 {
		t.Fatalf("410 不得触发阻断，实际 %+v", got)
	}
	// 负缓存不写入：第二次仍真正回源。
	if _, _, err := svc.Resolve(context.Background(), "gone-proxy", "old.jar"); !errorsIsUpstream(err) {
		t.Fatalf("二次仍应为 ErrUpstream，实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("410 不写负缓存，第二次应仍回源（共 2 次），实际 %d 次", n)
	}
	if reasons := spy.reasons(); len(reasons) != 0 {
		t.Fatalf("410 不应记录任何判定事件，实际 %v", reasons)
	}
}

// TestResolveOCIProxyFailureClassification 覆盖第二个回源调用点（ResolveOCIProxy）：
// 它原先写的是「非 404 即 recordFailure」，必须先按同一判定函数分流，
// 否则超时只修好了 raw 路径、OCI 仓库仍会被慢回源打成 AUTO_BLOCKED。
func TestResolveOCIProxyFailureClassification(t *testing.T) {
	t.Parallel()
	ociPath := func(name string) (string, string) {
		return "v2/lib/" + name + "/manifests/latest", "repository:lib/" + name + ":pull"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/slow/") {
			<-r.Context().Done() // 响应头之前拖延：预算耗尽即返回
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc, repos := newAssetServiceWithUpstreamTimeout(t, 300*time.Millisecond)
	spy := &blockEventSpy{}
	svc.SetUpstreamBlockRecorder(spy)
	repoID, err := repos.Create("oci-proxy", "docker", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 oci-proxy：%v", err)
	}

	// 超时分支：不阻断，对外仍是 504 语义。
	slowPath, slowScope := ociPath("slow")
	if _, _, err := svc.ResolveOCIProxy(context.Background(), "oci-proxy", slowPath, slowScope, ""); !errors.Is(err, domain.ErrUpstreamTimeout) {
		t.Fatalf("OCI 慢回源应返回 ErrUpstreamTimeout，实际：%v", err)
	}
	if got := svc.Status(repoID); got.Status == domain.StatusAutoBlocked || got.Status == domain.StatusHalfOpen {
		t.Fatalf("OCI 回源超时不得让仓库进入阻断，实际 %s", got.Status)
	}

	// 5xx 分支：维持既有阻断（保护不回退）。
	boomPath, boomScope := ociPath("boom")
	if _, _, err := svc.ResolveOCIProxy(context.Background(), "oci-proxy", boomPath, boomScope, ""); !errorsIsUpstream(err) {
		t.Fatalf("OCI 5xx 应返回 ErrUpstream，实际：%v", err)
	}
	if got := svc.Status(repoID); got.Status != domain.StatusAutoBlocked {
		t.Fatalf("OCI 5xx 应进入 AUTO_BLOCKED，实际 %s", got.Status)
	}

	// 事件口径：一次 upstream_timeout + 一次 upstream_error。
	want := []string{"upstream_timeout", "upstream_error"}
	got := spy.reasons()
	if len(got) != len(want) {
		t.Fatalf("判定事件应为 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("判定事件应为 %v，实际 %v", want, got)
		}
	}
}

// TestGroupCanceledMemberDoesNotBlockProxy 钉住「被同伴取消 ≠ 上游不可达」。
//
// group 并行探测时，快成员一成功就 cancel() 其余成员。若某个慢成员当时还没拿到
// 响应头，它的回源会以 context.Canceled 结束。这类取消**不构成上游不可达的证据**：
// 一旦被当成 unreachable，会触发 recordFailure 让该 proxy 仓库进入 AUTO_BLOCKED，
// 并逐档退避——同仓库所有其它路径随之零连接快速失败。那正好复现了本批要修的
// 「整仓不可用」，等于用一个新故障换掉旧故障。
//
// 变异验证：把 classifyUpstreamFailure 里 context.Canceled 的分支删掉，本用例必须转红。
func TestGroupCanceledMemberDoesNotBlockProxy(t *testing.T) {
	t.Parallel()

	// 慢成员：连上后永不回包，保证在「拿到响应头之前」被取消。
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()

	// 快成员：立刻返回内容，触发 cancel()。
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("payload"))
	}))
	defer fast.Close()

	svc, repos := newAssetService(t)

	slowID, err := repos.Create("cancel-slow", "raw", "proxy", "private", proxyConfigJSON(t, slow.URL))
	if err != nil {
		t.Fatalf("建慢成员：%v", err)
	}
	if _, err := repos.Create("cancel-fast", "raw", "proxy", "private", proxyConfigJSON(t, fast.URL)); err != nil {
		t.Fatalf("建快成员：%v", err)
	}
	if _, err := repos.Create("cancel-group", "raw", "group", "private",
		`{"members":["cancel-slow","cancel-fast"]}`); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, rc, _ := svc.Resolve(ctx, "cancel-group", "dir/file.txt")
	if rc != nil {
		_ = rc.Close()
	}

	// 阻断状态机是异步落定的，给它一点时间；若真的误阻断，这段时间足够置位。
	time.Sleep(300 * time.Millisecond)

	if got := svc.Status(slowID).Status; got == domain.StatusAutoBlocked {
		t.Fatalf("被同伴取消的慢成员不得被判为不可达：status = %s", got)
	}
}
