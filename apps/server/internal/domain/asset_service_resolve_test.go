package domain_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
