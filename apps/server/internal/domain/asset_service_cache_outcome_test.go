package domain_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// TestProxyResolveCacheOutcome 校验 proxy 读路径的缓存来源判定：
// 首次本地未命中→miss，二次命中已缓存副本→hit。
func TestProxyResolveCacheOutcome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("upstream artifact bytes"))
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	missCtx := domain.WithCacheOutcome(context.Background())
	_, rc, err := svc.Resolve(missCtx, "raw-proxy", "lib/foo.jar")
	if err != nil {
		t.Fatalf("首次 Resolve：%v", err)
	}
	_ = rc.Close()
	if got := domain.CacheOutcomeFromContext(missCtx); got != domain.CacheOutcomeMiss {
		t.Fatalf("首次本地未命中应判 miss，得 %q", got)
	}

	hitCtx := domain.WithCacheOutcome(context.Background())
	_, rc2, err := svc.Resolve(hitCtx, "raw-proxy", "lib/foo.jar")
	if err != nil {
		t.Fatalf("二次 Resolve：%v", err)
	}
	_ = rc2.Close()
	if got := domain.CacheOutcomeFromContext(hitCtx); got != domain.CacheOutcomeHit {
		t.Fatalf("二次命中缓存应判 hit，得 %q", got)
	}
}

// TestHostedResolveCacheOutcomeUnknown 校验 hosted 读路径不产出缓存来源（保持未知）。
func TestHostedResolveCacheOutcomeUnknown(t *testing.T) {
	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-hosted", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建 hosted 仓库：%v", err)
	}
	if _, err := svc.Put("raw-hosted", "a/b.txt", bytes.NewReader([]byte("content")), "text/plain"); err != nil {
		t.Fatalf("写制品：%v", err)
	}

	ctx := domain.WithCacheOutcome(context.Background())
	_, rc, err := svc.Resolve(ctx, "raw-hosted", "a/b.txt")
	if err != nil {
		t.Fatalf("Resolve：%v", err)
	}
	_ = rc.Close()
	if got := domain.CacheOutcomeFromContext(ctx); got != domain.CacheOutcomeUnknown {
		t.Fatalf("hosted 应保持未知，得 %q", got)
	}
}

// TestGroupResolveCacheOutcomeUnknown 校验 group 聚合读不因成员命中而产出 hit/miss（保持未知）。
func TestGroupResolveCacheOutcomeUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("member artifact"))
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	if _, err := repos.Create("raw-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 成员：%v", err)
	}
	if _, err := repos.Create("raw-group", "raw", "group", "private", groupConfigJSON(t, "raw-proxy")); err != nil {
		t.Fatalf("建 group 仓库：%v", err)
	}

	ctx := domain.WithCacheOutcome(context.Background())
	_, rc, err := svc.Resolve(ctx, "raw-group", "lib/foo.jar")
	if err != nil {
		t.Fatalf("group Resolve：%v", err)
	}
	_ = rc.Close()
	if got := domain.CacheOutcomeFromContext(ctx); got != domain.CacheOutcomeUnknown {
		t.Fatalf("group 应保持未知，得 %q", got)
	}
}
