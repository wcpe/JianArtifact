// Package scheduler 提供进程内周期作业调度：统一触发语义、失败隔离与状态可见。
//
// 语义固化见 docs/specs/0.11.0-job-scheduler.md（FR-44）。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"
)

// JobStatus 是单个作业的运行状态快照，供诊断与指标导出（FR-39）。
type JobStatus struct {
	// Name 是注册时的作业名称。
	Name string
	// Interval 是触发间隔。
	Interval time.Duration
	// Runs 是已执行次数（含失败）。
	Runs int64
	// Failures 是失败次数（含 panic）。
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
func (s *Scheduler) Start(ctx context.Context) {
	for _, j := range s.jobs {
		go j.loop(ctx)
	}
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

// runOnce 执行一次作业并记录状态；失败与 panic 都隔离在本作业内。
func (j *job) runOnce(ctx context.Context) {
	j.mu.Lock()
	j.runs++
	j.running = true
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
