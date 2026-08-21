package domain_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
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

// 目录形路径（空串或以 / 结尾）不是制品：proxy 不得回源，否则上游返回的 HTML
// 目录索引页会被当成制品缓存（path 以 / 结尾、text/html），前端文件树出现空白名假文件。
func TestResolveRejectsDirectoryLikePath(t *testing.T) {
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

func TestResolveProxySingleFlight(t *testing.T) {
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
// （不响应直到超时），另一个快速 404；整体应在秒级返回 404，而非串行等 2 个超时。
func TestResolveGroupFast404WithHangingMember(t *testing.T) {
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
	if err != domain.ErrNotFound {
		t.Fatalf("应 ErrNotFound，实际：%v（耗时 %s）", err, elapsed)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("group 缺失应快速 404，实际耗时 %s（并行未生效？）", elapsed)
	}
}

// TestResolveGroupFuseSkipsUnreachableMember 验证熔断：proxy 上游 5xx 失败后进入短窗熔断，
// 后续 group 请求直接跳过该成员（不再请求上游），快速 404。
func TestResolveGroupFuseSkipsUnreachableMember(t *testing.T) {
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

	if _, _, err := svc.Resolve(context.Background(), "g-fuse", "x.jar"); err != domain.ErrNotFound {
		t.Fatalf("首次应 ErrNotFound，实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("首次应回源 boom 上游 1 次，实际 %d", n)
	}
	if _, _, err := svc.Resolve(context.Background(), "g-fuse", "x.jar"); err != domain.ErrNotFound {
		t.Fatalf("二次应 ErrNotFound，实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("熔断后不应再请求 boom 上游，实际命中 %d 次", n)
	}
}

// TestResolveGroupParallelHitFromLaterMember 验证并行回源：后一个 proxy 成员能命中并返回内容。
func TestResolveGroupParallelHitFromLaterMember(t *testing.T) {
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

// ---------- FR-112：proxy 上游 auto-block 主动探测恢复 ----------

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
// 阻止窗口内 group 请求对该成员零连接、快速 404。
func TestResolveProxyAutoBlockZeroConnection(t *testing.T) {
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

	// 首次：boom 回源失败 → AUTO_BLOCKED；fast 404 → group 快速 404。
	if _, _, err := svc.Resolve(context.Background(), "g-auto", "x.jar"); err != domain.ErrNotFound {
		t.Fatalf("首次应 ErrNotFound，实际：%v", err)
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

	// 二次（阻止窗口内）：零连接、快速 404。
	start := time.Now()
	if _, _, err := svc.Resolve(context.Background(), "g-auto", "x.jar"); err != domain.ErrNotFound {
		t.Fatalf("二次应 ErrNotFound，实际：%v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("阻止窗口内应快速 404，实际耗时 %s", elapsed)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("阻止窗口内不应再请求 boom 上游，实际命中 %d 次", n)
	}
}

// TestResolveProxyAutoBlockRecovers 验证后台探测恢复：上游恢复后，后台 HEAD 探测成功，
// 状态自动回到 AVAILABLE，后续请求正常回源。
func TestResolveProxyAutoBlockRecovers(t *testing.T) {
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
func TestResolveProxyAutoBlockConcurrent(t *testing.T) {
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
		if err != domain.ErrNotFound {
			t.Fatalf("并发 Resolve[%d] 应 ErrNotFound，实际：%v", i, err)
		}
	}
	// 并发失败后进入 AUTO_BLOCKED（-race 验证并发安全）。
	if got := svc.Status(boomID).Status; got != domain.StatusAutoBlocked {
		t.Fatalf("并发失败后应 AUTO_BLOCKED，实际 %s", got)
	}
}

// ---------- FR-111：404 负缓存 ----------

// TestNegativeCacheProxyNotFoundCached 验证 proxy 明确 404 写负缓存：
// 首次回源得到 404 后，TTL 内同路径再次请求直接 404，不再回源。
func TestNegativeCacheProxyNotFoundCached(t *testing.T) {
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
	var fail atomic.Bool
	var hits int32
	fail.Store(true)
	payload := []byte("recovered after error")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 后台 HEAD 探测不计入回源次数（FR-112 恢复探测）。
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

	// 成员后来上传了该制品（不失效 group 层负缓存键，见 spec §6 权衡）。
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

// TestNegativeCacheGroupSkippedMemberNotCached 验证 M-1 不可判定 404 不写负缓存：
// group 有 offline 成员被跳过时，404 不写负缓存；成员恢复并上传后同路径立即重试成功。
func TestNegativeCacheGroupSkippedMemberNotCached(t *testing.T) {
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
	// 置 hosted 成员离线：该成员被跳过，404 不可判定。
	if err := repos.SetOnline("mem-hosted", false); err != nil {
		t.Fatalf("置离线：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "g-offline", "x.jar"); err != domain.ErrNotFound {
		t.Fatalf("首次应 ErrNotFound，实际：%v", err)
	}
	// 恢复成员并上传制品。
	if err := repos.SetOnline("mem-hosted", true); err != nil {
		t.Fatalf("恢复在线：%v", err)
	}
	payload := []byte("available-after-recovery")
	if _, err := svc.Put("mem-hosted", "x.jar", bytes.NewReader(payload), "text/plain"); err != nil {
		t.Fatalf("成员上传：%v", err)
	}
	// 若首次 404 被误写负缓存，此处会直接 404；正确行为是重新探测命中。
	_, rc, err := svc.Resolve(context.Background(), "g-offline", "x.jar")
	if err != nil {
		t.Fatalf("成员恢复后应重试成功，实际：%v", err)
	}
	if got := readClose(t, rc); !bytes.Equal(got, payload) {
		t.Fatalf("恢复后内容不符：%q", got)
	}
}

// TestNegativeCacheGroupAutoBlockSkippedNotCached 验证 M-1 的另一路径：
// proxy 成员被 auto-block 阻止跳过时，group 404 不写负缓存（上游恢复后立即重试成功）。
func TestNegativeCacheGroupAutoBlockSkippedNotCached(t *testing.T) {
	var boomHits int32
	var fail atomic.Bool
	fail.Store(true)
	boomSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 后台 HEAD 探测不计入回源次数（FR-112 恢复探测）。
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

	// 首次：boom 5xx 失败进入 AUTO_BLOCKED，fast 404 → group 404（不可判定：boom 回源失败）。
	if _, _, err := svc.Resolve(context.Background(), "g-blocked", "x.jar"); err != domain.ErrNotFound {
		t.Fatalf("首次应 ErrNotFound，实际：%v", err)
	}
	if got := svc.Status(boomID).Status; got != domain.StatusAutoBlocked {
		t.Fatalf("boom 应 AUTO_BLOCKED，实际 %s", got)
	}
	// 阻止窗口内再次请求：boom 被跳过（不可判定），fast 命中自身负缓存 → group 404。
	if _, _, err := svc.Resolve(context.Background(), "g-blocked", "x.jar"); err != domain.ErrNotFound {
		t.Fatalf("二次应 ErrNotFound，实际：%v", err)
	}
	if n := atomic.LoadInt32(&boomHits); n != 1 {
		t.Fatalf("阻止窗口内不应再请求 boom 上游，实际 %d 次", n)
	}

	// 上游恢复，等待自动恢复 AVAILABLE；group 负缓存未写，恢复后重新探测成员。
	fail.Store(false)
	waitForStatus(t, svc, boomID, domain.StatusAvailable, 3*time.Second)
	if _, _, err := svc.Resolve(context.Background(), "g-blocked", "x.jar"); err != domain.ErrNotFound {
		t.Fatalf("恢复后应 ErrNotFound，实际：%v", err)
	}
	// 恢复后 boom 被重新探测（GET 404），证明 group 404 未被误写负缓存：
	// 若写入了，此处 group 直接命中负缓存，boom 不会被探测。
	if n := atomic.LoadInt32(&boomHits); n != 2 {
		t.Fatalf("恢复后应重新探测 boom（共 2 次），实际 %d 次", n)
	}
}

// TestNegativeCacheInvalidatedByReplicationApply 验证复制应用成功后失效负缓存：
// proxy 404 写负缓存后，对端同步（复制应用）写入同路径，立即读取到新制品。
func TestNegativeCacheInvalidatedByReplicationApply(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	svc := domain.NewAssetService(repos, assets, blobs, upstream.NewClient(5*time.Second))

	if _, err := repos.Create("seed-hosted", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建 seed 仓库：%v", err)
	}
	if _, err := repos.Create("sync-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	// 用 seed hosted 仓库产生一个真实 blob，供复制变更引用。
	payload := []byte("synced artifact")
	seed, err := svc.Put("seed-hosted", "seed.txt", bytes.NewReader(payload), "text/plain")
	if err != nil {
		t.Fatalf("Put seed：%v", err)
	}

	// 首次：proxy 404 → 写负缓存。
	if _, _, err := svc.Resolve(context.Background(), "sync-proxy", "lib/synced.jar"); err != domain.ErrNotFound {
		t.Fatalf("首次应 ErrNotFound，实际：%v", err)
	}

	// 装配 ReplicationService 并注入负缓存（模拟生产 SetChangeRecorder 装配路径）。
	replSvc := domain.NewReplicationService(
		repository.NewReplChangeRepo(db), assets, repos,
		repository.NewAclRepo(db), repository.NewUserRepo(db), repository.NewTokenRepo(db),
		repository.NewSettingRepo(db), blobs,
	)
	svc.SetChangeRecorder(replSvc)

	// 复制应用：对端同步一个 asset put 到 sync-proxy。
	data, _ := json.Marshal(domain.AssetChangeData{
		Path: "lib/synced.jar", BlobHash: seed.BlobHash, Size: seed.Size, ContentType: seed.ContentType,
	})
	ch := repository.Change{
		NodeID: "peer-node", Op: domain.OpPut, EntityType: domain.EntityAsset,
		EntityKey: domain.AssetKey("sync-proxy", "lib/synced.jar"),
		Data:      string(data), TS: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if out := replSvc.ApplyOutcome(ch); out.Err != nil {
		t.Fatalf("复制应用失败：%v", out.Err)
	}

	// 负缓存已失效：立即读到对端同步的制品，不再回源。
	_, rc, err := svc.Resolve(context.Background(), "sync-proxy", "lib/synced.jar")
	if err != nil {
		t.Fatalf("复制应用后应能读到制品，实际：%v", err)
	}
	if got := readClose(t, rc); !bytes.Equal(got, payload) {
		t.Fatalf("复制应用后内容不符：%q", got)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("复制应用失效负缓存后不应再回源，实际 %d 次", n)
	}
}

// TestNegativeCacheTTLExpiry 验证负缓存 TTL 过期后重新回源/探测。
func TestNegativeCacheTTLExpiry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	svc.SetNegativeCacheTTL(50 * time.Millisecond)
	if _, err := repos.Create("ttl-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "ttl-proxy", "missing.jar"); err != domain.ErrNotFound {
		t.Fatalf("首次应 ErrNotFound，实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("首次应回源 1 次，实际 %d", n)
	}
	// TTL 内命中负缓存。
	if _, _, err := svc.Resolve(context.Background(), "ttl-proxy", "missing.jar"); err != domain.ErrNotFound {
		t.Fatalf("TTL 内应命中负缓存 404，实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("TTL 内不应回源，实际 %d 次", n)
	}
	// 等待 TTL 过期后重新回源。
	time.Sleep(80 * time.Millisecond)
	if _, _, err := svc.Resolve(context.Background(), "ttl-proxy", "missing.jar"); err != domain.ErrNotFound {
		t.Fatalf("TTL 过期后应重新探测返回 404，实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("TTL 过期后应重新回源（共 2 次），实际 %d 次", n)
	}
}

// TestResolveProxyOfflineNoUpstream 验证 FR-113：手动 offline 的 proxy 仓库单独读
// 直接 404，不发起回源（upstream 零命中）。
func TestResolveProxyOfflineNoUpstream(t *testing.T) {
	var hits int32
	payload := []byte("should-not-fetch")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("off-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy：%v", err)
	}
	// 置 offline。
	if err := repos.SetOnline("off-proxy", false); err != nil {
		t.Fatalf("SetOnline(false)：%v", err)
	}

	// 单独读 offline proxy：本地未命中 → 应 404 且不回源。
	if _, _, err := svc.Resolve(context.Background(), "off-proxy", "lib/missing.jar"); err != domain.ErrNotFound {
		t.Fatalf("offline proxy 单独读应 ErrNotFound，实际：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("offline proxy 不应回源上游，实际命中 %d 次", n)
	}

	// 置回 online 后可正常回源。
	if err := repos.SetOnline("off-proxy", true); err != nil {
		t.Fatalf("SetOnline(true)：%v", err)
	}
	_, rc, err := svc.Resolve(context.Background(), "off-proxy", "lib/missing.jar")
	if err != nil {
		t.Fatalf("online 后应回源（该上游总是 200），实际：%v", err)
	}
	defer func() { _ = rc.Close() }()
	if got := readClose(t, rc); !bytes.Equal(got, payload) {
		t.Fatalf("回源内容不符：%q", got)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("online 后应回源 1 次，实际 %d 次", n)
	}
}
