package domain

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件实现 FR-159 的后台定时探测作业（调度器作业名 upstream-probe）。
//
// 两条不可动摇的约束（见 docs/specs/0.13.0-upstream-health-history.md）：
//  1. **探测不得影响断路器状态机**：不调 recordFailure / recordSuccess，因此既不改
//     Status / BlockedUntil / 退避档位，也不产生 upstream_block_events。它是观测，不是判定。
//  2. **探测必须走既有探测链路**（ProbeWithCredential，与 recheck / 后台半开探测同源），
//     不新造 HTTP 路径——否则会出现「探测说健康、回源却失败」的第二套口径。

// upstreamProbeConcurrency 是单轮探测的并发上限。
//
// 为什么需要上限：一次探测就是一个到上游的 TCP + HEAD，实例上有几十个 proxy 仓库时
// 不设限会让每轮同时打开几十条连接，既打扰上游也可能打满本地 fd。
// 8 是一个保守值：一分钟一轮、单次探测超时 5s 时，8 并发足以在一轮内探完 96 个仓库。
const upstreamProbeConcurrency = 8

// UpstreamProbeService 是 upstream-probe 调度作业的领域实现。
type UpstreamProbeService struct {
	repos  *repository.RepoRepo
	assets *AssetService
	health *UpstreamHealthService
	// probeTimeout 覆盖单次探测的超时；<= 0 时回落到 proxy_health.go 的 probeTimeout，
	// 使「定时探测」与「半开探测 / 手动重测」共用同一时限口径。测试用它缩短等待。
	probeTimeout time.Duration
	// concurrency 覆盖并发上限；<= 0 时取 upstreamProbeConcurrency。测试用它断言串行语义。
	concurrency int
}

// NewUpstreamProbeService 构造定时探测服务。
func NewUpstreamProbeService(repos *repository.RepoRepo, assets *AssetService, health *UpstreamHealthService) *UpstreamProbeService {
	return &UpstreamProbeService{repos: repos, assets: assets, health: health}
}

// SetProbeTimeout 调整单次探测超时（测试与调优用；<= 0 忽略）。
func (s *UpstreamProbeService) SetProbeTimeout(d time.Duration) {
	if s == nil || d <= 0 {
		return
	}
	s.probeTimeout = d
}

// SetConcurrency 调整单轮并发上限（测试与调优用；<= 0 忽略）。
func (s *UpstreamProbeService) SetConcurrency(n int) {
	if s == nil || n <= 0 {
		return
	}
	s.concurrency = n
}

// UpstreamProbeResult 汇总一轮探测的计数，供作业日志与测试断言。
type UpstreamProbeResult struct {
	Probed  int // 实际发出探测的仓库数
	OK      int // 上游可达（含 404 这类「可达但无此资源」）
	Failed  int // 上游不可达 / 没响应 / 失败
	Skipped int // 因未接线或配置无法探测而跳过的仓库数
}

// Run 执行一轮探测：列出 type=proxy 且 online=true 的仓库，逐个探测并写入 probe 序列。
//
// 单仓库失败**不影响**其它仓库：错误只被计入结果与日志，不会中断整轮，
// 整体也不返回错误——探测失败是正常的观测结果，不该让调度器把它记成「作业失败」。
func (s *UpstreamProbeService) Run(ctx context.Context) (UpstreamProbeResult, error) {
	var result UpstreamProbeResult
	if s == nil || s.repos == nil || s.assets == nil {
		return result, nil
	}
	// 未接线采样服务时整轮无意义（探了也没地方记），直接跳过，不打扰上游。
	if !s.health.Enabled() {
		return result, nil
	}
	// 只取 proxy 且 online：hosted 没有上游、group 没有独立上游、offline 是用户明确要求停用。
	// 条件放在 SQL 里而不是 Go 侧过滤，理由见 ListByTypeAndOnline 的注释。
	repos, err := s.repos.ListByTypeAndOnline("proxy", true)
	if err != nil {
		return result, err
	}
	if len(repos) == 0 {
		return result, nil
	}

	limit := s.concurrency
	if limit <= 0 {
		limit = upstreamProbeConcurrency
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for index := range repos {
		repo := repos[index]
		if ctx.Err() != nil {
			// 进程正在关停：不再发起新探测（在途的会因 ctx 取消而被归为 canceled，不记录）。
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			ok, probed := s.probeOne(ctx, &repo)
			mu.Lock()
			defer mu.Unlock()
			if !probed {
				result.Skipped++
				return
			}
			result.Probed++
			if ok {
				result.OK++
				return
			}
			result.Failed++
		}()
	}
	wg.Wait()
	if result.Probed > 0 || result.Skipped > 0 {
		log.Printf("上游健康探测：本轮探测 %d 个 proxy 仓库（可达 %d、失败 %d、跳过 %d）",
			result.Probed, result.OK, result.Failed, result.Skipped)
	}
	return result, nil
}

// probeOne 探测单个仓库并把结果写入 probe 序列。返回 ok 与是否真的发出了探测。
//
// 注意这里**只有** s.assets.ProbeOnly 与 s.health.RecordProbe 两个调用：
// 既没有 recordFailure / recordSuccess，也没有 recordBlockEvent——
// 「探测不改断路器状态」在实现上就是「这几行代码根本不存在」。
func (s *UpstreamProbeService) probeOne(ctx context.Context, repo *repository.Repository) (ok bool, probed bool) {
	cfg, err := repo.DecodeConfig()
	if err != nil || cfg.RemoteURL == "" {
		// 配置不可用（历史脏数据）：跳过，不记采样——记成失败会把配置问题误报成上游故障。
		return false, false
	}
	timeout := s.probeTimeout
	if timeout <= 0 {
		timeout = probeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	status, probeErr := s.assets.ProbeOnly(probeCtx, cfg.RemoteURL, cfg.CredentialRef)
	category, reachable := upstreamProbeFailCategory(status, probeErr)
	if probeErr != nil && errors.Is(probeErr, context.Canceled) {
		// 进程关停导致的取消：不记录（采样服务也会丢弃 canceled 类别，这里提前返回省一次计算）。
		return false, false
	}
	s.health.RecordProbe(repo.ID, category, time.Since(started))
	if probeErr != nil && ctx.Err() == nil {
		// 单个仓库探测失败只记日志，不影响其它仓库，也不让整轮变成「作业失败」。
		// 日志里只出现仓库名与类别，不含上游地址与凭据。
		log.Printf("上游健康探测：仓库 %s 探测失败（类别 %s）", repo.Name, category)
	}
	return reachable, true
}
