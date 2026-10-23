package domain_test

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// 本文件覆盖 FR-157 的可观测契约：解析失败结论指标、uncertain 诊断行（含脱敏）、
// 负缓存容量上限与查看/清理能力。
//
// 组织约定：用例分两类。
//   - 纯数据断言（指标计数、负缓存快照/淘汰）可以 t.Parallel()；
//   - 抓日志的用例**不得**并行：log.SetOutput 是进程级写入。Go 会等全部串行用例
//     结束后才启动并行用例，因此串行抓日志不会与并行用例的输出互相污染
//     （约定见 internal/httpserver/parallelism_test.go）。

// failureCounter 是 ResolveFailureRecorder 的计数桩：按结论分区累计调用次数。
// 它刻意统计「调用次数」而不只是结果，才能钉住「一次请求只计一次」。
type failureCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newFailureCounter() *failureCounter {
	return &failureCounter{counts: map[string]int{}}
}

func (f *failureCounter) ResolveFailure(resolution string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[resolution]++
}

func (f *failureCounter) get(resolution string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[resolution]
}

// total 返回全部结论的累计次数，用于断言「没有多余计数」。
func (f *failureCounter) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	sum := 0
	for _, n := range f.counts {
		sum += n
	}
	return sum
}

// captureLog 把标准日志重定向到缓冲区；返回读取当前累积输出的函数。
// 调用方用例必须串行（见文件头约定）。
func captureLog(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	previous := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(previous) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// writerFunc 适配 log.SetOutput 需要的 io.Writer。
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// newCountingUpstream 起一个按状态码应答的上游，返回服务器与命中计数。
func newCountingUpstream(t *testing.T, status int) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		if status == http.StatusNotFound {
			http.NotFound(w, nil)
			return
		}
		http.Error(w, "boom", status)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// --- 指标：结论分区与采样点唯一 ---

// 上游确认 404 的缺失路径 → 结论 confirmed、指标 confirmed 加一、且**不**产生诊断行。
func TestResolveFailureMetricConfirmedWithoutDiagnostic(t *testing.T) {
	diagOutput := captureLog(t)
	srv, _ := newCountingUpstream(t, http.StatusNotFound)

	svc, repos := newAssetService(t)
	counter := newFailureCounter()
	svc.SetResolveFailureRecorder(counter)
	if _, err := repos.Create("obs-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "obs-proxy", "missing.jar"); err != domain.ErrNotFound {
		t.Fatalf("上游 404 应返回 ErrNotFound，实际：%v", err)
	}
	if got := counter.get("confirmed"); got != 1 {
		t.Fatalf("confirmed 应计 1 次，实际 %d（全部计数 %d）", got, counter.total())
	}
	if got := counter.get("uncertain"); got != 0 {
		t.Fatalf("confirmed 路径不应产生 uncertain 计数，实际 %d", got)
	}
	// confirmed 是正常 404 流量：逐条记会刷屏，因此不得输出诊断行。
	if logs := diagOutput(); strings.Contains(logs, "代理解析不可判定") {
		t.Fatalf("confirmed 不应输出诊断行，实际日志：%s", logs)
	}
}

// group 多成员同时失败时，指标只加一次（钉住采样点唯一：不在成员层与递归层累加）。
func TestResolveFailureMetricCountsGroupOnce(t *testing.T) {
	t.Parallel()
	srvA, _ := newCountingUpstream(t, http.StatusNotFound)
	srvB, _ := newCountingUpstream(t, http.StatusNotFound)

	svc, repos := newAssetService(t)
	counter := newFailureCounter()
	svc.SetResolveFailureRecorder(counter)
	if _, err := repos.Create("once-a", "raw", "proxy", "private", proxyConfigJSON(t, srvA.URL)); err != nil {
		t.Fatalf("建成员 A：%v", err)
	}
	if _, err := repos.Create("once-b", "raw", "proxy", "private", proxyConfigJSON(t, srvB.URL)); err != nil {
		t.Fatalf("建成员 B：%v", err)
	}
	if _, err := repos.Create("once-group", "raw", "group", "private", groupConfigJSON(t, "once-a", "once-b")); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "once-group", "dup.jar"); err != domain.ErrNotFound {
		t.Fatalf("两成员均 404 应返回 ErrNotFound，实际：%v", err)
	}
	// 两个成员各自都记过 confirmed，但顶层出口只结算一次 → 只有 1。
	if got := counter.get("confirmed"); got != 1 {
		t.Fatalf("一次 group 请求应只计 1 次 confirmed，实际 %d", got)
	}
	if got := counter.total(); got != 1 {
		t.Fatalf("一次 group 请求应只有 1 次总计数，实际 %d", got)
	}
}

// 未经协议层挂 recorder 的调用路径（直接 ResolveWithRepo）也必须能计数：
// 钉住「顶层出口自挂载」，否则这些路径读回 Unknown 会系统性漏计。
func TestResolveFailureMetricWithoutProtocolRecorder(t *testing.T) {
	t.Parallel()
	srv, _ := newCountingUpstream(t, http.StatusNotFound)

	svc, repos := newAssetService(t)
	counter := newFailureCounter()
	svc.SetResolveFailureRecorder(counter)
	if _, err := repos.Create("self-mount", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}
	repo, err := repos.GetByName("self-mount")
	if err != nil {
		t.Fatalf("取仓库：%v", err)
	}

	// 注意：这里**没有** WithResolveFailure，模拟除 maven 之外的直接调用点。
	if _, _, err := svc.ResolveWithRepo(context.Background(), repo, "missing.jar"); err != domain.ErrNotFound {
		t.Fatalf("上游 404 应返回 ErrNotFound，实际：%v", err)
	}
	if got := counter.get("confirmed"); got != 1 {
		t.Fatalf("顶层出口应自挂 recorder 并计 1 次，实际 %d", got)
	}
}

// 多成员同时不可判定（上游 5xx）时 uncertain 只加一次，并输出一条成员明细齐全的诊断行。
func TestResolveFailureMetricCountsUncertainOnce(t *testing.T) {
	diagOutput := captureLog(t)
	srvA, _ := newCountingUpstream(t, http.StatusInternalServerError)
	srvB, _ := newCountingUpstream(t, http.StatusInternalServerError)

	svc, repos := newAssetService(t)
	counter := newFailureCounter()
	svc.SetResolveFailureRecorder(counter)
	if _, err := repos.Create("unc-a", "raw", "proxy", "private", proxyConfigJSON(t, srvA.URL)); err != nil {
		t.Fatalf("建成员 A：%v", err)
	}
	if _, err := repos.Create("unc-b", "raw", "proxy", "private", proxyConfigJSON(t, srvB.URL)); err != nil {
		t.Fatalf("建成员 B：%v", err)
	}
	if _, err := repos.Create("unc-group", "raw", "group", "private", groupConfigJSON(t, "unc-a", "unc-b")); err != nil {
		t.Fatalf("建 group：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "unc-group", "boom.jar"); !errorsIsUpstream(err) {
		t.Fatalf("两成员均 5xx 应为上游失败，实际：%v", err)
	}
	if got := counter.get("uncertain"); got != 1 {
		t.Fatalf("一次 group 请求应只计 1 次 uncertain，实际 %d", got)
	}
	if got := counter.get("confirmed"); got != 0 {
		t.Fatalf("不可判定不应计 confirmed，实际 %d", got)
	}

	logs := diagOutput()
	lines := diagnosticLines(logs)
	if len(lines) != 1 {
		t.Fatalf("一次请求应只有一条诊断行，实际 %d 行：\n%s", len(lines), logs)
	}
	line := lines[0]
	for _, want := range []string{
		"WARN 代理解析不可判定",
		"repo=unc-group",
		"path=boom.jar",
		"resolution=uncertain",
		"member=unc-a:upstream_error",
		"member=unc-b:upstream_error",
		"upstream_host=" + srvA.URL,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("诊断行缺少 %q：%s", want, line)
		}
	}
	// 不得出现误导性表述：这条结论是「不可判定」，不是「确认不存在」。
	if strings.Contains(line, "confirmed") {
		t.Fatalf("uncertain 诊断行不得出现 confirmed 字样：%s", line)
	}
}

// diagnosticLines 取出日志中所有诊断行（按行切分，空行丢弃）。
func diagnosticLines(logs string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "代理解析不可判定") {
			out = append(out, line)
		}
	}
	return out
}

// --- 诊断行脱敏（NFR-06）---

// 控制字符必须被转义：路径来自客户端，未转义的换行会伪造出一条诊断行（日志注入）。
func TestResolveDiagnosticEscapesControlCharacters(t *testing.T) {
	diagOutput := captureLog(t)
	srv, _ := newCountingUpstream(t, http.StatusInternalServerError)

	svc, repos := newAssetService(t)
	if _, err := repos.Create("inject-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	injected := "evil.jar\nWARN 代理解析不可判定 repo=伪造 resolution=confirmed\t\x01"
	if _, _, err := svc.Resolve(context.Background(), "inject-proxy", injected); err == nil {
		t.Fatalf("上游 5xx 应失败")
	}
	lines := diagnosticLines(diagOutput())
	if len(lines) != 1 {
		t.Fatalf("注入的换行不得产生第二条诊断行，实际 %d 行", len(lines))
	}
	if !strings.Contains(lines[0], `path=evil.jar\nWARN`) {
		t.Fatalf("路径中的换行应以字面 \\n 转义，实际：%s", lines[0])
	}
	if strings.ContainsRune(lines[0], '\t') || strings.ContainsRune(lines[0], 0x01) {
		t.Fatalf("控制字符必须被转义：%q", lines[0])
	}
}

// 超长路径截断，避免单行日志爆量。
func TestResolveDiagnosticTruncatesLongPath(t *testing.T) {
	diagOutput := captureLog(t)
	srv, _ := newCountingUpstream(t, http.StatusInternalServerError)

	svc, repos := newAssetService(t)
	if _, err := repos.Create("long-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}
	if _, _, err := svc.Resolve(context.Background(), "long-proxy", strings.Repeat("a", 900)+".jar"); err == nil {
		t.Fatalf("上游 5xx 应失败")
	}
	line := diagnosticLines(diagOutput())[0]
	if !strings.Contains(line, "已截断") {
		t.Fatalf("超长路径应被截断，实际：%s", line)
	}
	if len(line) > 1200 {
		t.Fatalf("截断后诊断行仍过长（%d 字节）：%s", len(line), line)
	}
}

// 上游 URL 只输出 scheme://host：path 段常含团队/项目名，属不必要暴露；
// 凭据引用名（credentialRef）一律不输出。
func TestResolveDiagnosticUpstreamHostOnly(t *testing.T) {
	diagOutput := captureLog(t)
	srv, _ := newCountingUpstream(t, http.StatusInternalServerError)

	svc, repos := newAssetService(t)
	// 上游 URL 带 path 段；凭据引用名取一个足够独特、可被字符串断言命中的值。
	if _, err := repos.Create("host-proxy", "raw", "proxy", "private",
		proxyConfigWithCredentialJSON(t, srv.URL+"/team-internal/project", "upstream-secret-ref")); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}
	if _, _, err := svc.Resolve(context.Background(), "host-proxy", "x.jar"); err == nil {
		t.Fatalf("上游 5xx 应失败")
	}

	logs := diagOutput()
	line := diagnosticLines(logs)[0]
	if !strings.Contains(line, "upstream_host="+srv.URL) {
		t.Fatalf("诊断行应含 scheme://host（%s），实际：%s", srv.URL, line)
	}
	for _, forbidden := range []string{"team-internal", "project", "upstream-secret-ref", "Authorization"} {
		if strings.Contains(logs, forbidden) {
			t.Fatalf("诊断日志不得包含 %q：%s", forbidden, logs)
		}
	}
}

// --- 负缓存查看与清理 ---

// 清理后同路径立即回源：这是「负缓存可清」的用户可见证明。
func TestNegativeCachePurgeThenRefetch(t *testing.T) {
	t.Parallel()
	srv, hits := newCountingUpstream(t, http.StatusNotFound)

	svc, repos := newAssetService(t)
	repoID, err := repos.Create("purge-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "purge-proxy", "gone.jar"); err != domain.ErrNotFound {
		t.Fatalf("首次应 ErrNotFound，实际：%v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("首次应回源 1 次，实际 %d", got)
	}
	// 负缓存命中：不再回源。
	if _, _, err := svc.Resolve(context.Background(), "purge-proxy", "gone.jar"); err != domain.ErrNotFound {
		t.Fatalf("二次应命中负缓存返回 ErrNotFound，实际：%v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("负缓存命中不应回源，实际 %d 次", got)
	}
	// 精确清理后立即回源（上游确实被再访问一次，而不是返回缓存 404）。
	if removed := svc.PurgeNegativeCache(domain.NegativeCacheFilter{RepositoryID: repoID, ExactPath: "gone.jar"}); removed != 1 {
		t.Fatalf("精确清理应删除 1 条，实际 %d", removed)
	}
	if _, _, err := svc.Resolve(context.Background(), "purge-proxy", "gone.jar"); err != domain.ErrNotFound {
		t.Fatalf("清理后应重新回源并 ErrNotFound，实际：%v", err)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("清理后应重新回源（共 2 次），实际 %d 次", got)
	}
}

// 清理范围：按仓库清与全清各自只影响目标范围。
func TestNegativeCachePurgeScopes(t *testing.T) {
	t.Parallel()
	srvA, _ := newCountingUpstream(t, http.StatusNotFound)
	srvB, _ := newCountingUpstream(t, http.StatusNotFound)

	svc, repos := newAssetService(t)
	repoAID, err := repos.Create("scope-a", "raw", "proxy", "private", proxyConfigJSON(t, srvA.URL))
	if err != nil {
		t.Fatalf("建仓库 A：%v", err)
	}
	if _, err := repos.Create("scope-b", "raw", "proxy", "private", proxyConfigJSON(t, srvB.URL)); err != nil {
		t.Fatalf("建仓库 B：%v", err)
	}
	for _, path := range []string{"a1.jar", "a2.jar"} {
		if _, _, err := svc.Resolve(context.Background(), "scope-a", path); err != domain.ErrNotFound {
			t.Fatalf("A/%s 应 ErrNotFound，实际：%v", path, err)
		}
	}
	if _, _, err := svc.Resolve(context.Background(), "scope-b", "b1.jar"); err != domain.ErrNotFound {
		t.Fatalf("B/b1.jar 应 ErrNotFound，实际：%v", err)
	}

	// 按仓库只清 A 的两条，B 保留。
	if removed := svc.PurgeNegativeCache(domain.NegativeCacheFilter{RepositoryID: repoAID}); removed != 2 {
		t.Fatalf("按仓库清理应删除 2 条，实际 %d", removed)
	}
	snap := mustSnapshot(t, svc, domain.NegativeCacheFilter{})
	if snap.Total != 1 || snap.Items[0].Repository != "scope-b" {
		t.Fatalf("按仓库清理后应只剩 scope-b 的 1 条，实际 %+v", snap)
	}
	// 全清。
	if removed := svc.PurgeNegativeCache(domain.NegativeCacheFilter{}); removed != 1 {
		t.Fatalf("全清应删除 1 条，实际 %d", removed)
	}
	if snap := mustSnapshot(t, svc, domain.NegativeCacheFilter{}); snap.Total != 0 || len(snap.Items) != 0 {
		t.Fatalf("全清后应为空，实际 %+v", snap)
	}
}

// mustSnapshot 取负缓存快照并在失败时终止用例。
func mustSnapshot(t *testing.T, svc *domain.AssetService, f domain.NegativeCacheFilter) domain.NegativeCacheSnapshot {
	t.Helper()
	snap, err := svc.NegativeCacheSnapshot(f)
	if err != nil {
		t.Fatalf("取负缓存快照：%v", err)
	}
	return snap
}

// 快照的过滤与分页：total 是过滤前总数，repo / path_prefix 过滤生效，limit 截断生效。
func TestNegativeCacheSnapshotFilters(t *testing.T) {
	t.Parallel()
	srvA, _ := newCountingUpstream(t, http.StatusNotFound)
	srvB, _ := newCountingUpstream(t, http.StatusNotFound)

	svc, repos := newAssetService(t)
	svc.SetNegativeCacheTTL(time.Hour)
	repoAID, err := repos.Create("snap-a", "raw", "proxy", "private", proxyConfigJSON(t, srvA.URL))
	if err != nil {
		t.Fatalf("建仓库 A：%v", err)
	}
	if _, err := repos.Create("snap-b", "raw", "proxy", "private", proxyConfigJSON(t, srvB.URL)); err != nil {
		t.Fatalf("建仓库 B：%v", err)
	}
	for _, path := range []string{"lib/a1.jar", "lib/a2.jar", "other/a3.jar"} {
		if _, _, err := svc.Resolve(context.Background(), "snap-a", path); err != domain.ErrNotFound {
			t.Fatalf("A/%s 应 ErrNotFound，实际：%v", path, err)
		}
	}
	if _, _, err := svc.Resolve(context.Background(), "snap-b", "lib/b1.jar"); err != domain.ErrNotFound {
		t.Fatalf("B/lib/b1.jar 应 ErrNotFound，实际：%v", err)
	}

	all := mustSnapshot(t, svc, domain.NegativeCacheFilter{})
	if all.Total != 4 || len(all.Items) != 4 {
		t.Fatalf("全量应为 4 条，实际 total=%d items=%d", all.Total, len(all.Items))
	}
	if all.TTL != time.Hour || all.MaxEntries <= 0 {
		t.Fatalf("快照应带出 TTL 与上限，实际 %+v", all)
	}
	// 条目字段完整：仓库名由仓库表补全，剩余 TTL 必须为正。
	for _, item := range all.Items {
		if item.Repository == "" || item.Path == "" || item.Remaining <= 0 || item.WrittenAt.IsZero() {
			t.Fatalf("条目字段不完整：%+v", item)
		}
	}

	// 按仓库过滤：total 仍是过滤前总数。
	byRepo := mustSnapshot(t, svc, domain.NegativeCacheFilter{RepositoryID: repoAID})
	if byRepo.Total != 4 {
		t.Fatalf("按仓库过滤时 total 应为过滤前总数 4，实际 %d", byRepo.Total)
	}
	if len(byRepo.Items) != 3 {
		t.Fatalf("snap-a 应有 3 条，实际 %d", len(byRepo.Items))
	}
	for _, item := range byRepo.Items {
		if item.Repository != "snap-a" {
			t.Fatalf("过滤结果混入其它仓库：%+v", item)
		}
	}

	// 按路径前缀过滤。
	byPrefix := mustSnapshot(t, svc, domain.NegativeCacheFilter{RepositoryID: repoAID, PathPrefix: "lib/"})
	if len(byPrefix.Items) != 2 {
		t.Fatalf("lib/ 前缀应有 2 条，实际 %d", len(byPrefix.Items))
	}

	// limit 截断；且按写入时间升序（第一条是最早写入的 lib/a1.jar）。
	limited := mustSnapshot(t, svc, domain.NegativeCacheFilter{RepositoryID: repoAID, Limit: 1})
	if len(limited.Items) != 1 {
		t.Fatalf("limit=1 应只返回 1 条，实际 %d", len(limited.Items))
	}
	if limited.Items[0].Path != "lib/a1.jar" {
		t.Fatalf("应按写入时间升序返回最早的一条，实际 %s", limited.Items[0].Path)
	}
}

// --- 负缓存容量上限（FR-157 前置改动）---

// 达到上限后淘汰写入时间最早的一条，条目数不超过上限。
func TestNegativeCacheMaxEntriesEvictsOldest(t *testing.T) {
	t.Parallel()
	srv, _ := newCountingUpstream(t, http.StatusNotFound)

	svc, repos := newAssetService(t)
	svc.SetNegativeCacheTTL(time.Hour)
	svc.SetNegativeCacheMaxEntries(3)
	if _, err := repos.Create("cap-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	paths := []string{"p1.jar", "p2.jar", "p3.jar", "p4.jar"}
	for _, path := range paths {
		if _, _, err := svc.Resolve(context.Background(), "cap-proxy", path); err != domain.ErrNotFound {
			t.Fatalf("%s 应 ErrNotFound，实际：%v", path, err)
		}
		// 拉开写入时间，保证「最早写入」有确定答案（实现按写入时间淘汰）。
		time.Sleep(2 * time.Millisecond)
	}

	snap := mustSnapshot(t, svc, domain.NegativeCacheFilter{})
	if snap.Total != 3 {
		t.Fatalf("上限 3 时条目数不得超过 3，实际 %d", snap.Total)
	}
	if snap.MaxEntries != 3 {
		t.Fatalf("快照应反映当前上限 3，实际 %d", snap.MaxEntries)
	}
	got := make(map[string]bool, len(snap.Items))
	for _, item := range snap.Items {
		got[item.Path] = true
	}
	if got["p1.jar"] {
		t.Fatalf("应淘汰最早写入的 p1.jar，实际保留 %v", got)
	}
	for _, keep := range []string{"p2.jar", "p3.jar", "p4.jar"} {
		if !got[keep] {
			t.Fatalf("应保留 %s，实际保留 %v", keep, got)
		}
	}
}

// 写入触发过期清理：过期条目在 add 时被真实删除，内存不随时间无限增长。
func TestNegativeCacheAddPurgesExpiredEntries(t *testing.T) {
	t.Parallel()
	srv, _ := newCountingUpstream(t, http.StatusNotFound)

	svc, repos := newAssetService(t)
	svc.SetNegativeCacheTTL(30 * time.Millisecond)
	svc.SetNegativeCacheMaxEntries(1)
	if _, err := repos.Create("expire-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}
	if _, _, err := svc.Resolve(context.Background(), "expire-proxy", "old.jar"); err != domain.ErrNotFound {
		t.Fatalf("old.jar 应 ErrNotFound，实际：%v", err)
	}
	// 等旧条目过期：此时若不清理，新写入会因「已达上限」而淘汰（或保留）旧条目，
	// 条目数仍可能是 1，因此用「剩余 TTL 为正且只有新条目」来区分「清理」与「淘汰」。
	time.Sleep(50 * time.Millisecond)
	if _, _, err := svc.Resolve(context.Background(), "expire-proxy", "new.jar"); err != domain.ErrNotFound {
		t.Fatalf("new.jar 应 ErrNotFound，实际：%v", err)
	}

	snap := mustSnapshot(t, svc, domain.NegativeCacheFilter{})
	if snap.Total != 1 {
		t.Fatalf("过期条目应被清理，实际条目数 %d", snap.Total)
	}
	if snap.Items[0].Path != "new.jar" {
		t.Fatalf("清理后应只剩 new.jar，实际 %s", snap.Items[0].Path)
	}
	if snap.Items[0].Remaining <= 0 {
		t.Fatalf("new.jar 应仍在有效期内，实际剩余 %v", snap.Items[0].Remaining)
	}
}

// 同键重复写入不占新额度：反复确认同一路径的 404 不应淘汰其它路径。
func TestNegativeCacheAddSameKeyDoesNotEvict(t *testing.T) {
	t.Parallel()
	srv, _ := newCountingUpstream(t, http.StatusNotFound)

	svc, repos := newAssetService(t)
	svc.SetNegativeCacheTTL(time.Hour)
	svc.SetNegativeCacheMaxEntries(2)
	if _, err := repos.Create("samekey-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}
	for _, path := range []string{"k1.jar", "k2.jar"} {
		if _, _, err := svc.Resolve(context.Background(), "samekey-proxy", path); err != domain.ErrNotFound {
			t.Fatalf("%s 应 ErrNotFound，实际：%v", path, err)
		}
	}
	// 清掉负缓存后再写同键：命中路径会先被 negCache 挡住，因此这里直接用清理验证额度不被挤占。
	if removed := svc.PurgeNegativeCache(domain.NegativeCacheFilter{ExactPath: "k1.jar"}); removed != 1 {
		t.Fatalf("精确清理应删除 1 条，实际 %d", removed)
	}
	if _, _, err := svc.Resolve(context.Background(), "samekey-proxy", "k1.jar"); err != domain.ErrNotFound {
		t.Fatalf("k1.jar 重新回源应 ErrNotFound，实际：%v", err)
	}
	snap := mustSnapshot(t, svc, domain.NegativeCacheFilter{})
	if snap.Total != 2 {
		t.Fatalf("同键重写不应使条目数变化，实际 %d", snap.Total)
	}
}
