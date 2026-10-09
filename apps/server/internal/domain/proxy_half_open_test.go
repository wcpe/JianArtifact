package domain_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// TestResolveProxyHalfOpenBlocksBusinessTrafficBehindProbe 端到端验证 FR-43 的流量闸门：
// 上游持续 5xx → AUTO_BLOCKED；窗口到期进入 HALF_OPEN（对外状态可见，不再误报
// AUTO_BLOCKED）；本轮唯一的探测（后台 HEAD）停在飞行中时，并发发起多个真实业务请求，
// **一个都不放行**（快速失败），上游在此期间不再被真实回源碰一次。
//
// 这是本任务修掉的行为缺口：旧实现窗口一到（now.Before(blockedUntil) 立刻为 false）
// 就把全部真实业务流量无条件放行。
func TestResolveProxyHalfOpenBlocksBusinessTrafficBehindProbe(t *testing.T) {
	// HEAD（后台探测）阻塞在飞行中，用于把本轮半开固定在「探测已发出、结论未到」的窗口里；
	// GET（真实回源）计数，用于断言业务流量是否穿透闸门。
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock() // 提前失败也要放行探测，否则 srv.Close 会等待挂起请求
	var gets, heads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			atomic.AddInt32(&heads, 1)
			<-release
		} else {
			atomic.AddInt32(&gets, 1)
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	base := 60 * time.Millisecond
	svc.SetAutoBlockBase(base)
	repoID, err := repos.Create("half-block-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 half-block-proxy：%v", err)
	}

	// 首次回源失败 → AUTO_BLOCKED（第一档）。
	if _, _, err := svc.Resolve(context.Background(), "half-block-proxy", "first.jar"); !errorsIsUpstream(err) {
		t.Fatalf("首次应回源失败（ErrUpstream），实际：%v", err)
	}
	if got := svc.Status(repoID).Status; got != domain.StatusAutoBlocked {
		t.Fatalf("首次失败后应 AUTO_BLOCKED，实际 %s", got)
	}
	if got := atomic.LoadInt32(&gets); got != 1 {
		t.Fatalf("首次应真实回源 1 次，实际 %d 次", got)
	}

	// 窗口到期：状态应切到 HALF_OPEN（不再把半开期误报为 AUTO_BLOCKED），
	// 后台线程随即发出本轮唯一探测并阻塞在飞行中。
	waitForStatus(t, svc, repoID, domain.StatusHalfOpen, 3*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&heads) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&heads); got != 1 {
		t.Fatalf("半开期应只发出 1 次探测，实际 %d 次", got)
	}

	// 探测在飞期间并发发起 16 个真实业务请求：全部必须被闸门快速失败，零连接。
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, _, err := svc.Resolve(context.Background(), "half-block-proxy", "concurrent.jar")
			errs[idx] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if !errorsIsUpstream(err) {
			t.Fatalf("并发请求[%d]应快速失败（ErrUpstream），实际：%v", i, err)
		}
	}
	// 关键断言：整轮半开只碰上游一次（那次探测），业务流量一个都没穿透。
	if got := atomic.LoadInt32(&gets); got != 1 {
		t.Fatalf("半开期业务流量不得穿透闸门（真实回源应仍为 1 次），实际 %d 次", got)
	}
	if got := atomic.LoadInt32(&heads); got != 1 {
		t.Fatalf("整轮半开只应有 1 次上游交互，实际探测 %d 次", got)
	}

	// 放开探测：失败收口 → 回到 AUTO_BLOCKED 且档位推进一档。
	// 收口发生在被释放的探测请求返回之后，故轮询等待（不假定即时可见）。
	unblock()
	deadline = time.Now().Add(3 * time.Second)
	var advanced domain.RemoteHealth
	for time.Now().Before(deadline) {
		advanced = svc.Status(repoID)
		if advanced.BlockedFor > base {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if advanced.BlockedFor <= base {
		t.Fatalf("半开探测失败后应推进退避档位（>%s），实际 %s", base, advanced.BlockedFor)
	}
	if advanced.Status != domain.StatusAutoBlocked {
		t.Fatalf("半开探测失败后应回到 AUTO_BLOCKED，实际 %s", advanced.Status)
	}
}

// TestResolveProxyHalfOpenProbeSuccessRecovers 半开探测成功 → AVAILABLE 并重置退避，
// 随后业务请求正常回源（闸门放行）。
func TestResolveProxyHalfOpenProbeSuccessRecovers(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	payload := []byte("recovered-after-half-open")
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
	base := 60 * time.Millisecond
	svc.SetAutoBlockBase(base)
	repoID, err := repos.Create("half-rec-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 half-rec-proxy：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "half-rec-proxy", "a.jar"); !errorsIsUpstream(err) {
		t.Fatalf("首次应回源失败，实际：%v", err)
	}
	// 上游恢复：后台探测在窗口到期后成功 → AVAILABLE（经 HALF_OPEN 过渡）。
	fail.Store(false)
	waitForStatus(t, svc, repoID, domain.StatusAvailable, 3*time.Second)
	if got := svc.Status(repoID); !got.BlockedUntil.IsZero() || got.BlockedFor != 0 {
		t.Fatalf("恢复后应清空窗口与退避档位，实际 %+v", got)
	}
	// 闸门放行：业务请求正常回源成功。
	if _, rc, err := svc.Resolve(context.Background(), "half-rec-proxy", "b.jar"); err != nil {
		t.Fatalf("恢复后应可回源，实际：%v", err)
	} else {
		_ = readClose(t, rc)
	}
}
