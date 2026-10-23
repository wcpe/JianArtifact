package domain_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住 FR-159 领域层的两条硬性要求（规格 §5.1）：
//   - 探测**不得**改变断路器状态、不得产生 block 事件；
//   - 只探测 type=proxy 且 online=true 的仓库（hosted / group / offline 一律跳过）。
//
// 采集器用一个只累加内存的假实现承担：这些用例关心的是「有没有发出采样」与
// 「断路器有没有被动过」，不需要真实数据库（持久化的累加语义由 repository 层用例覆盖）。

// recordingUpstreamHealthRecorder 是 UpstreamHealthRecorder 的内存假实现。
type recordingUpstreamHealthRecorder struct {
	mu      sync.Mutex
	samples []repository.UpstreamHealthDelta
	err     error
}

func (r *recordingUpstreamHealthRecorder) AddSamples(delta repository.UpstreamHealthDelta) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.samples = append(r.samples, delta)
	return nil
}

// snapshot 返回目前已落盘（被 Flush 写出）的增量副本。
func (r *recordingUpstreamHealthRecorder) snapshot() []repository.UpstreamHealthDelta {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]repository.UpstreamHealthDelta(nil), r.samples...)
}

// countingBlockRecorder 统计断路器阻止事件（upstream_block_events 的来源）。
type countingBlockRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *countingBlockRecorder) UpstreamBlockEvent(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, reason)
}

func (r *countingBlockRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// 探测失败后：断路器 Status / BlockedUntil / 退避档位**均不变**，且不产生 block 事件。
// 这是 FR-159 最关键的一条约束——探测是观测，不是判定。
func TestUpstreamProbeDoesNotTouchCircuitBreaker(t *testing.T) {
	t.Parallel()
	// 上游永远返回 500：探测必然失败。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	recorder := &recordingUpstreamHealthRecorder{}
	healthSvc := domain.NewUpstreamHealthService(recorder)
	svc.SetUpstreamHealthService(healthSvc)
	blocks := &countingBlockRecorder{}
	svc.SetUpstreamBlockRecorder(blocks)

	repoID, err := repos.Create("probe-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 probe-proxy：%v", err)
	}

	// 先让仓库进入一个确定的状态：失败一次 → AUTO_BLOCKED + 退避窗口。
	svc.SetAutoBlockBase(time.Hour) // 窗口足够长，后台线程不会在用例期间自行推进
	svc.RecheckConnection(repoID, srv.URL)
	before := svc.Status(repoID)
	if before.Status != domain.StatusAutoBlocked {
		t.Fatalf("前置状态应 AUTO_BLOCKED，实际 %s", before.Status)
	}
	blockEventsBefore := blocks.count()

	// 跑一轮探测作业（此时上游仍返回 500 → 探测失败）。
	probeSvc := domain.NewUpstreamProbeService(repos, svc, healthSvc)
	probeSvc.SetProbeTimeout(2 * time.Second)
	result, err := probeSvc.Run(context.Background())
	if err != nil {
		t.Fatalf("探测作业：%v", err)
	}
	if result.Probed != 1 || result.Failed != 1 {
		t.Fatalf("应探测 1 个并失败 1 个，实际 %+v", result)
	}

	after := svc.Status(repoID)
	if after.Status != before.Status {
		t.Fatalf("探测不得改变状态：期望 %s，实际 %s", before.Status, after.Status)
	}
	if !after.BlockedUntil.Equal(before.BlockedUntil) {
		t.Fatalf("探测不得改变阻止窗口：期望 %v，实际 %v", before.BlockedUntil, after.BlockedUntil)
	}
	if after.BlockedFor != before.BlockedFor {
		t.Fatalf("探测不得改变退避档位：期望 %v，实际 %v", before.BlockedFor, after.BlockedFor)
	}
	if got := blocks.count(); got != blockEventsBefore {
		t.Fatalf("探测不得产生 block 事件：期望仍为 %d，实际 %d", blockEventsBefore, got)
	}

	// 但采样必须记下来：探测失败要落进 probe 序列（否则「历史」这件事就没了）。
	if err := healthSvc.Flush(); err != nil {
		t.Fatalf("落盘采样：%v", err)
	}
	samples := recorder.snapshot()
	if len(samples) != 1 {
		t.Fatalf("应有 1 条 probe 采样，实际 %d 条：%+v", len(samples), samples)
	}
	if samples[0].Source != repository.UpstreamHealthSourceProbe {
		t.Fatalf("采样来源应为 probe，实际 %s", samples[0].Source)
	}
	if samples[0].RepositoryID != repoID {
		t.Fatalf("采样仓库应为 %d，实际 %d", repoID, samples[0].RepositoryID)
	}
	// 上游 500 与既有 shouldBlockStatus 的判定同源 → unreachable 列。
	if samples[0].Unreachable != 1 || samples[0].Samples != 1 {
		t.Fatalf("500 应计入 unreachable 列：%+v", samples[0])
	}
}

// 探测成功（HEAD 200）时同样不改状态：一个 READY 仓库探测成功后仍是 READY，
// 不会被 recordSuccess 改成 AVAILABLE——「观测不得写状态」是双向的。
func TestUpstreamProbeSuccessDoesNotMarkAvailable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	recorder := &recordingUpstreamHealthRecorder{}
	healthSvc := domain.NewUpstreamHealthService(recorder)
	svc.SetUpstreamHealthService(healthSvc)
	blocks := &countingBlockRecorder{}
	svc.SetUpstreamBlockRecorder(blocks)

	repoID, err := repos.Create("healthy-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 healthy-proxy：%v", err)
	}
	probeSvc := domain.NewUpstreamProbeService(repos, svc, healthSvc)
	probeSvc.SetProbeTimeout(2 * time.Second)
	result, err := probeSvc.Run(context.Background())
	if err != nil {
		t.Fatalf("探测作业：%v", err)
	}
	if result.Probed != 1 || result.OK != 1 {
		t.Fatalf("应探测 1 个并成功 1 个，实际 %+v", result)
	}
	if got := svc.Status(repoID).Status; got != domain.StatusReady {
		t.Fatalf("探测不得把 READY 改写成 AVAILABLE，实际 %s", got)
	}
	if got := blocks.count(); got != 0 {
		t.Fatalf("成功探测不得产生 block 事件，实际 %d 条", got)
	}
	if err := healthSvc.Flush(); err != nil {
		t.Fatalf("落盘采样：%v", err)
	}
	samples := recorder.snapshot()
	if len(samples) != 1 || samples[0].OK != 1 || samples[0].Samples != 1 {
		t.Fatalf("成功探测应计入 ok 列：%+v", samples)
	}
}

// 只探测 type=proxy 且 online=true：hosted / group / offline 一律跳过。
func TestUpstreamProbeSkipsNonProbeableRepositories(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	recorder := &recordingUpstreamHealthRecorder{}
	healthSvc := domain.NewUpstreamHealthService(recorder)
	svc.SetUpstreamHealthService(healthSvc)

	if _, err := repos.Create("hosted-repo", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建 hosted 仓库：%v", err)
	}
	if _, err := repos.Create("group-repo", "raw", "group", "private", groupConfigJSON(t)); err != nil {
		t.Fatalf("建 group 仓库：%v", err)
	}
	onlineProxyID, err := repos.Create("online-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 online proxy 仓库：%v", err)
	}
	if _, err := repos.Create("offline-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 offline proxy 仓库：%v", err)
	}
	// 置为离线：offline 是用户明确要求停用，探测也不该碰它。
	if err := repos.SetOnline("offline-proxy", false); err != nil {
		t.Fatalf("下线 offline-proxy：%v", err)
	}

	probeSvc := domain.NewUpstreamProbeService(repos, svc, healthSvc)
	probeSvc.SetProbeTimeout(2 * time.Second)
	result, err := probeSvc.Run(context.Background())
	if err != nil {
		t.Fatalf("探测作业：%v", err)
	}
	if result.Probed != 1 {
		t.Fatalf("只应探测 1 个仓库（online 的 proxy），实际 %+v", result)
	}
	if err := healthSvc.Flush(); err != nil {
		t.Fatalf("落盘采样：%v", err)
	}
	samples := recorder.snapshot()
	if len(samples) != 1 {
		t.Fatalf("只应有 1 条采样，实际 %d 条：%+v", len(samples), samples)
	}
	if samples[0].RepositoryID != onlineProxyID {
		t.Fatalf("采样应属于 online proxy（%d），实际 %d", onlineProxyID, samples[0].RepositoryID)
	}
}

// 采样分类落列：slow_rate → slow、transfer_aborted → aborted、no_response → timeout、
// unreachable → unreachable、confirmed_missing（404/410）→ ok；
// local_write_failure 与 canceled **不计入任何列**（整条采样被丢弃）。
func TestUpstreamHealthCategoryMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		category  string
		wantOK    int64
		wantSlow  int64
		wantAbrt  int64
		wantTime  int64
		wantUnrch int64
		wantDrop  bool
	}{
		{name: "成功", category: "", wantOK: 1},
		{name: "慢传输计入 slow", category: "slow_rate", wantSlow: 1},
		{name: "传输中断计入 aborted", category: "transfer_aborted", wantAbrt: 1},
		{name: "没响应计入 timeout", category: "no_response", wantTime: 1},
		{name: "不可达计入 unreachable", category: "unreachable", wantUnrch: 1},
		// 404/410 证明上游可达：算成功，否则「连通率」会把「上游没这个文件」误报成上游坏了。
		{name: "确认缺失计入 ok", category: "confirmed_missing", wantOK: 1},
		// 这两类都不是上游健康信号，必须整条丢弃（连 samples 都不加）。
		{name: "本地写失败不计入", category: "local_write_failure", wantDrop: true},
		{name: "取消不计入", category: "canceled", wantDrop: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorder := &recordingUpstreamHealthRecorder{}
			healthSvc := domain.NewUpstreamHealthService(recorder)
			healthSvc.RecordRequest(1, tc.category, 120*time.Millisecond)
			if err := healthSvc.Flush(); err != nil {
				t.Fatalf("落盘采样：%v", err)
			}
			samples := recorder.snapshot()
			if tc.wantDrop {
				if len(samples) != 0 {
					t.Fatalf("该类别不得产生任何采样，实际 %+v", samples)
				}
				return
			}
			if len(samples) != 1 {
				t.Fatalf("应有 1 条采样，实际 %d 条", len(samples))
			}
			got := samples[0]
			if got.Samples != 1 {
				t.Fatalf("samples 应为 1，实际 %d", got.Samples)
			}
			if got.OK != tc.wantOK || got.Slow != tc.wantSlow || got.Aborted != tc.wantAbrt ||
				got.Timeout != tc.wantTime || got.Unreachable != tc.wantUnrch {
				t.Fatalf("落列错误：%+v（期望 ok=%d slow=%d aborted=%d timeout=%d unreachable=%d）",
					got, tc.wantOK, tc.wantSlow, tc.wantAbrt, tc.wantTime, tc.wantUnrch)
			}
			// 延迟只统计成功样本：失败样本不带延迟和（超时上限不该混进平均值）。
			if tc.wantOK > 0 && got.LatencySumMs != 120 {
				t.Fatalf("成功样本应记录延迟，实际 %d", got.LatencySumMs)
			}
			if tc.wantOK == 0 && got.LatencyCountMs != 0 {
				t.Fatalf("失败样本不应计入延迟统计，实际 %d", got.LatencyCountMs)
			}
		})
	}
}

// 同一分钟内的多次采样在进程内就被合并成一个桶，Flush 只写一次（批量写的前提）。
func TestUpstreamHealthServiceMergesSamplesBeforeFlush(t *testing.T) {
	t.Parallel()
	recorder := &recordingUpstreamHealthRecorder{}
	healthSvc := domain.NewUpstreamHealthService(recorder)
	for i := 0; i < 5; i++ {
		healthSvc.RecordRequest(7, "", 100*time.Millisecond)
	}
	healthSvc.RecordProbe(7, "", 10*time.Millisecond)

	// Flush 之前不应有任何写入（证明写库发生在批量刷盘，而不是每请求一次）。
	if len(recorder.snapshot()) != 0 {
		t.Fatalf("Flush 之前不应写库")
	}
	if err := healthSvc.Flush(); err != nil {
		t.Fatalf("落盘采样：%v", err)
	}
	samples := recorder.snapshot()
	if len(samples) != 2 {
		t.Fatalf("两路来源应各写一行（同分钟合并），实际 %d 条：%+v", len(samples), samples)
	}
	bySource := map[string]repository.UpstreamHealthDelta{}
	for _, delta := range samples {
		bySource[delta.Source] = delta
	}
	request := bySource[repository.UpstreamHealthSourceRequest]
	if request.Samples != 5 || request.OK != 5 || request.LatencySumMs != 500 {
		t.Fatalf("request 合并错误：%+v", request)
	}
	probe := bySource[repository.UpstreamHealthSourceProbe]
	if probe.Samples != 1 || probe.OK != 1 {
		t.Fatalf("probe 合并错误：%+v", probe)
	}
}

// 未接线（nil 服务）时所有记录都是空操作，不 panic。
func TestUpstreamHealthServiceDisabledIsNoop(t *testing.T) {
	t.Parallel()
	var nilService *domain.UpstreamHealthService
	nilService.RecordRequest(1, "", time.Second)
	nilService.RecordProbe(1, "", time.Second)
	if err := nilService.Flush(); err != nil {
		t.Fatalf("未接线时 Flush 应为空操作，实际 %v", err)
	}
	if nilService.Enabled() {
		t.Fatalf("未接线时 Enabled 应为 false")
	}
	// recorder 为 nil 构造出的服务同样是空操作。
	disabled := domain.NewUpstreamHealthService(nil)
	if disabled.Enabled() {
		t.Fatalf("recorder 为 nil 时 Enabled 应为 false")
	}
	disabled.RecordRequest(1, "", time.Second)
}

// 刷盘失败时把该批并回 pending，下一轮重试成功后才真正落盘（不丢样本）。
func TestUpstreamHealthFlushRetriesAfterFailure(t *testing.T) {
	t.Parallel()
	recorder := &recordingUpstreamHealthRecorder{err: context.DeadlineExceeded}
	healthSvc := domain.NewUpstreamHealthService(recorder)
	healthSvc.RecordRequest(11, "", 50*time.Millisecond)
	if err := healthSvc.Flush(); err == nil {
		t.Fatalf("注入失败时应返回错误")
	}
	// 恢复后重试：样本必须还在。
	recorder.mu.Lock()
	recorder.err = nil
	recorder.mu.Unlock()
	if err := healthSvc.Flush(); err != nil {
		t.Fatalf("重试落盘：%v", err)
	}
	samples := recorder.snapshot()
	if len(samples) != 1 || samples[0].Samples != 1 || samples[0].RepositoryID != 11 {
		t.Fatalf("失败后样本应保留并在下一轮写入，实际 %+v", samples)
	}
}

// 关停（ctx 取消）时必须把 pending 落盘——否则进程退出前最后一段采样会丢。
func TestUpstreamHealthStartFlushesOnShutdown(t *testing.T) {
	t.Parallel()
	recorder := &recordingUpstreamHealthRecorder{}
	healthSvc := domain.NewUpstreamHealthService(recorder)
	ctx, cancel := context.WithCancel(context.Background())
	healthSvc.Start(ctx)
	healthSvc.RecordRequest(21, "", 30*time.Millisecond)
	cancel()

	// 等待关停分支完成落盘（不睡固定时长：轮询到出现样本为止）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(recorder.snapshot()) == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("关停时未落盘 pending 采样")
}
