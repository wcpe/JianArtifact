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

	waitFor(t, "失败作业累计至少 2 次执行", func() bool {
		return statusByName(t, s, "bad").Runs >= 2
	})
	if atomic.LoadInt32(&healthy) == 0 {
		t.Fatal("单个作业持续失败导致其他作业停摆")
	}
	st := statusByName(t, s, "bad")
	if st.Failures != st.Runs {
		t.Fatalf("持续失败的作业失败次数应等于执行次数：Runs=%d Failures=%d", st.Runs, st.Failures)
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

	waitFor(t, "panic 作业继续按周期执行", func() bool {
		return statusByName(t, s, "boom").Runs >= 2
	})
	st := statusByName(t, s, "boom")
	if st.Failures != st.Runs {
		t.Fatalf("panic 应计入失败：Runs=%d Failures=%d", st.Runs, st.Failures)
	}
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
