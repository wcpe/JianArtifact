// Package scheduler 提供进程内周期作业调度：统一触发语义、失败隔离与状态可见。
//
// 语义固化见 docs/specs/0.11.0-job-scheduler.md（FR-44）。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"
)

// 按名触发（Run）的可判别错误：上层据此映射 404 / 409，无需解析错误文本。
var (
	// ErrJobNotFound 表示该名称没有已注册的作业（未注册、间隔 <= 0 被禁用或名称拼写错误）。
	ErrJobNotFound = errors.New("作业未注册")
	// ErrJobRunning 表示该作业正在运行，本次触发被拒（不排队、不并发重入）。
	ErrJobRunning = errors.New("作业正在运行")
)

// JobStatus 是单个作业的运行状态快照，供诊断与指标导出（FR-39）。
type JobStatus struct {
	// Name 是注册时的作业名称。
	Name string
	// Interval 是触发间隔。
	Interval time.Duration
	// Runs 是已执行次数（含失败），在每次执行开始时计入。
	Runs int64
	// Failures 是失败次数（含 panic），在每次执行结束时回填；
	// 执行中 Runs 与 Failures 可能瞬时不等，判断作业是否跑完请用 Running。
	Failures int64
	// Running 表示当前是否正在执行。
	Running bool
	// LastStart 是最近一次开始时间；零值表示尚未执行。
	LastStart time.Time
	// LastFinish 是最近一次结束时间；零值表示尚未执行完成。
	LastFinish time.Time
	// LastError 是最近一次失败描述；为空表示最近一次执行成功。
	LastError string
}

// jobFailure 描述一次失败执行：普通错误或 panic（附调用堆栈）。
type jobFailure struct {
	err   error
	stack []byte
}

// job 是单个已注册作业及其运行状态。
type job struct {
	name     string
	interval time.Duration
	run      func(ctx context.Context) error

	mu         sync.Mutex
	runs       int64
	failures   int64
	running    bool
	lastStart  time.Time
	lastFinish time.Time
	lastError  string
}

// Scheduler 在进程内按固定间隔运行已注册作业，触发语义统一固化：
//   - 启动不立即执行，首次执行在第一个间隔到期后（不扫描历史数据）；
//   - 名称、间隔或执行函数任一无效时不注册（间隔 <= 0 即禁用）；
//   - 单作业失败或 panic 只记日志与计数，不影响其他作业与后续周期；
//   - 同一作业不自我重叠：上一次结束后再等一个间隔。
type Scheduler struct {
	jobs []*job

	// mu 保护 ctx：Start 在启动时写入，Run 在请求期读取，两者可能来自不同 goroutine。
	mu  sync.RWMutex
	ctx context.Context
}

// New 创建空调度器；作业须在 Start 之前注册。
func New() *Scheduler {
	return &Scheduler{}
}

// Register 注册周期作业；名称、间隔或执行函数任一无效时静默跳过（禁用语义）。
func (s *Scheduler) Register(name string, interval time.Duration, run func(ctx context.Context) error) {
	if name == "" || interval <= 0 || run == nil {
		return
	}
	s.jobs = append(s.jobs, &job{name: name, interval: interval, run: run})
}

// Start 为每个已注册作业启动独立循环，随 ctx 取消优雅停止。
// 手动触发（Run）沿用同一个 ctx，因此关闭流程对两者一致。
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	for _, j := range s.jobs {
		go j.loop(ctx)
	}
}

// Run 按名触发一次作业执行：取得独占权后异步启动并立即返回，不等待作业跑完——
// 执行结果照既有机制回填 Status（runs / failures / lastError / lastFinish）。
//
// 返回的可判别错误：
//   - ErrJobNotFound：该名称没有已注册的作业；
//   - ErrJobRunning：该作业正在运行（含周期触发的那一轮），本次触发被拒。
//
// 本方法**不改变**既有调度语义：同一作业不自我重叠，被拒的触发不排队、后续不补跑。
// Run 可在 Start 之前调用（尚无运行 ctx 时以 context.Background() 执行）。
func (s *Scheduler) Run(name string) error {
	j := s.job(name)
	if j == nil {
		return fmt.Errorf("%w：%s", ErrJobNotFound, name)
	}
	if !j.claim() {
		return fmt.Errorf("%w：%s", ErrJobRunning, name)
	}
	go j.runClaimed(s.runContext())
	return nil
}

// job 按名查找已注册作业；不存在返回 nil。
func (s *Scheduler) job(name string) *job {
	for _, j := range s.jobs {
		if j.name == name {
			return j
		}
	}
	return nil
}

// runContext 返回作业执行所用的上下文：优先用 Start 传入的 ctx（随其取消优雅停止），
// 尚未 Start 时退回 context.Background()。
func (s *Scheduler) runContext() context.Context {
	s.mu.RLock()
	ctx := s.ctx
	s.mu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// Status 返回全部作业的状态快照（副本，可并发调用）。
func (s *Scheduler) Status() []JobStatus {
	out := make([]JobStatus, 0, len(s.jobs))
	for _, j := range s.jobs {
		j.mu.Lock()
		out = append(out, JobStatus{
			Name:       j.name,
			Interval:   j.interval,
			Runs:       j.runs,
			Failures:   j.failures,
			Running:    j.running,
			LastStart:  j.lastStart,
			LastFinish: j.lastFinish,
			LastError:  j.lastError,
		})
		j.mu.Unlock()
	}
	return out
}

// loop 按间隔驱动单个作业：每轮重新起算定时，作业耗时超过间隔时不会补跑连发。
func (j *job) loop(ctx context.Context) {
	for {
		timer := time.NewTimer(j.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		j.runOnce(ctx)
	}
}

// runOnce 由周期循环调用：先取得独占权，再同步执行一轮。
// 周期循环本身是串行的，取不到独占权只可能是手动触发那一轮正在运行——
// 此时保持"不自我重叠"：跳过本轮，不排队、不补跑（下一轮照常重新起算间隔）。
func (j *job) runOnce(ctx context.Context) {
	if !j.claim() {
		return
	}
	j.runClaimed(ctx)
}

// claim 尝试独占本轮执行：已在运行时返回 false。
// 周期触发与手动触发共用这一个状态位，因此两条路径之间也不会并发重入。
func (j *job) claim() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running {
		return false
	}
	j.running = true
	return true
}

// runClaimed 执行一轮已取得独占权的作业并记录状态；失败与 panic 都隔离在本作业内。
func (j *job) runClaimed(ctx context.Context) {
	j.mu.Lock()
	j.runs++
	j.lastStart = time.Now()
	j.mu.Unlock()

	failure := j.invoke(ctx)

	j.mu.Lock()
	j.running = false
	j.lastFinish = time.Now()
	if failure != nil {
		j.failures++
		j.lastError = failure.err.Error()
	} else {
		j.lastError = ""
	}
	j.mu.Unlock()

	if failure == nil {
		return
	}
	if len(failure.stack) > 0 {
		log.Printf("定时任务 %s 发生 panic：%v\n%s", j.name, failure.err, failure.stack)
		return
	}
	log.Printf("定时任务 %s 执行失败：%v", j.name, failure.err)
}

// invoke 调用作业并隔离 panic：panic 转为失败返回，单个作业不会拖垮进程。
func (j *job) invoke(ctx context.Context) (failure *jobFailure) {
	defer func() {
		if r := recover(); r != nil {
			failure = &jobFailure{err: fmt.Errorf("panic：%v", r), stack: debug.Stack()}
		}
	}()
	if err := j.run(ctx); err != nil {
		return &jobFailure{err: err}
	}
	return nil
}
