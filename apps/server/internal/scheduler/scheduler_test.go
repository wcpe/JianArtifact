package scheduler

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 调度器使用真实时钟；测试以毫秒级间隔驱动，与仓库既有后台循环测试同口径。

// TestMain 静默日志：本包测试会刻意制造失败与 panic，避免污染测试输出。
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// statusByName 取出指定名称作业的状态快照。
func statusByName(t *testing.T, s *Scheduler, name string) JobStatus {
	t.Helper()
	for _, st := range s.Status() {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("状态中没有名为 %q 的作业", name)
	return JobStatus{}
}

// waitFor 在超时前轮询等待条件成立。
func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", desc)
}

func TestSchedulerDoesNotRunAtStartupAndRunsOnInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan struct{}, 4)
	s := New()
	s.Register("chk", 80*time.Millisecond, func(context.Context) error {
		calls <- struct{}{}
		return nil
	})
	s.Start(ctx)

	select {
	case <-calls:
		t.Fatal("作业不得在启动时立即执行")
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case <-calls:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("作业未按间隔触发")
	}
}

func TestSchedulerStopsAfterContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan struct{}, 16)
	s := New()
	s.Register("chk", 40*time.Millisecond, func(context.Context) error {
		calls <- struct{}{}
		return nil
	})
	s.Start(ctx)
	select {
	case <-calls:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("作业未按间隔触发")
	}

	cancel()
	// 清空取消前已入队的触发，再观察取消后是否仍有执行。
	for {
		select {
		case <-calls:
			continue
		default:
		}
		break
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(calls); n != 0 {
		t.Fatalf("context 取消后仍执行了 %d 次", n)
	}
}

func TestSchedulerSkipsInvalidJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran int32
	count := func(context.Context) error {
		atomic.AddInt32(&ran, 1)
		return nil
	}
	s := New()
	s.Register("", 20*time.Millisecond, count)      // 名称为空
	s.Register("zero", 0, count)                    // 间隔为 0
	s.Register("negative", -time.Second, count)     // 间隔为负
	s.Register("nil-run", 20*time.Millisecond, nil) // 执行函数为空
	s.Start(ctx)

	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("被禁用的作业不应执行，实际执行 %d 次", got)
	}
	if got := len(s.Status()); got != 0 {
		t.Fatalf("被禁用的作业不应进入状态，实际 %d 个", got)
	}
}

func TestSchedulerIsolatesFailingJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var healthy int32
	s := New()
	s.Register("bad", 30*time.Millisecond, func(context.Context) error {
		return errors.New("上游不可达")
	})
	s.Register("healthy", 30*time.Millisecond, func(context.Context) error {
		atomic.AddInt32(&healthy, 1)
		return nil
	})
	s.Start(ctx)

	// 以"失败已完成两次"为等待条件：Runs 在开始时计数，不能用来判断执行已结束。
	waitFor(t, "失败作业累计至少 2 次失败", func() bool {
		return statusByName(t, s, "bad").Failures >= 2
	})
	if atomic.LoadInt32(&healthy) == 0 {
		t.Fatal("单个作业持续失败导致其他作业停摆")
	}
	st := statusByName(t, s, "bad")
	if st.Runs < st.Failures {
		t.Fatalf("失败次数不应超过执行次数：Runs=%d Failures=%d", st.Runs, st.Failures)
	}
	if !strings.Contains(st.LastError, "上游不可达") {
		t.Fatalf("状态应记录最近一次错误，实际 %q", st.LastError)
	}
	ok := statusByName(t, s, "healthy")
	if ok.Failures != 0 || ok.LastError != "" {
		t.Fatalf("健康作业不应被记为失败：Failures=%d LastError=%q", ok.Failures, ok.LastError)
	}
}

func TestSchedulerIsolatesPanicAndKeepsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New()
	s.Register("boom", 30*time.Millisecond, func(context.Context) error {
		panic("炸了")
	})
	s.Start(ctx)

	// 以"panic 已完成两次"为等待条件：既证明 panic 被隔离（进程存活），
	// 也证明作业继续按周期执行（失败计数持续增长）。
	waitFor(t, "panic 作业累计至少 2 次失败", func() bool {
		return statusByName(t, s, "boom").Failures >= 2
	})
	st := statusByName(t, s, "boom")
	if !strings.Contains(st.LastError, "panic") {
		t.Fatalf("状态应标明 panic，实际 %q", st.LastError)
	}
}

func TestSchedulerDoesNotOverlapItself(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const runDur = 100 * time.Millisecond
	var mu sync.Mutex
	var starts []time.Time
	s := New()
	s.Register("slow", 20*time.Millisecond, func(context.Context) error {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		time.Sleep(runDur)
		return nil
	})
	s.Start(ctx)

	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	got := append([]time.Time(nil), starts...)
	mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("长作业未按周期执行，实际执行 %d 次", len(got))
	}
	for i := 1; i < len(got); i++ {
		if gap := got[i].Sub(got[i-1]); gap < runDur {
			t.Fatalf("第 %d 次执行距上一次仅 %v，小于上次耗时 %v：发生了重叠或补跑", i, gap, runDur)
		}
	}
}

func TestSchedulerStatusReflectsLastResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int32
	s := New()
	s.Register("flaky", 30*time.Millisecond, func(context.Context) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			return errors.New("首次失败")
		}
		return nil
	})
	s.Start(ctx)

	waitFor(t, "失败一次后成功一次", func() bool {
		st := statusByName(t, s, "flaky")
		return st.Runs >= 2 && !st.Running
	})
	st := statusByName(t, s, "flaky")
	if st.Failures != 1 {
		t.Fatalf("失败次数应为 1，实际 %d", st.Failures)
	}
	if st.LastError != "" {
		t.Fatalf("最近一次成功不应残留错误：%q", st.LastError)
	}
	if st.Interval != 30*time.Millisecond {
		t.Fatalf("状态应携带注册间隔，实际 %v", st.Interval)
	}
	if st.LastStart.IsZero() || st.LastFinish.IsZero() || st.LastFinish.Before(st.LastStart) {
		t.Fatalf("应记录最近执行时间：start=%v finish=%v", st.LastStart, st.LastFinish)
	}
}

func TestSchedulerStatusReportsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	s := New()
	s.Register("blocked", 20*time.Millisecond, func(context.Context) error {
		close(started)
		<-release
		return nil
	})
	s.Start(ctx)

	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("作业未按间隔触发")
	}
	if !statusByName(t, s, "blocked").Running {
		t.Fatal("执行期间应标记为运行中")
	}
	close(release)
	waitFor(t, "执行结束后不再处于运行中", func() bool {
		return !statusByName(t, s, "blocked").Running
	})
}

func TestSchedulerStatusIsConcurrencySafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New()
	s.Register("a", 5*time.Millisecond, func(context.Context) error { return nil })
	s.Register("b", 5*time.Millisecond, func(context.Context) error { return errors.New("持续失败") })
	s.Start(ctx)

	// 与执行并发读取状态；数据竞争由 go test -race 判定。
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(150 * time.Millisecond)
			for time.Now().Before(deadline) {
				_ = s.Status()
			}
		}()
	}
	wg.Wait()
}

// TestSchedulerCountersSettleOnCompletion 锁定计数语义：Runs 在每次执行开始时计入、
// Failures 在结束时回填，因此执行中两者可能瞬时不等；判断作业是否跑完必须看 Running。
func TestSchedulerCountersSettleOnCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold := make(chan struct{})
	s := New()
	s.Register("hold", 20*time.Millisecond, func(context.Context) error {
		<-hold
		return errors.New("失败")
	})
	s.Start(ctx)

	waitFor(t, "作业进入运行中", func() bool {
		return statusByName(t, s, "hold").Running
	})
	mid := statusByName(t, s, "hold")
	if mid.Runs != 1 || mid.Failures != 0 {
		t.Fatalf("执行中应先计执行次数、后计失败次数：Runs=%d Failures=%d", mid.Runs, mid.Failures)
	}

	close(hold)
	waitFor(t, "失败在结束后回填", func() bool {
		return statusByName(t, s, "hold").Failures >= 1
	})
	// 停止调度并等最后一个在途运行结束，此后计数不再变化，才可断言两者一致。
	cancel()
	waitFor(t, "在途运行结束", func() bool {
		return !statusByName(t, s, "hold").Running
	})
	st := statusByName(t, s, "hold")
	if st.Runs != st.Failures {
		t.Fatalf("每轮都失败的作业在停止后计数应一致：Runs=%d Failures=%d", st.Runs, st.Failures)
	}
}

// TestSchedulerRunRejectsUnknownJob 锁定按名触发的 404 语义：未注册（含间隔 <= 0 被禁用）
// 的名称必须返回可判别的 ErrJobNotFound，而不是静默成功。
func TestSchedulerRunRejectsUnknownJob(t *testing.T) {
	s := New()
	s.Register("disabled", 0, func(context.Context) error { return nil }) // 间隔 0 = 禁用，不注册

	err := s.Run("ghost")
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("未知作业名应返回 ErrJobNotFound，得 %v", err)
	}
	if err := s.Run("disabled"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("被禁用的作业应等价于未注册，得 %v", err)
	}
}

// TestSchedulerRunExecutesOnceAndBackfillsStatus 锁定触发语义：受理后异步执行一次，
// 结果照既有机制回填 Status（runs 递增、lastError 记录失败），且不阻塞调用方。
func TestSchedulerRunExecutesOnceAndBackfillsStatus(t *testing.T) {
	done := make(chan struct{})
	s := New()
	s.Register("manual", time.Hour, func(context.Context) error {
		close(done)
		return errors.New("手动触发失败")
	})

	// Start 之前即可触发（尚无运行 ctx 时以 Background 执行）。
	if err := s.Run("manual"); err != nil {
		t.Fatalf("首次触发应被受理，得 %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("受理后作业未执行")
	}
	waitFor(t, "失败在结束后回填", func() bool { return statusByName(t, s, "manual").Failures == 1 })

	st := statusByName(t, s, "manual")
	if st.Runs != 1 || st.Running {
		t.Fatalf("触发一轮应 runs=1 且已结束：Runs=%d Running=%v", st.Runs, st.Running)
	}
	if st.LastStart.IsZero() || st.LastFinish.Before(st.LastStart) {
		t.Fatalf("应记录起止时间：start=%v finish=%v", st.LastStart, st.LastFinish)
	}
	if !strings.Contains(st.LastError, "手动触发失败") {
		t.Fatalf("状态应记录本次失败，实际 %q", st.LastError)
	}
	if st.Failures != 1 {
		t.Fatalf("失败次数应为 1，实际 %d", st.Failures)
	}
}

// TestSchedulerRunRejectsWhileRunning 锁定"不自我重叠"：作业执行中再次触发必须被拒，
// 绝不能并发跑第二次（这是调度器既有语义，手动触发不得绕过）。
func TestSchedulerRunRejectsWhileRunning(t *testing.T) {
	release := make(chan struct{})
	var calls int32
	s := New()
	s.Register("busy", time.Hour, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		<-release
		return nil
	})

	if err := s.Run("busy"); err != nil {
		t.Fatalf("首次触发应被受理，得 %v", err)
	}
	// 以"作业函数已进入"为等待条件：claim 先置运行位、再启动 goroutine，只看 Running 会在函数体执行前返回。
	waitFor(t, "作业函数开始执行", func() bool { return atomic.LoadInt32(&calls) == 1 })

	if err := s.Run("busy"); !errors.Is(err, ErrJobRunning) {
		t.Fatalf("运行中触发应返回 ErrJobRunning，得 %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("运行中触发不得并发跑第二次，实际执行 %d 次", got)
	}

	close(release)
	waitFor(t, "执行结束后可再次触发", func() bool { return !statusByName(t, s, "busy").Running })
	if err := s.Run("busy"); err != nil {
		t.Fatalf("上一轮结束后应可再次触发，得 %v", err)
	}
	// release 已关闭，第二次触发的函数体直接返回。
	waitFor(t, "第二次触发开始执行", func() bool { return atomic.LoadInt32(&calls) == 2 })
}

// TestSchedulerRunConcurrentTriggersOnlyOneAccepted 锁定并发双击只跑一次：
// 多个并发的按名触发里恰好一个被受理，其余全部按"正在运行"拒绝，作业函数只执行一次。
func TestSchedulerRunConcurrentTriggersOnlyOneAccepted(t *testing.T) {
	release := make(chan struct{})
	var calls int32
	s := New()
	s.Register("once", time.Hour, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		<-release
		return nil
	})

	const n = 8
	results := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx] = s.Run("once")
		}(i)
	}
	close(start)
	wg.Wait()

	accepted := 0
	for _, err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrJobRunning):
		default:
			t.Fatalf("并发触发只应出现受理或运行中两种结果，得 %v", err)
		}
	}
	if accepted != 1 {
		t.Fatalf("并发触发应只受理一次，实际受理 %d 次", accepted)
	}
	waitFor(t, "作业函数开始执行", func() bool { return atomic.LoadInt32(&calls) == 1 })
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("作业函数只应执行一次，实际 %d 次", got)
	}

	close(release)
	waitFor(t, "执行结束", func() bool { return !statusByName(t, s, "once").Running })
	if st := statusByName(t, s, "once"); st.Runs != 1 {
		t.Fatalf("受理一轮应只计一次执行，实际 Runs=%d", st.Runs)
	}
}

// TestSchedulerRunSharesRunningFlagWithLoop 锁定手动触发与周期触发共用同一"运行中"状态位：
// 周期那一轮正在跑时手动触发同样被拒，两条路径之间也不会并发重入。
func TestSchedulerRunSharesRunningFlagWithLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s := New()
	s.Register("shared", 20*time.Millisecond, func(context.Context) error {
		once.Do(func() { close(started) })
		<-release
		return nil
	})
	s.Start(ctx)
	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("周期作业未触发")
	}

	if err := s.Run("shared"); !errors.Is(err, ErrJobRunning) {
		t.Fatalf("周期作业运行中手动触发应被拒（ErrJobRunning），得 %v", err)
	}
	close(release)
}
