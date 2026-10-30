package metrics

import (
	"strings"
	"testing"
)

// 本文件覆盖代理解析可观测新增的两个指标族：解析失败结论计数与负缓存条目数。
// 用例复用 metrics_test.go 里的 newTestExposition / render。

// 首抓（尚无样本）时两个族都必须输出 HELP / TYPE：沿用既有「固定指标族即使无样本
// 也输出 HELP / TYPE」的约定，抓取端据此发现指标。
func TestResolveFamiliesEmitHelpAndTypeWithoutSamples(t *testing.T) {
	_, exp := newTestExposition(nil)
	// 负缓存族在接线后才可用；这里先接线，使两族都可断言。
	exp.SetNegativeCacheEntries(func() int { return 0 })
	out := render(t, exp)

	for _, name := range []string{resolveFailuresMetric, negativeCacheMetric} {
		if !strings.Contains(out, "# HELP "+name+" ") {
			t.Fatalf("指标 %s 缺少 HELP：\n%s", name, out)
		}
	}
	if !strings.Contains(out, "# TYPE "+resolveFailuresMetric+" counter") {
		t.Fatalf("resolve_failures 应为 counter：\n%s", out)
	}
	if !strings.Contains(out, "# TYPE "+negativeCacheMetric+" gauge") {
		t.Fatalf("negative_cache_entries 应为 gauge：\n%s", out)
	}
}

// 未接线负缓存取数回调时该族整族不输出（无值可报，不虚构 0）。
func TestNegativeCacheFamilyAbsentWhenNotWired(t *testing.T) {
	_, exp := newTestExposition(nil)
	out := render(t, exp)
	if strings.Contains(out, negativeCacheMetric) {
		t.Fatalf("未接线时不应输出负缓存条目族：\n%s", out)
	}
}

// 结论取值是闭集：闭集外的取值直接忽略，不新增序列。
func TestResolveFailureIgnoresUnknownResolution(t *testing.T) {
	reg, exp := newTestExposition(nil)
	reg.ResolveFailure("confirmed")
	reg.ResolveFailure("timeout")   // 不是本指标的取值（属阻断事件维度）
	reg.ResolveFailure("")          // Unknown 不计样本
	reg.ResolveFailure("CONFIRMED") // 大小写不同即不同值，同样忽略

	out := render(t, exp)
	if !strings.Contains(out, `jianartifact_resolve_failures_total{resolution="confirmed"} 1`) {
		t.Fatalf("confirmed 应计 1：\n%s", out)
	}
	for _, forbidden := range []string{`resolution="timeout"`, `resolution=""`, `resolution="CONFIRMED"`} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("闭集外取值不得产生序列（%s）：\n%s", forbidden, out)
		}
	}
}

// 解析失败计数按结论分区累加，且只有两个取值。
func TestResolveFailureCountsBothResolutions(t *testing.T) {
	reg, exp := newTestExposition(nil)
	reg.ResolveFailure(resolutionConfirmed)
	reg.ResolveFailure(resolutionUncertain)
	reg.ResolveFailure(resolutionUncertain)

	out := render(t, exp)
	for _, want := range []string{
		`jianartifact_resolve_failures_total{resolution="confirmed"} 1`,
		`jianartifact_resolve_failures_total{resolution="uncertain"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, out)
		}
	}
}

// 负缓存条目数取数自回调，每次渲染实时读（不缓存）。
func TestNegativeCacheEntriesFollowsCallback(t *testing.T) {
	_, exp := newTestExposition(nil)
	entries := 0
	exp.SetNegativeCacheEntries(func() int { return entries })

	if out := render(t, exp); !strings.Contains(out, "jianartifact_negative_cache_entries 0") {
		t.Fatalf("应输出 0 条样本：\n%s", out)
	}
	entries = 7
	if out := render(t, exp); !strings.Contains(out, "jianartifact_negative_cache_entries 7") {
		t.Fatalf("应实时反映 7 条：\n%s", out)
	}
}

// 与阻断事件维度并存且互不改写：同一次上游故障让两族各自累加，
// 但两族不可相加、不可互相推导（口径边界，spec §3.2）。
func TestResolveFailureIndependentOfBlockEvents(t *testing.T) {
	reg, exp := newTestExposition(nil)
	// 一次上游故障：状态机记一次阻止事件，同时本次解析结论为不可判定。
	reg.UpstreamBlockEvent(blockReasonUpstreamError)
	reg.ResolveFailure(resolutionUncertain)

	out := render(t, exp)
	for _, want := range []string{
		`jianartifact_upstream_block_events_total{reason="upstream_error"} 1`,
		`jianartifact_resolve_failures_total{resolution="uncertain"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, out)
		}
	}
	// 解析失败族不得出现阻断事件的 reason 标签，反之亦然。
	if strings.Contains(out, `jianartifact_resolve_failures_total{reason=`) {
		t.Fatalf("解析失败族不得带 reason 标签：\n%s", out)
	}
	// 再记一次阻止事件不应改变解析失败计数（两族独立累加）。
	reg.UpstreamBlockEvent(blockReasonUpstreamError)
	out = render(t, exp)
	if !strings.Contains(out, `jianartifact_resolve_failures_total{resolution="uncertain"} 1`) {
		t.Fatalf("阻断事件不应改写解析失败计数：\n%s", out)
	}
	if !strings.Contains(out, `jianartifact_upstream_block_events_total{reason="upstream_error"} 2`) {
		t.Fatalf("阻断事件应独立累加到 2：\n%s", out)
	}
}
