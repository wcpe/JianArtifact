package metrics

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestExposition 构造固定运行时数据的渲染器，便于断言确定性输出。
func newTestExposition(jobs func() []JobStatus) (*Registry, *Exposition) {
	reg := New()
	exp := NewExposition(reg, jobs)
	exp.runtime = func() RuntimeStats {
		return RuntimeStats{Goroutines: 7, HeapBytes: 1024, GCCount: 3}
	}
	return reg, exp
}

func render(t *testing.T, exp *Exposition) string {
	t.Helper()
	var buf bytes.Buffer
	exp.WritePrometheus(&buf)
	return buf.String()
}

func TestExpositionAlwaysIncludesRuntimeMetrics(t *testing.T) {
	_, exp := newTestExposition(nil)
	out := render(t, exp)
	for _, want := range []string{
		"# TYPE jianartifact_runtime_goroutines gauge",
		"jianartifact_runtime_goroutines 7",
		"jianartifact_runtime_heap_bytes 1024",
		"# TYPE jianartifact_runtime_gc_total counter",
		"jianartifact_runtime_gc_total 3",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, out)
		}
	}
}

func TestProtocolRequestNormalizesLabels(t *testing.T) {
	reg, exp := newTestExposition(nil)
	reg.ProtocolRequest("get", 200, "hit")
	reg.ProtocolRequest("BREW", 200, "")
	reg.ProtocolRequest("GET", 999, "hit")
	out := render(t, exp)

	for _, want := range []string{
		`jianartifact_protocol_requests_total{cache_result="hit",method="GET",status="200"} 1`,
		`jianartifact_protocol_requests_total{cache_result="unknown",method="other",status="200"} 1`,
		`jianartifact_protocol_requests_total{cache_result="hit",method="GET",status="other"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, out)
		}
	}
}

func TestProtocolRequestAccumulates(t *testing.T) {
	reg, exp := newTestExposition(nil)
	reg.ProtocolRequest("GET", 200, "miss")
	reg.ProtocolRequest("GET", 200, "miss")
	reg.ProtocolRequest("GET", 404, "miss")
	out := render(t, exp)
	if !strings.Contains(out, `jianartifact_protocol_requests_total{cache_result="miss",method="GET",status="200"} 2`) {
		t.Fatalf("计数未累加：\n%s", out)
	}
	if !strings.Contains(out, `jianartifact_protocol_requests_total{cache_result="miss",method="GET",status="404"} 1`) {
		t.Fatalf("不同状态码未分开计数：\n%s", out)
	}
}

func TestSchedulerJobMetrics(t *testing.T) {
	lastRun := time.Unix(1_700_000_000, 0)
	_, exp := newTestExposition(func() []JobStatus {
		return []JobStatus{
			{Name: "blob-gc", Runs: 5, Failures: 1, Running: true, LastRun: lastRun, Failed: true},
			{Name: "pending", Runs: 0, Failures: 0},
		}
	})
	out := render(t, exp)

	for _, want := range []string{
		`jianartifact_scheduler_job_runs_total{job="blob-gc"} 5`,
		`jianartifact_scheduler_job_failures_total{job="blob-gc"} 1`,
		`jianartifact_scheduler_job_running{job="blob-gc"} 1`,
		`jianartifact_scheduler_job_last_run_timestamp_seconds{job="blob-gc"} 1700000000`,
		`jianartifact_scheduler_job_last_run_failed{job="blob-gc"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, out)
		}
	}
	// 尚未执行的作业只输出计数与运行中仪表，不虚构时间与失败结论。
	if strings.Contains(out, `last_run_timestamp_seconds{job="pending"}`) || strings.Contains(out, `last_run_failed{job="pending"}`) {
		t.Fatalf("未执行的作业不应输出最近执行时间与失败结论：\n%s", out)
	}
	if !strings.Contains(out, `jianartifact_scheduler_job_runs_total{job="pending"} 0`) {
		t.Fatalf("未执行的作业应输出零计数：\n%s", out)
	}
}

func TestExpositionEscapesLabelValuesAndSorts(t *testing.T) {
	_, exp := newTestExposition(func() []JobStatus {
		return []JobStatus{
			{Name: "zz\"x"},
			{Name: `aa\y`},
			{Name: "aa\nb"},
			{Name: "alpha"},
		}
	})
	out := render(t, exp)
	if !strings.Contains(out, `job="zz\"x"`) {
		t.Fatalf("双引号未转义：\n%s", out)
	}
	if !strings.Contains(out, `job="aa\\y"`) {
		t.Fatalf("反斜杠未转义：\n%s", out)
	}
	if !strings.Contains(out, `job="aa\nb"`) {
		t.Fatalf("换行未转义为 \\n：\n%s", out)
	}
	// 标签按原始值升序输出：aa\nb < aa\y < alpha < zz"x
	iNewline := strings.Index(out, `job="aa\nb"`)
	iBackslash := strings.Index(out, `job="aa\\y"`)
	iAlpha := strings.Index(out, `job="alpha"`)
	iQuote := strings.Index(out, `job="zz\"x"`)
	if !(iNewline < iBackslash && iBackslash < iAlpha && iAlpha < iQuote) {
		t.Fatalf("标签未按序输出：newline=%d backslash=%d alpha=%d quote=%d\n%s", iNewline, iBackslash, iAlpha, iQuote, out)
	}
}

func TestExpositionStructurePairsHelpAndType(t *testing.T) {
	reg, exp := newTestExposition(func() []JobStatus { return []JobStatus{{Name: "j", Runs: 1}} })
	reg.ProtocolRequest("GET", 200, "hit")
	out := render(t, exp)

	for _, name := range []string{
		"jianartifact_protocol_requests_total",
		"jianartifact_scheduler_job_failures_total",
		"jianartifact_scheduler_job_runs_total",
		"jianartifact_scheduler_job_running",
		"jianartifact_runtime_gc_total",
		"jianartifact_runtime_goroutines",
		"jianartifact_runtime_heap_bytes",
	} {
		if !strings.Contains(out, "# HELP "+name+" ") || !strings.Contains(out, "# TYPE "+name+" ") {
			t.Fatalf("指标 %s 缺少 HELP 或 TYPE：\n%s", name, out)
		}
	}
}

func TestExpositionEmitsFixedFamiliesWithoutSamples(t *testing.T) {
	// 未接入调度器且尚无任何协议请求：固定指标族仍输出 HELP / TYPE，
	// 样本行只反映真实数据，不虚构零值序列。
	_, exp := newTestExposition(nil)
	out := render(t, exp)
	if !strings.Contains(out, "# TYPE "+protocolMetric+" counter") || !strings.Contains(out, "# HELP "+protocolMetric+" ") {
		t.Fatalf("固定指标族应无样本也输出 HELP / TYPE：\n%s", out)
	}
	if strings.Contains(out, protocolMetric+"{") {
		t.Fatalf("尚无请求时不应输出协议样本行：\n%s", out)
	}
	if strings.Contains(out, "jianartifact_scheduler_job_") {
		t.Fatalf("未接入调度器时不应输出作业指标：\n%s", out)
	}
}

func TestRegistryIsConcurrencySafe(t *testing.T) {
	reg, exp := newTestExposition(func() []JobStatus { return []JobStatus{{Name: "j"}} })
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				reg.ProtocolRequest("GET", 200, "hit")
				_ = render(t, exp)
			}
		}()
	}
	wg.Wait()
	if !strings.Contains(render(t, exp), `status="200"} 800`) {
		t.Fatalf("并发累加计数不正确：\n%s", render(t, exp))
	}
}

// TestPublishRejectionsRenderClosedSet 发布拒绝指标（FR-41）：reason 是闭集枚举，
// 闭集外的取值不产生样本；固定指标族即使暂无样本也输出 HELP / TYPE。
func TestPublishRejectionsRenderClosedSet(t *testing.T) {
	reg, exp := newTestExposition(nil)

	// 首抓（尚无样本）仍输出 HELP / TYPE，样本行只反映真实数据。
	out := render(t, exp)
	for _, want := range []string{
		"# HELP jianartifact_publish_rejections_total",
		"# TYPE jianartifact_publish_rejections_total counter",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "jianartifact_publish_rejections_total{") {
		t.Fatalf("尚未发生拒绝时不应输出样本：\n%s", out)
	}

	reg.PublishRejection("quota")
	reg.PublishRejection("quota")
	// 闭集外的取值必须被忽略：标签基数受控，暴露面与规格完全一致。
	reg.PublishRejection("bogus")
	reg.PublishRejection("")
	out = render(t, exp)
	if !strings.Contains(out, `jianartifact_publish_rejections_total{reason="quota"} 2`) {
		t.Fatalf("缺少 reason=quota 的计数：\n%s", out)
	}
	for _, unwanted := range []string{"bogus", `reason=""`, `reason="other"`} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("闭集外取值不得进入样本（%q）：\n%s", unwanted, out)
		}
	}
	if strings.Count(out, "jianartifact_publish_rejections_total{") != 1 {
		t.Fatalf("v1 只允许一个 reason 取值：\n%s", out)
	}
}

// TestPublishRejectionConcurrentWithRender 拒绝计数与渲染并发无数据竞争（-race 下验证）。
func TestPublishRejectionConcurrentWithRender(t *testing.T) {
	reg, exp := newTestExposition(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.PublishRejection("quota")
			_ = render(t, exp)
		}()
	}
	wg.Wait()
	if !strings.Contains(render(t, exp), `jianartifact_publish_rejections_total{reason="quota"} 8`) {
		t.Fatalf("并发累加计数不正确：\n%s", render(t, exp))
	}
}
