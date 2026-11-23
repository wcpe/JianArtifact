package domain

import (
	"context"
	"errors"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

const (
	securityAuditBatchDelay   = 10 * time.Millisecond
	securityAuditHeartbeat    = time.Second
	securityAuditYield        = time.Millisecond
	securityAuditBusyRetry    = 50 * time.Millisecond
	securityAuditStorageRetry = 250 * time.Millisecond
	securityAuditWriteTimeout = 250 * time.Millisecond
)

func (s *SecurityAuditService) runWorker(ctx context.Context) {
	defer s.finishWorker(ctx)
	worker := repository.NewSecurityEventID()
	for {
		writeCtx, cancel := context.WithTimeout(ctx, securityAuditWriteTimeout)
		checkpoint, err := s.coverage.Open(writeCtx, worker, time.Time{})
		cancel()
		if err == nil {
			s.confirmCheckpoint(checkpoint, nil, false)
			break
		}
		if !s.retryWorker(ctx, err) {
			return
		}
	}
	for number := int64(1); ctx.Err() == nil; number++ {
		batch, items, ok := s.nextBatch(ctx, number)
		if !ok {
			return
		}
		for {
			writeCtx, cancel := context.WithTimeout(ctx, securityAuditWriteTimeout)
			checkpoint, err := s.coverage.Write(writeCtx, worker, batch)
			cancel()
			if err == nil {
				s.confirmCheckpoint(checkpoint, items, batch.CleanStop)
				break
			}
			if !s.retryWorker(ctx, err) {
				return
			}
		}
		if batch.CleanStop || !waitSecurityAudit(ctx, nil, securityAuditYield) {
			return
		}
	}
}

func (s *SecurityAuditService) retryWorker(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	failure, retry := "storage_unavailable", securityAuditStorageRetry
	if errors.Is(err, repository.ErrSecurityCoverageBusy) {
		failure, retry = "writer_busy", securityAuditBusyRetry
	}
	if errors.Is(err, repository.ErrSecurityCoverageLease) || errors.Is(err, repository.ErrSecurityCoverageOrder) {
		s.mu.Lock()
		s.failure, s.stopErr = "worker_fenced", err
		s.mu.Unlock()
		return false
	}
	s.mu.Lock()
	s.failure = failure
	s.mu.Unlock()
	// 洪峰信号不打断让路或故障退避，避免频繁与关键事务争锁。
	return waitSecurityAudit(ctx, nil, retry)
}

func (s *SecurityAuditService) confirmCheckpoint(checkpoint repository.SecurityAuditCheckpoint, items []*securityAuditQueued, clean bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint, s.lastSuccess, s.failure, s.cleanStop = checkpoint, time.Now(), "", clean
	s.inflightCoverage = ""
	s.queue.release(items)
}

func (s *SecurityAuditService) finishWorker(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil && s.stopErr == nil && !s.cleanStop {
		s.failure, s.stopErr = "worker_cancelled", err
	}
	if s.stopErr != nil {
		s.unpersisted = true
	}
	s.finished = true
	s.queue = securityAuditBuffer{}
	s.gaps = securityAuditGaps{}
	s.cancel()
	close(s.done)
}

func (s *SecurityAuditService) nextBatch(ctx context.Context, number int64) (repository.SecurityAuditBatch, []*securityAuditQueued, bool) {
	for ctx.Err() == nil {
		now := time.Now()
		s.mu.Lock()
		wait := s.lastSuccess.Add(securityAuditHeartbeat).Sub(now)
		if s.queue.size > 0 {
			wait = min(wait, s.flushAt.Sub(now))
		}
		if s.closing || s.queue.size >= repository.SecurityAuditBatchLimit || wait <= 0 {
			batch, items := s.takeBatch(now, number)
			s.mu.Unlock()
			return batch, items, true
		}
		s.mu.Unlock()
		if !waitSecurityAudit(ctx, s.wake, wait) {
			break
		}
	}
	return repository.SecurityAuditBatch{}, nil, false
}

// takeBatch 在锁内截取一个精确重放单位，剩余最早的事实限制可证明的覆盖水位。
func (s *SecurityAuditService) takeBatch(now time.Time, number int64) (repository.SecurityAuditBatch, []*securityAuditQueued) {
	through := s.advanceClock(now)
	heartbeat := through
	if s.closing {
		through = s.closeAt
	}
	items := s.queue.take(repository.SecurityAuditBatchLimit)
	gaps := s.gaps.take(repository.SecurityAuditBatchLimit - len(items))
	for _, gap := range gaps {
		if s.inflightCoverage != "unknown" {
			s.inflightCoverage = gap.Coverage
		}
	}
	clean := s.closing && len(items) == 0 && len(gaps) == 0
	if s.queue.size > 0 {
		through = minSecurityAuditTime(through, s.queue.entries[s.queue.head].observation.Event.OccurredAt)
	}
	if oldest := s.gaps.oldest(); !oldest.IsZero() {
		through = minSecurityAuditTime(through, oldest)
	}
	var observations []repository.SecurityAuditObservation
	if len(items) > 0 {
		observations = make([]repository.SecurityAuditObservation, len(items))
		for i, item := range items {
			observations[i] = item.observation
		}
	}
	return repository.SecurityAuditBatch{Number: number, ID: repository.NewSecurityEventID(), HeartbeatAt: heartbeat,
		CoveredThrough: through, CleanStop: clean, Observations: observations, Gaps: gaps}, items
}

func minSecurityAuditTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func waitSecurityAudit(ctx context.Context, wake <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-timer.C:
		return true
	}
}
