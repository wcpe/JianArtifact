package domain

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件实现上游健康历史的领域层：
//   - UpstreamHealthRecorder 接口（领域定义、装配注入、nil 表示不记录，对齐 RejectionRecorder 范式）；
//   - UpstreamHealthService：把真实请求与后台探测的采样在进程内小批量累积后刷盘。
//
// 与「当前连接状态」的分工：那里回答「此刻上游通不通」（六态内存态），这里回答
// 「过去一段时间通不通、失败分别是什么原因」（持久化时间序列）。两者不互相替代。
//
// 与「回源失败原因五分类」的分工：那里判定失败属于哪一类，这里**只消费**它的结论，绝不重新判一次——
// 两处判定必然漂移（见 docs/specs/0.13.0-upstream-health-history.md）。

// UpstreamHealthRecorder 把一批采样增量累加进分钟桶，供持久化实现注入。
// 由装配层注入 *repository.UpstreamHealthRepo；nil 表示不记录（测试与未接线部署）。
type UpstreamHealthRecorder interface {
	AddSamples(delta repository.UpstreamHealthDelta) error
}

// upstreamHealthFlushInterval 是进程内累积的刷盘间隔。
//
// 为什么需要批量：采样发生在**回源主路径**上，每次请求都同步写一次 SQLite 会让
// 回源耗时多出一次 B-Tree 维护 + WAL 追加，并在高频回源下与资产落盘事务争抢单写者。
// 批量把写次数从「每请求一次」降到「每 5 秒一次」，同时用 ON CONFLICT 累加保证
// 合并后的语义与逐条写完全一致（不是近似）。
const upstreamHealthFlushInterval = 5 * time.Second

// upstreamHealthMaxPending 是进程内待刷盘桶数的上限。
//
// 存在的意义只有一个：数据库长时间不可用时，pending 会随「分钟桶 × 仓库」增长。
// 达到上限后丢弃新增采样并记一条日志——采样是观测数据，宁可丢观测也不能撑爆内存
// 或拖慢回源。上限取得足够大（正常情况下也就「仓库数 × 几分钟」个桶）。
const upstreamHealthMaxPending = 4096

// UpstreamHealthService 负责上游健康采样在进程内的累积与定期落盘。
//
// 生命周期与其它采样服务一致（见 OperationsDashboardService.Start）：
// Start(ctx) 起后台 goroutine，ctx 结束前做最后一次 flush——**关闭时必须 flush**，
// 否则最后一段（最多 5 秒）采样会丢（见 docs/specs/0.13.0-upstream-health-history.md）。
type UpstreamHealthService struct {
	recorder UpstreamHealthRecorder

	mu      sync.Mutex
	pending map[upstreamHealthKey]repository.UpstreamHealthDelta
	// now 可注入的时钟，测试用来确定性地断言桶归属；生产为 time.Now。
	now func() time.Time
}

// upstreamHealthKey 是进程内累积的合并键，与表主键一一对应（三条都不可省）。
type upstreamHealthKey struct {
	bucketStart  string
	repositoryID int64
	source       string
}

// NewUpstreamHealthService 构造上游健康采样服务。recorder 为 nil 时全部写入都是空操作。
func NewUpstreamHealthService(recorder UpstreamHealthRecorder) *UpstreamHealthService {
	return &UpstreamHealthService{
		recorder: recorder,
		pending:  map[upstreamHealthKey]repository.UpstreamHealthDelta{},
		now:      time.Now,
	}
}

// Enabled 报告采样是否已接线（未接线时调用方可以完全跳过采样构造，省掉无用的时间计算）。
func (s *UpstreamHealthService) Enabled() bool { return s != nil && s.recorder != nil }

// upstreamHealthCategoryToColumn 把回源失败的可观测类别映射到分钟桶的统计列。
//
// 这是**唯一的映射点**：采样写入不做第二套判定，只在分类结果上落列。
// 返回值 collected=false 表示该类别**不进入任何列**：
//   - local_write_failure：本地磁盘/权限故障，与上游无关；
//   - canceled：客户端断连 / 同组其它成员已成功 / 优雅停机，是正常的请求生命周期终点。
//
// confirmed_missing（上游明确 404/410）计入 ok：上游**可达且健康地答复了**「没有这个制品」，
// 它证明的正是连通性。见 docs/specs/0.13.0-upstream-health-history.md 的列清单里没有 confirmed_missing 专属列，
// 而把它算作失败会让「连通率」把「上游好好的，只是没这个文件」误报成上游故障——
// 因此这里归到 ok，并保持「ok + 四类失败 = samples」这条让连通率可算的不变式。
func upstreamHealthCategoryToColumn(category string) (ok bool, failure string, collected bool) {
	switch category {
	case "":
		return true, "", true
	case upstreamCategoryConfirmedMissing:
		return true, "", true
	case upstreamCategoryNoResponse:
		return false, repository.UpstreamHealthFailureTimeout, true
	case upstreamCategoryUnreachable:
		return false, repository.UpstreamHealthFailureUnreachable, true
	case upstreamCategorySlowRate:
		return false, repository.UpstreamHealthFailureSlow, true
	case upstreamCategoryTransferAborted:
		return false, repository.UpstreamHealthFailureAborted, true
	case upstreamCategoryLocalWriteFailure, upstreamCategoryCanceled:
		return false, "", false
	default:
		// 未知类别按不可达计入：宁可多报一次上游故障，也不要让样本悄悄消失
		// （消失会让 samples 与失败列之和对不上，连通率随之失真）。
		return false, repository.UpstreamHealthFailureUnreachable, true
	}
}

// recordUpstreamSample 把一次采样并入进程内待刷盘桶。
// 未接线时是空操作；这也是「nil 不记录」范式的落点。
func (s *UpstreamHealthService) recordUpstreamSample(repoID int64, source, category string, latency time.Duration) {
	if !s.Enabled() || repoID <= 0 {
		return
	}
	ok, failure, collected := upstreamHealthCategoryToColumn(category)
	if !collected {
		// local_write_failure / canceled：既不是上游故障也不是正常成功，整条采样丢弃。
		return
	}
	bucket := repository.UpstreamHealthBucketStart(s.now())
	delta := repository.UpstreamHealthDelta{
		BucketStart: bucket, RepositoryID: repoID, Source: source, Samples: 1,
	}
	if ok {
		delta.OK = 1
		// 延迟只统计成功样本：失败请求的耗时由超时上限决定，混进平均值会把
		// 「上游很快但资源不存在」与「上游很慢以至于超时」压成同一个数字。
		ms := latency.Milliseconds()
		if ms < 0 {
			ms = 0
		}
		delta.LatencySumMs = ms
		delta.LatencyCountMs = 1
		delta.MaxLatencyMs = ms
	} else {
		switch failure {
		case repository.UpstreamHealthFailureTimeout:
			delta.Timeout = 1
		case repository.UpstreamHealthFailureUnreachable:
			delta.Unreachable = 1
		case repository.UpstreamHealthFailureSlow:
			delta.Slow = 1
		case repository.UpstreamHealthFailureAborted:
			delta.Aborted = 1
		}
	}
	key := upstreamHealthKey{bucketStart: bucket, repositoryID: repoID, source: source}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, found := s.pending[key]; found {
		existing.AddSamples(delta)
		s.pending[key] = existing
		return
	}
	if len(s.pending) >= upstreamHealthMaxPending {
		// 库长期不可用：丢弃这一条并说明为什么，避免把内存与回源路径一起拖下水。
		log.Printf("上游健康采样：待刷盘桶数已达上限 %d，丢弃本次采样（仓库 %d）", upstreamHealthMaxPending, repoID)
		return
	}
	s.pending[key] = delta
}

// RecordRequest 记录一次**真实回源请求**的采样结果，供回源路径调用。
// category 直接取五分类判定结果的 category（失败时）或空串（成功时）。
// 本方法只做内存合并，不碰磁盘，因此可以安全地放在回源结束后的收尾位置。
func (s *UpstreamHealthService) RecordRequest(repoID int64, category string, latency time.Duration) {
	s.recordUpstreamSample(repoID, repository.UpstreamHealthSourceRequest, category, latency)
}

// RecordProbe 记录一次**后台探测**的采样结果。与真实请求分别成序列，互不稀释。
func (s *UpstreamHealthService) RecordProbe(repoID int64, category string, latency time.Duration) {
	s.recordUpstreamSample(repoID, repository.UpstreamHealthSourceProbe, category, latency)
}

// Flush 把当前累积的桶原子化地落盘：取出快照后清空 pending，再逐桶累加写入。
// 写入失败时把**尚未写成功的部分**并回 pending，等下一轮重试（不丢样本）。
func (s *UpstreamHealthService) Flush() error {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return nil
	}
	items := make([]repository.UpstreamHealthDelta, 0, len(s.pending))
	for key, delta := range s.pending {
		items = append(items, delta)
		delete(s.pending, key)
	}
	s.mu.Unlock()

	var firstErr error
	for index, delta := range items {
		if err := s.recorder.AddSamples(delta); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// 本条与后续都还没写成功：并回 pending 等待下一轮。
			s.mu.Lock()
			for _, remaining := range items[index:] {
				key := upstreamHealthKey{
					bucketStart:  remaining.BucketStart,
					repositoryID: remaining.RepositoryID,
					source:       remaining.Source,
				}
				if existing, found := s.pending[key]; found {
					existing.AddSamples(remaining)
					s.pending[key] = existing
					continue
				}
				s.pending[key] = remaining
			}
			s.mu.Unlock()
			break
		}
	}
	return firstErr
}

// Start 启动定期刷盘的后台循环，随 ctx 取消优雅停止。
//
// ctx 结束后**必须再 flush 一次**：否则进程关停时 pending 里最多 5 秒的采样会丢，
// 而这正是「重启后历史仍在」这一验收点最容易漏掉的一段（见 docs/specs/0.13.0-upstream-health-history.md）。
func (s *UpstreamHealthService) Start(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	go func() {
		ticker := time.NewTicker(upstreamHealthFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// 关停前最后一次落盘：用独立的上下文预算，避免沿用已取消的 ctx 而立刻失败。
				if err := s.Flush(); err != nil {
					log.Printf("上游健康采样：关停前落盘失败，可能丢失少量采样：%v", err)
				}
				return
			case <-ticker.C:
				if err := s.Flush(); err != nil {
					// 刷盘失败不影响回源：样本会在下一轮重试（见 Flush 的并回逻辑）。
					log.Printf("上游健康采样：定期落盘失败，将在下一轮重试：%v", err)
				}
			}
		}
	}()
}

// upstreamProbeFailCategory 把一次探测失败归类为回源失败的可观测类别。
//
// 复用既有判定而不是另造一套：
//   - 拿到 status 但与既有 shouldBlockStatus 一致地视为失败（5xx / 401 / 403）→ unreachable，
//     与 probeUpstream 的判定同源，避免「探测说成功、回源判定不可达」两套口径；
//   - 请求被取消（进程关停、上游探测被中断）→ canceled，不进入任何列；
//   - 其余错误交给 classifyUpstreamFailure（与回源链路共用同一实现）。
//
// 返回的 ok 表示探测在「上游是否可达」意义上是否成功：404/410 属于**上游可达**
// （只是没这个资源），与 probeUpstream 的既有语义一致（shouldBlockStatus 对其返回 false）。
func upstreamProbeFailCategory(status int, err error) (category string, reachable bool) {
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return upstreamCategoryCanceled, false
		}
		verdict := classifyUpstreamFailure(err)
		// 探测拿到 404/410 时 ProbeWithCredential 不会返回错误（它把状态码交回调用方），
		// 因此这里的 confirmed_missing 只可能来自真实错误；仍按「可达」处理以与
		// shouldBlockStatus 的口径一致。
		if verdict.category == upstreamCategoryConfirmedMissing {
			return verdict.category, true
		}
		return verdict.category, false
	}
	if shouldBlockStatus(status) {
		return upstreamCategoryUnreachable, false
	}
	return "", true
}
