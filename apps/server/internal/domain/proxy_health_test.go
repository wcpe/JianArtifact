package domain

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// forceWindowExpired 把已有仓库状态强行推进到「窗口已到、尚无探测在飞」，
// 用于确定性地验证闸门——不启动后台探测线程，避免它抢占名额让断言变成竞态。
// **只改写状态与窗口截止时间，保留退避档位序列**，否则会掩盖退避推进行为。
// 仓库不存在时按给定状态新建。
func forceWindowExpired(h *proxyHealth, repoID int64, state RemoteStatus, blockedUntil time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.repos[repoID]
	if st == nil {
		st = &remoteState{curBlock: time.Second}
		h.repos[repoID] = st
	}
	st.state = state
	st.blockedUntil = blockedUntil
	st.probeInFlight = false
	st.settleRound()
}

// newHalfOpenRepo 新建一个「处于半开、名额空闲」的仓库状态。
func newHalfOpenRepo(h *proxyHealth, repoID int64) {
	forceWindowExpired(h, repoID, StatusHalfOpen, time.Now().Add(-time.Second))
}

// TestBeginUpstreamAllowsExactlyOneConcurrentProbe 并发闸门（FR-43 §2.3 核心修复）：
// 多 goroutine 同时冲闸门，**恰好一个**被放行，其余在探测结论返回前等效封锁。
// 判定与置位在同一把锁内完成，故不可能出现两个请求同时放行（-race 下验证）。
func TestBeginUpstreamAllowsExactlyOneConcurrentProbe(t *testing.T) {
	cases := []struct {
		name         string
		state        RemoteStatus
		blockedUntil time.Time
	}{
		// 窗口内：全部封锁（零连接快速失败）。
		{name: "窗口内全封锁", state: StatusAutoBlocked, blockedUntil: time.Now().Add(time.Hour)},
		// 窗口已到、后台线程尚未切态：必须视同半开，只放行一个——旧实现正是在这里
		// 无条件放行全部流量（now.Before(blockedUntil) 立刻为 false）。
		{name: "窗口已到只放行一个", state: StatusAutoBlocked, blockedUntil: time.Now().Add(-time.Second)},
		// 已处于半开：同样只放行一个。
		{name: "已半开只放行一个", state: StatusHalfOpen, blockedUntil: time.Now().Add(-time.Second)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const repoID int64 = 1
			h := newProxyHealth(func(context.Context, string, string) error { return nil })
			forceWindowExpired(h, repoID, tc.state, tc.blockedUntil)

			const n = 32
			var wg sync.WaitGroup
			var granted int32
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start // 尽量让所有 goroutine 同时冲闸门
					if h.beginUpstream(repoID) {
						atomic.AddInt32(&granted, 1)
					}
				}()
			}
			close(start)
			wg.Wait()

			want := int32(1)
			if tc.name == "窗口内全封锁" {
				want = 0
			}
			if got := atomic.LoadInt32(&granted); got != want {
				t.Fatalf("放行数 = %d，期望 %d", got, want)
			}
			// 窗口到期后对外状态必须是 HALF_OPEN（不再把半开期误报为 AUTO_BLOCKED）。
			if tc.name != "窗口内全封锁" {
				if got := h.status(repoID).Status; got != StatusHalfOpen {
					t.Fatalf("半开期对外状态应为 HALF_OPEN，实际 %s", got)
				}
			}
		})
	}
}

// TestConcurrentProbeGateMatchesShouldBlock 闸门与查询式判定一致：
// shouldBlock 反映闸门实际结果，两类调用都不会出现「两个请求同时通过」。
// shouldBlock 本身是非占用式的，因此只断言「名额被占后它报告封锁」。
func TestConcurrentProbeGateMatchesShouldBlock(t *testing.T) {
	const repoID int64 = 3
	h := newProxyHealth(func(context.Context, string, string) error { return nil })
	newHalfOpenRepo(h, repoID)

	if h.shouldBlock(repoID) {
		t.Fatal("半开期名额空闲时不应报告封锁（否则首个探测永远发不出）")
	}
	if !h.beginUpstream(repoID) {
		t.Fatal("名额空闲时应放行")
	}
	if !h.shouldBlock(repoID) {
		t.Fatal("名额被占后应报告封锁（其余请求快速失败）")
	}
	if h.beginUpstream(repoID) {
		t.Fatal("名额被占后不得再放行")
	}
}

// TestHalfOpenFailureAdvancesBackoffOnce 半开探测失败：回到 AUTO_BLOCKED 并推进一档，
// 且名额被释放、下一轮半开可重新抢占。
func TestHalfOpenFailureAdvancesBackoffOnce(t *testing.T) {
	const repoID int64 = 5
	base := 30 * time.Millisecond
	h := newProxyHealth(func(context.Context, string, string) error { return errors.New("boom") })
	h.setBase(base)

	// 首次失败：进入 AUTO_BLOCKED 起始档。
	h.recordFailure(repoID, "http://upstream", "", UpstreamBlockReasonUpstreamError)
	if first := h.status(repoID); first.BlockedFor != base || first.Status != StatusAutoBlocked {
		t.Fatalf("首次失败应为 AUTO_BLOCKED/%s，实际 %+v", base, first)
	}
	forceWindowExpired(h, repoID, StatusAutoBlocked, time.Now().Add(-time.Second))

	// 抢到名额 → 半开；失败收口。
	if !h.beginUpstream(repoID) {
		t.Fatal("窗口到期后应放行本轮探测")
	}
	if got := h.status(repoID).Status; got != StatusHalfOpen {
		t.Fatalf("应进入 HALF_OPEN，实际 %s", got)
	}
	h.recordFailure(repoID, "http://upstream", "", UpstreamBlockReasonProbeFailed)
	second := h.status(repoID)
	if second.Status != StatusAutoBlocked {
		t.Fatalf("半开失败应回到 AUTO_BLOCKED，实际 %s", second.Status)
	}
	if second.BlockedFor != 2*base {
		t.Fatalf("退避应推进一档（%s），实际 %s", 2*base, second.BlockedFor)
	}
	// 名额已释放：下一轮半开可再次抢占。
	forceWindowExpired(h, repoID, StatusAutoBlocked, time.Now().Add(-time.Second))
	if !h.beginUpstream(repoID) {
		t.Fatal("新一轮半开应可再次抢占名额")
	}
}

// TestHalfOpenSuccessResetsBackoff 半开探测成功：回到 AVAILABLE 并重置退避，
// 后续再次失败从起始档重新计时。
func TestHalfOpenSuccessResetsBackoff(t *testing.T) {
	const repoID int64 = 9
	base := 30 * time.Millisecond
	h := newProxyHealth(func(context.Context, string, string) error { return errors.New("boom") })
	h.setBase(base)

	h.recordFailure(repoID, "http://upstream", "", UpstreamBlockReasonUpstreamError)
	forceWindowExpired(h, repoID, StatusAutoBlocked, time.Now().Add(-time.Second))
	if !h.beginUpstream(repoID) {
		t.Fatal("应放行探测")
	}
	h.recordFailure(repoID, "http://upstream", "", UpstreamBlockReasonProbeFailed)
	if got := h.status(repoID).BlockedFor; got != 2*base {
		t.Fatalf("第二轮窗口应为 2×base，实际 %s", got)
	}

	// 半开成功 → AVAILABLE 且退避归零。
	forceWindowExpired(h, repoID, StatusAutoBlocked, time.Now().Add(-time.Second))
	if !h.beginUpstream(repoID) {
		t.Fatal("应放行探测")
	}
	h.recordSuccess(repoID)
	got := h.status(repoID)
	if got.Status != StatusAvailable || !got.BlockedUntil.IsZero() || got.BlockedFor != 0 {
		t.Fatalf("复位不完整：%+v", got)
	}

	// 再次失败应从起始档重新开始（而非接着 2×base）。
	h.recordFailure(repoID, "http://upstream", "", UpstreamBlockReasonUpstreamError)
	if again := h.status(repoID).BlockedFor; again != base {
		t.Fatalf("复位后应从起始档重新计时（%s），实际 %s", base, again)
	}
}

// TestBeginUpstreamWithoutFailureIsTransparent 从未失败过的仓库直通（READY 路径不受闸门影响）。
func TestBeginUpstreamWithoutFailureIsTransparent(t *testing.T) {
	h := newProxyHealth(func(context.Context, string, string) error { return nil })
	if !h.beginUpstream(123) {
		t.Fatal("未记录过失败的仓库应直通")
	}
	if h.shouldBlock(123) {
		t.Fatal("未记录过失败的仓库不应被封锁")
	}
}

// TestReleaseProbeFreesHalfOpenSlot 「上游可达但资源缺失」（404/410）的收口路径必须释放名额，
// 否则后续请求会在整轮半开内被无限封锁。
func TestReleaseProbeFreesHalfOpenSlot(t *testing.T) {
	const repoID int64 = 17
	h := newProxyHealth(func(context.Context, string, string) error { return nil })
	newHalfOpenRepo(h, repoID)

	if !h.beginUpstream(repoID) {
		t.Fatal("应放行首个探测")
	}
	if h.beginUpstream(repoID) {
		t.Fatal("名额占用期间应封锁")
	}
	h.releaseProbe(repoID)
	if !h.beginUpstream(repoID) {
		t.Fatal("释放名额后应可再次抢占（否则整轮被无限封锁）")
	}
	// 释放名额不改状态：仍停留在半开期，未误报可用。
	if got := h.status(repoID).Status; got != StatusHalfOpen {
		t.Fatalf("释放名额不应改状态，应仍为 HALF_OPEN，实际 %s", got)
	}
	// 对非半开路径是幂等空操作（不应 panic，也不应改动状态）。
	h.releaseProbe(999)
}

// TestBlockedCountCountsBlockedAndHalfOpen 指标取数：阻止态仓库数按状态统计
// （AUTO_BLOCKED 与 HALF_OPEN 都算阻止中，与对外状态一致）。
func TestBlockedCountCountsBlockedAndHalfOpen(t *testing.T) {
	h := newProxyHealth(func(context.Context, string, string) error { return nil })
	h.setBase(time.Hour) // 窗口长，停在 AUTO_BLOCKED
	if got := h.blockedCount(); got != 0 {
		t.Fatalf("空状态应为 0，实际 %d", got)
	}
	h.recordFailure(1, "http://a", "", UpstreamBlockReasonUpstreamError)
	h.recordFailure(2, "http://b", "", UpstreamBlockReasonUpstreamError)
	h.recordSuccess(2)
	if got := h.blockedCount(); got != 1 {
		t.Fatalf("仅仓库 1 阻止中，应为 1，实际 %d", got)
	}
	// 半开期同样计入。
	newHalfOpenRepo(h, 3)
	if !h.beginUpstream(3) {
		t.Fatal("应放行探测")
	}
	if got := h.blockedCount(); got != 2 {
		t.Fatalf("AUTO_BLOCKED + HALF_OPEN 应为 2，实际 %d", got)
	}
}

// TestRecordFailureEmitsBlockEvent 阻止事件计数按闭集 reason 分区累加，
// 未接线记录器时不 panic。
func TestRecordFailureEmitsBlockEvent(t *testing.T) {
	const repoID int64 = 19
	spy := &recorderSpy{}
	h := newProxyHealth(func(context.Context, string, string) error { return errors.New("boom") })
	h.setBase(time.Millisecond)
	h.setBlockRecorder(spy)

	h.recordFailure(repoID, "http://upstream", "", UpstreamBlockReasonUpstreamError)
	if got := spy.reasons(); len(got) != 1 || got[0] != string(UpstreamBlockReasonUpstreamError) {
		t.Fatalf("应记录一次 upstream_error，实际 %v", got)
	}
	// 半开探测失败 → probe_failed。
	forceWindowExpired(h, repoID, StatusAutoBlocked, time.Now().Add(-time.Second))
	if !h.beginUpstream(repoID) {
		t.Fatal("应放行探测")
	}
	h.recordFailure(repoID, "http://upstream", "", UpstreamBlockReasonProbeFailed)
	if got := spy.reasons(); len(got) != 2 || got[1] != string(UpstreamBlockReasonProbeFailed) {
		t.Fatalf("应记录一次 probe_failed，实际 %v", got)
	}

	// 未接线记录器：不 panic、不记录。
	bare := newProxyHealth(func(context.Context, string, string) error { return errors.New("boom") })
	bare.recordFailure(repoID, "http://upstream", "", UpstreamBlockReasonUpstreamError)
}

// recorderSpy 是 UpstreamBlockRecorder 的测试替身，并发安全。
type recorderSpy struct {
	mu   sync.Mutex
	seen []string
}

func (s *recorderSpy) UpstreamBlockEvent(reason string) {
	s.mu.Lock()
	s.seen = append(s.seen, reason)
	s.mu.Unlock()
}

func (s *recorderSpy) reasons() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}
