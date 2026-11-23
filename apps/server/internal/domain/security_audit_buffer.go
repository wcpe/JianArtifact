package domain

import (
	"math"
	"net/netip"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

const (
	securityAuditGlobalRate  = 100
	securityAuditSourceRate  = 10
	securityAuditSourceLimit = 4096
	securityAuditQueueLimit  = 1024
	securityAuditQueueBytes  = 2 * 1024 * 1024
)

type securityAuditBuffer struct {
	entries      [securityAuditQueueLimit]*securityAuditQueued
	head, size   int
	count, bytes int
	global       [securityAuditGlobalRate]int64
	globalUsed   int
	globalNext   int
	sources      map[netip.Addr]*securityAuditSourceBudget
	anchor       time.Time
	lastTick     int64
	expiresNext  int64
}

type securityAuditSourceBudget struct {
	times      [securityAuditSourceRate]int64
	used, next int
}

type securityAuditQueued struct {
	observation repository.SecurityAuditObservation
	bytes       int
}

func (q *securityAuditBuffer) offer(o repository.SecurityAuditObservation, ip netip.Addr, now time.Time, size int) string {
	if q.count >= securityAuditQueueLimit || size > securityAuditQueueBytes-q.bytes {
		return "queue_full"
	}
	if q.anchor.IsZero() {
		q.anchor = now
	}
	// 运行时使用单调时间；没有单调时间的输入也不因回拨新增预算。
	q.lastTick = max(q.lastTick, int64(now.Sub(q.anchor)))
	at := q.lastTick
	if q.globalUsed == len(q.global) && at-q.global[q.globalNext] < int64(time.Second) {
		return "global_rate"
	}
	bucket := q.sources[ip]
	if bucket == nil {
		if len(q.sources) >= securityAuditSourceLimit && at >= q.expiresNext {
			q.expireSources(at)
		}
		if len(q.sources) >= securityAuditSourceLimit {
			return "source_table_full"
		}
		if q.sources == nil {
			q.sources = make(map[netip.Addr]*securityAuditSourceBudget)
		}
		bucket = &securityAuditSourceBudget{}
		q.sources[ip] = bucket
	} else if bucket.used == len(bucket.times) && at-bucket.times[bucket.next] < int64(time.Minute) {
		return "source_rate"
	}
	q.global[q.globalNext] = at
	q.globalNext = (q.globalNext + 1) % len(q.global)
	q.globalUsed = min(q.globalUsed+1, len(q.global))
	bucket.times[bucket.next] = at
	bucket.next = (bucket.next + 1) % len(bucket.times)
	bucket.used = min(bucket.used+1, len(bucket.times))
	q.entries[(q.head+q.size)%len(q.entries)] = &securityAuditQueued{observation: o, bytes: size}
	q.size++
	q.count++
	q.bytes += size
	return ""
}

func (q *securityAuditBuffer) expireSources(at int64) {
	q.expiresNext = math.MaxInt64
	for ip, bucket := range q.sources {
		last := bucket.times[(bucket.next+len(bucket.times)-1)%len(bucket.times)]
		expires := last + int64(time.Minute)
		if at >= expires {
			delete(q.sources, ip)
		} else {
			q.expiresNext = min(q.expiresNext, expires)
		}
	}
	if len(q.sources) == 0 {
		q.expiresNext = 0
	}
}

func (q *securityAuditBuffer) take(limit int) []*securityAuditQueued {
	if q.size == 0 {
		return nil
	}
	items := make([]*securityAuditQueued, min(q.size, limit))
	for i := range items {
		items[i], q.entries[q.head] = q.entries[q.head], nil
		q.head = (q.head + 1) % len(q.entries)
		q.size--
	}
	return items
}

func (q *securityAuditBuffer) release(items []*securityAuditQueued) {
	for _, item := range items {
		q.count--
		q.bytes -= item.bytes
	}
}

const securityAuditGapReasons = 6

type securityAuditGapReason string

var securityAuditReasons = [...]securityAuditGapReason{"global_rate", "source_rate", "source_table_full", "queue_full", "database_unavailable", "invalid_fact"}

type securityAuditGapMinute struct {
	start  time.Time
	counts [securityAuditGapReasons]int64
}

type securityAuditGaps struct {
	minutes    [repository.SecurityAuditGapWindowLimit]securityAuditGapMinute
	head, size int
	overflow   repository.SecurityAuditGap
}

func (g *securityAuditGaps) add(at time.Time, reason string) {
	start := at.UTC().Truncate(time.Minute)
	if g.size == 0 || g.minutes[(g.head+g.size-1)%len(g.minutes)].start.Before(start) {
		if g.size == len(g.minutes) {
			old := g.minutes[g.head].start
			if g.overflow.Start.IsZero() {
				g.overflow = repository.SecurityAuditGap{Start: old, Reason: "counter_overflow", Coverage: "unknown"}
			}
			g.overflow.End = old.Add(time.Minute)
			g.advance()
		}
		g.minutes[(g.head+g.size)%len(g.minutes)] = securityAuditGapMinute{start: start}
		g.size++
	}
	minute := &g.minutes[(g.head+g.size-1)%len(g.minutes)]
	for i, allowed := range securityAuditReasons {
		if reason == string(allowed) && minute.counts[i] < math.MaxInt64 {
			minute.counts[i]++
			return
		}
	}
}

func (g *securityAuditGaps) advance() {
	g.minutes[g.head] = securityAuditGapMinute{}
	g.head = (g.head + 1) % len(g.minutes)
	g.size--
}

func (g *securityAuditGaps) take(limit int) []repository.SecurityAuditGap {
	var result []repository.SecurityAuditGap
	if limit > 0 && !g.overflow.Start.IsZero() {
		result = append(result, g.overflow)
		g.overflow = repository.SecurityAuditGap{}
	}
	for g.size > 0 && len(result) < limit {
		minute := &g.minutes[g.head]
		for i, reason := range securityAuditReasons {
			count := minute.counts[i]
			if count == 0 {
				continue
			}
			if len(result) == limit {
				return result
			}
			result = append(result, repository.SecurityAuditGap{Start: minute.start, End: minute.start.Add(time.Minute),
				Reason: string(reason), Coverage: "gapped", ObservedCount: &count, SuppressedCount: &count})
			minute.counts[i] = 0
		}
		g.advance()
	}
	return result
}

func (g *securityAuditGaps) oldest() time.Time {
	if !g.overflow.Start.IsZero() {
		return g.overflow.Start
	}
	if g.size > 0 {
		return g.minutes[g.head].start
	}
	return time.Time{}
}
