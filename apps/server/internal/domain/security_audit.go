package domain

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

var errSecurityAuditStopped = errors.New("安全审计采集已停止，不能重新启动")

// SecurityAuditHealth 只报告脱敏采集状态。水位是最近一次已证实事务可见的事件序号；
// 覆盖状态仅针对 [CoveredFrom, CoveredThrough)；边界之外或区间为空均未知。
// 计数是本进程观察到的事实，不能当作已落库数。
type SecurityAuditHealth struct {
	State, Coverage, Failure       string
	Watermark                      int64
	LastSuccess, HeartbeatAt       time.Time
	CoveredFrom, CoveredThrough    time.Time
	LastGap                        repository.SecurityAuditGap
	Queued, QueueBytes             int
	ObservedCount, SuppressedCount int64
}

// SecurityAuditService 并列提供非关键事实的有界采集与关键事务内审计。
type SecurityAuditService struct {
	coverage *repository.SecurityCoverageRepo
	events   *repository.SecurityEventRepo

	mu               sync.Mutex
	wake, done       chan struct{}
	cancel           context.CancelFunc
	started          bool
	closing          bool
	cleanStop        bool
	finished         bool
	closeAt          time.Time
	stopErr          error
	queue            securityAuditBuffer
	gaps             securityAuditGaps
	flushAt          time.Time
	wallAt           time.Time
	checkpoint       repository.SecurityAuditCheckpoint
	inflightCoverage string
	lastSuccess      time.Time
	failure          string
	observed         int64
	suppressed       int64
	unpersisted      bool
}

// NewSecurityAuditService 构造安全审计。关键事务 hook 不依赖 Start 或异步存储健康。
func NewSecurityAuditService(db *persistence.DB) *SecurityAuditService {
	return &SecurityAuditService{coverage: repository.NewSecurityCoverageRepo(db), events: repository.NewSecurityEventRepo(db),
		wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// Start 启动唯一采集工人。以服务生命周期为上下文，不传单个 HTTP 请求；
// SQLite 尚不可用时后台重试，Health 在首个可靠检查点前不会报告完整覆盖。
func (s *SecurityAuditService) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.finished {
		return errSecurityAuditStopped
	}
	if !s.started {
		run, cancel := context.WithCancel(ctx)
		s.started, s.cancel = true, cancel
		s.wallAt = time.Now().UTC()
		go s.runWorker(run)
	}
	return nil
}

// Close 在停止入口接收后调用；排空已接收事实，然后设置持久停止标记。
// 截止时间只限制排空；到期后取消工人，仍等在途事务回滚和连接归还后返回。
// 未确认停止则下一次启动报告未知尾段。
func (s *SecurityAuditService) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closing {
		s.closing = true
		s.closeAt = s.advanceClock(time.Now())
		if !s.started {
			s.finished = true
			close(s.done)
		}
	}
	cancel := s.cancel
	s.mu.Unlock()
	s.notifyWorker()
	var deadlineErr error
	select {
	case <-s.done:
	case <-ctx.Done():
		deadlineErr = ctx.Err()
		if cancel != nil {
			cancel()
		}
		// 工人只有短 SQLite 写入、连接恢复和可取消的等待，没有外部回调或网络。
		<-s.done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if deadlineErr != nil && s.started && !s.cleanStop {
		return deadlineErr
	}
	return s.stopErr
}

// Health 在内存中读取有界快照，不访问数据库且不暴露来源、对象或数据库错误文本。
func (s *SecurityAuditService) Health() SecurityAuditHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.checkpoint
	h := SecurityAuditHealth{State: "running", Coverage: cp.Coverage, Failure: s.failure, Watermark: cp.Watermark,
		LastSuccess: s.lastSuccess, HeartbeatAt: cp.HeartbeatAt, CoveredFrom: cp.CoveredFrom, CoveredThrough: cp.CoveredThrough, LastGap: cp.LastGap,
		Queued: s.queue.count, QueueBytes: s.queue.bytes, ObservedCount: s.observed, SuppressedCount: s.suppressed}
	switch {
	case s.finished:
		h.State = "stopped"
	case s.closing:
		h.State = "stopping"
	case s.failure != "":
		h.State = "degraded"
	case s.lastSuccess.IsZero():
		h.State = "starting"
	}
	if s.failure != "" || h.Coverage == "" || s.unpersisted || s.inflightCoverage == "unknown" || !s.gaps.overflow.Start.IsZero() {
		h.Coverage = "unknown"
	} else if h.Coverage == "complete" && (s.inflightCoverage == "gapped" || s.gaps.size != 0) {
		h.Coverage = "gapped"
	}
	h.LastGap.ObservedCount = copyAuditCount(h.LastGap.ObservedCount)
	h.LastGap.SuppressedCount = copyAuditCount(h.LastGap.SuppressedCount)
	return h
}

func copyAuditCount(p *int64) *int64 {
	if p == nil {
		return nil
	}
	n := *p
	return &n
}

func (s *SecurityAuditService) enqueue(o repository.SecurityAuditObservation) {
	now := time.Now()
	o.Event.OccurredAt = now.UTC()
	o.Event.EventID = repository.NewSecurityEventID()
	invalid := repository.ValidateSecurityAuditObservation(o) != nil
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.observed < math.MaxInt64 {
		s.observed++
	}
	if !s.started || s.closing || s.finished {
		s.incrementSuppressed()
		s.unpersisted = true
		return
	}
	o.Event.OccurredAt = s.advanceClock(now)
	if invalid {
		s.gaps.add(o.Event.OccurredAt, "invalid_fact")
		s.incrementSuppressed()
		return
	}
	o, size := detachSecurityAuditObservation(o)
	reason := s.queue.offer(o, o.Event.ClientIP, now, size)
	if reason != "" {
		if reason == "queue_full" && s.failure == "storage_unavailable" {
			reason = "database_unavailable"
		}
		s.gaps.add(o.Event.OccurredAt, reason)
		s.incrementSuppressed()
		return
	}
	if s.queue.size == 1 {
		s.flushAt = now.Add(securityAuditBatchDelay)
	}
	if s.queue.size == 1 || s.queue.size == repository.SecurityAuditBatchLimit {
		s.notifyWorker()
	}
}

func (s *SecurityAuditService) incrementSuppressed() {
	if s.suppressed < math.MaxInt64 {
		s.suppressed++
	}
}

func (s *SecurityAuditService) advanceClock(now time.Time) time.Time {
	if wall := now.UTC(); s.wallAt.Before(wall) {
		s.wallAt = wall
	}
	return s.wallAt
}

func (s *SecurityAuditService) notifyWorker() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
