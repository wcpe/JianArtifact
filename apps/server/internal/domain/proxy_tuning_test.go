package domain_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// 本文件覆盖两项配置的装配面：两项配置经 setter 注入后**真的改变运行时行为**。
//
// 只断言「配置解析对了」不足以证明接线成功——装配层此前从未调用过
// SetNegativeCacheTTL，负缓存 TTL 实际是运行时改不了的常量，因此这里从
// Resolve 的外部可观测行为（上游命中次数 / 耗时上界）两端钉住。

// TestNegativeCacheDisabledAlwaysHitsUpstream 验证「JIAN_NEGATIVE_CACHE_TTL=0 时负缓存
// 完全关闭」：同一条缺失路径连续两次读都真实回源（上游命中计数 = 2），而不是第二次
// 直接返回 404。
func TestNegativeCacheDisabledAlwaysHitsUpstream(t *testing.T) {
	t.Parallel()
	srv, hits := newCountingUpstream(t, http.StatusNotFound)

	svc, repos := newAssetService(t)
	svc.SetNegativeCacheTTL(0) // 等价于 JIAN_NEGATIVE_CACHE_TTL=0
	if _, err := repos.Create("closed-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	for i := 1; i <= 2; i++ {
		if _, _, err := svc.Resolve(context.Background(), "closed-proxy", "missing.jar"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("第 %d 次读应 ErrNotFound，实际：%v", i, err)
		}
		// 关闭态下每次都必须真实回源；若第二次命中负缓存，命中计数会停在 1。
		if got := atomic.LoadInt32(hits); got != int32(i) {
			t.Fatalf("第 %d 次读后上游命中计数 = %d，期望 %d（关闭态不得命中负缓存）", i, got, i)
		}
	}
	// 负缓存表不得因这些 404 而增长。
	if got := svc.NegativeCacheEntriesCount(); got != 0 {
		t.Fatalf("关闭态下负缓存条目数应为 0，实际 %d", got)
	}
}

// TestNegativeCacheEnabledStillCaches 是上一条的对照组：默认 TTL 下第二次读仍命中负缓存
// （上游只被请求 1 次），证明上面那条不是因为「本来就缓存不住」而通过。
func TestNegativeCacheEnabledStillCaches(t *testing.T) {
	t.Parallel()
	srv, hits := newCountingUpstream(t, http.StatusNotFound)

	svc, repos := newAssetService(t)
	svc.SetNegativeCacheTTL(60 * time.Second) // 等价于 JIAN_NEGATIVE_CACHE_TTL=60（默认）
	if _, err := repos.Create("open-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	for i := 1; i <= 2; i++ {
		if _, _, err := svc.Resolve(context.Background(), "open-proxy", "missing.jar"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("第 %d 次读应 ErrNotFound，实际：%v", i, err)
		}
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("默认 TTL 下第二次应命中负缓存，上游命中计数 = %d，期望 1", got)
	}
	if got := svc.NegativeCacheEntriesCount(); got != 1 {
		t.Fatalf("默认 TTL 下应有 1 条负缓存条目，实际 %d", got)
	}
}

// hangingUpstream 构造「永不给响应头」的上游：请求进来后一直挂到 ctx 结束。
// 这正是成员确认预算要挡的场景（跨地域慢成员 / 黑洞路由）。
func hangingUpstream(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// assertHangingMemberSkippedAt 读一次 group 仓库（一个成员永久挂起、另一个快速 404），
// 返回整体耗时与该轮两个上游的命中次数，供各用例按自己的预算断言。
//
// 只统一断言「返回码是超时而非 404」：这是 FIX-1 的定稿语义，与预算大小无关。
// 耗时上界与命中次数交给调用方——预算极短时，请求可能还没派发到挂起成员就被看门狗
// 取消（命中 0 次），那是短预算的正常结果而不是缺陷，不该在这里钉死。
func assertHangingMemberSkippedAt(t *testing.T, repoName string, budget time.Duration) (time.Duration, int32, int32) {
	t.Helper()
	hangSrv, hangHits := hangingUpstream(t)
	fastSrv, fastHits := newCountingUpstream(t, http.StatusNotFound)

	// 回源整体超时取远大于任何一档成员确认预算，确保耗时由看门狗而非回源超时决定。
	svc, repos := newAssetServiceWithUpstreamTimeout(t, 30*time.Second)
	svc.SetGroupMemberTimeout(budget)
	createGroupWithProxyMembers(t, repos, repoName,
		proxyMember{name: repoName + "-hang", remoteURL: hangSrv.URL},
		proxyMember{name: repoName + "-fast", remoteURL: fastSrv.URL},
	)

	start := time.Now()
	_, _, err := svc.Resolve(context.Background(), repoName, "missing.jar")
	elapsed := time.Since(start)

	if !errors.Is(err, domain.ErrUpstreamTimeout) {
		t.Fatalf("挂起成员应判为超时（ErrUpstreamTimeout），实际：%v（耗时 %s）", err, elapsed)
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("不可判定的超时不得折叠成 ErrNotFound，实际：%v（耗时 %s）", err, elapsed)
	}
	return elapsed, atomic.LoadInt32(hangHits), atomic.LoadInt32(fastHits)
}

// TestGroupMemberTimeoutInjectedShortBudget 验证注入的**短**预算真的缩短了确认阶段：
// 设成 200ms 时整体应远快于默认 3s。
//
// 判据只用耗时：若看门狗仍读硬编码 3s，耗时必然落在 3s 量级；本用例据此把
// 「配置被读取」与「配置未接线」区分开。
func TestGroupMemberTimeoutInjectedShortBudget(t *testing.T) {
	t.Parallel()
	elapsed, _, _ := assertHangingMemberSkippedAt(t, "g-budget-short", 200*time.Millisecond)
	// 上界留足 -race 与 CI 抖动余量，但仍明确小于默认 3s。
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("注入 200ms 预算后整体耗时 = %s，仍接近默认 3s：看门狗可能没读到注入值", elapsed)
	}
}

// TestGroupMemberTimeoutInjectedLongBudget 是两向断言：注入 1s 预算时整体耗时必须
// **达到** 1s 量级，证明看门狗确实按注入值等待，而不是无论设多大都按更小的值走。
//
// 判据（1s ≤ elapsed < 2.5s）同时排除三种实现：硬编码 3s（≥3s）、硬编码更小的值
// （<1s）、以及根本不等待（≈0s）。
func TestGroupMemberTimeoutInjectedLongBudget(t *testing.T) {
	t.Parallel()
	elapsed, _, _ := assertHangingMemberSkippedAt(t, "g-budget-long", time.Second)
	if elapsed < 900*time.Millisecond {
		t.Fatalf("注入 1s 预算后整体耗时 = %s，短于预算：看门狗未按注入值等待", elapsed)
	}
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("注入 1s 预算后整体耗时 = %s，超出 2.5s：看门狗可能仍在用默认 3s", elapsed)
	}
}

// TestGroupMemberTimeoutDefaultUnchanged 钉住默认值不变（3s）：这是两阶段预算定稿的语义，
// 本 FR 只把数值变可配置，不得改变默认行为。3s 足够宽裕，因此这里可以同时断言
// 「挂起成员被真实请求 1 次」与「整体在 3s 量级返回」。
func TestGroupMemberTimeoutDefaultUnchanged(t *testing.T) {
	t.Parallel()
	elapsed, hangHits, _ := assertHangingMemberSkippedAt(t, "g-budget-default", 3*time.Second)
	if hangHits != 1 {
		t.Fatalf("挂起成员只应被请求 1 次，实际 %d 次（耗时 %s）", hangHits, elapsed)
	}
	if elapsed < 2500*time.Millisecond {
		t.Fatalf("默认预算下整体耗时 = %s，短于 3s 量级：默认值被改小了", elapsed)
	}
	if elapsed >= 4500*time.Millisecond {
		t.Fatalf("默认预算下整体耗时 = %s，超出 4.5s：确认预算不再是 3s 量级", elapsed)
	}
}
