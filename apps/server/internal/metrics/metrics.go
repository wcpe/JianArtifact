// Package metrics 提供进程内指标累计与 Prometheus 文本暴露格式渲染（FR-39）。
//
// 设计要点见 docs/specs/0.11.0-prometheus-metrics.md：
//   - 零第三方依赖，文本暴露格式手写渲染；
//   - 标签基数受控：未知方法与异常状态码归 other，缓存结果只取三个已知值；
//   - 抓取路径不查询数据库、不访问网络，全部为进程内数据。
package metrics

import (
	"io"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 内容类型为 Prometheus 文本暴露格式；与 api/openapi.yaml 的 /metrics 声明一致。
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// 已知的请求方法与缓存结果取值；未知值一律归入 other / unknown，避免标签基数膨胀。
var knownMethods = map[string]bool{
	"GET": true, "HEAD": true, "PUT": true, "POST": true,
	"DELETE": true, "OPTIONS": true, "PATCH": true,
}

const (
	labelOther        = "other"
	cacheUnknown      = "unknown"
	cacheHit          = "hit"
	cacheMiss         = "miss"
	metricPrefix      = "jianartifact_"
	protocolMetric    = metricPrefix + "protocol_requests_total"
	runtimeGoroutines = metricPrefix + "runtime_goroutines"
	runtimeHeapBytes  = metricPrefix + "runtime_heap_bytes"
	runtimeGCTotal    = metricPrefix + "runtime_gc_total"
)

// protocolKey 是制品协议请求计数的标签组合（值均已归一化）。
type protocolKey struct {
	method      string
	status      string
	cacheResult string
}

// Registry 累计进程内计数器；并发安全。
type Registry struct {
	mu       sync.Mutex
	protocol map[protocolKey]uint64
}

// New 创建空指标登记表。
func New() *Registry {
	return &Registry{protocol: make(map[protocolKey]uint64)}
}

// ProtocolRequest 记录一次已完成的制品协议请求；标签值在内部归一化。
func (r *Registry) ProtocolRequest(method string, status int, cacheResult string) {
	key := protocolKey{
		method:      normalizeMethod(method),
		status:      normalizeStatus(status),
		cacheResult: normalizeCacheResult(cacheResult),
	}
	r.mu.Lock()
	r.protocol[key]++
	r.mu.Unlock()
}

// protocolSnapshot 复制一份计数快照，避免渲染期间持锁。
func (r *Registry) protocolSnapshot() map[protocolKey]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[protocolKey]uint64, len(r.protocol))
	for k, v := range r.protocol {
		out[k] = v
	}
	return out
}

// JobStatus 是渲染用的定时任务作业状态快照（由调度器适配而来）。
type JobStatus struct {
	// Name 是作业名称。
	Name string
	// Runs 是已执行次数（含失败）。
	Runs int64
	// Failures 是失败次数（含 panic）。
	Failures int64
	// Running 表示当前是否正在执行。
	Running bool
	// LastRun 是最近一次结束时间；零值表示尚未执行。
	LastRun time.Time
	// Failed 表示最近一次是否失败。
	Failed bool
}

// RuntimeStats 是渲染时的进程运行时快照。
type RuntimeStats struct {
	Goroutines int
	HeapBytes  uint64
	GCCount    uint32
}

// readRuntimeStats 读取真实的进程运行时数据。
func readRuntimeStats() RuntimeStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return RuntimeStats{Goroutines: runtime.NumGoroutine(), HeapBytes: ms.HeapAlloc, GCCount: ms.NumGC}
}

// Exposition 把指标登记表与动态来源渲染为 Prometheus 文本暴露格式。
type Exposition struct {
	registry *Registry
	jobs     func() []JobStatus  // 定时任务作业快照；nil 表示不输出作业指标
	runtime  func() RuntimeStats // 运行时快照；测试可替换
}

// NewExposition 构造渲染器；jobs 可为 nil。
func NewExposition(registry *Registry, jobs func() []JobStatus) *Exposition {
	return &Exposition{registry: registry, jobs: jobs, runtime: readRuntimeStats}
}

// WritePrometheus 按文本暴露格式写出当前指标快照。
func (e *Exposition) WritePrometheus(w io.Writer) {
	_, _ = io.WriteString(w, e.render())
}

// labelPair 是单个标签。
type labelPair struct {
	name  string
	value string
}

// sample 是单个样本行。
type sample struct {
	labels []labelPair
	value  string
}

// family 是一组同名指标（HELP / TYPE / 样本）。
type family struct {
	name      string
	help      string
	kind      string // counter / gauge
	available bool   // 该指标族是否随进程存在（与当前是否有样本无关）
	samples   []sample
}

// render 生成完整文本：指标按名称排序，样本按标签值排序，保证输出稳定。
func (e *Exposition) render() string {
	families := []family{
		e.protocolFamily(),
		e.schedulerFamily("runs_total", "counter", "定时任务作业累计执行次数", func(st JobStatus) *sample {
			return &sample{labels: jobLabels(st.Name), value: strconv.FormatInt(st.Runs, 10)}
		}),
		e.schedulerFamily("failures_total", "counter", "定时任务作业累计失败次数（含 panic）", func(st JobStatus) *sample {
			return &sample{labels: jobLabels(st.Name), value: strconv.FormatInt(st.Failures, 10)}
		}),
		e.schedulerFamily("running", "gauge", "定时任务作业当前是否正在执行", func(st JobStatus) *sample {
			return &sample{labels: jobLabels(st.Name), value: boolValue(st.Running)}
		}),
		e.schedulerFamily("last_run_timestamp_seconds", "gauge", "定时任务作业最近一次结束的 Unix 时间（秒），尚未执行时无样本", func(st JobStatus) *sample {
			if st.LastRun.IsZero() {
				return nil
			}
			return &sample{labels: jobLabels(st.Name), value: timestampValue(st.LastRun)}
		}),
		e.schedulerFamily("last_run_failed", "gauge", "定时任务作业最近一次是否失败，尚未执行时无样本", func(st JobStatus) *sample {
			if st.LastRun.IsZero() {
				return nil
			}
			return &sample{labels: jobLabels(st.Name), value: boolValue(st.Failed)}
		}),
	}
	rs := e.runtime()
	families = append(families,
		family{name: runtimeGoroutines, help: "当前 goroutine 数量", kind: "gauge", available: true,
			samples: []sample{{value: strconv.Itoa(rs.Goroutines)}}},
		family{name: runtimeHeapBytes, help: "当前堆占用字节数", kind: "gauge", available: true,
			samples: []sample{{value: strconv.FormatUint(rs.HeapBytes, 10)}}},
		family{name: runtimeGCTotal, help: "累计 GC 次数", kind: "counter", available: true,
			samples: []sample{{value: strconv.FormatUint(uint64(rs.GCCount), 10)}}},
	)

	sort.Slice(families, func(i, j int) bool { return families[i].name < families[j].name })
	var b strings.Builder
	for _, f := range families {
		// 固定指标族即使暂无样本也输出 HELP / TYPE：抓取端据此发现指标，样本行只反映真实数据。
		if !f.available {
			continue
		}
		sortSamples(f.samples)
		b.WriteString("# HELP " + f.name + " " + escapeHelp(f.help) + "\n")
		b.WriteString("# TYPE " + f.name + " " + f.kind + "\n")
		for _, s := range f.samples {
			b.WriteString(f.name)
			writeLabels(&b, s.labels)
			b.WriteString(" " + s.value + "\n")
		}
	}
	return b.String()
}

// protocolFamily 汇总制品协议请求计数；进程存在即视为可用（首抓无样本时仍输出 HELP / TYPE）。
func (e *Exposition) protocolFamily() family {
	f := family{
		name:      protocolMetric,
		help:      "制品协议请求完成计数（按方法、状态码与缓存结果分区）",
		kind:      "counter",
		available: true,
	}
	for key, count := range e.registry.protocolSnapshot() {
		f.samples = append(f.samples, sample{
			labels: []labelPair{
				{name: "cache_result", value: key.cacheResult},
				{name: "method", value: key.method},
				{name: "status", value: key.status},
			},
			value: strconv.FormatUint(count, 10),
		})
	}
	return f
}

// schedulerFamily 汇总某个调度器作业指标；suffix 为指标名后缀。
// 未接入调度器（jobs 为 nil）时该族不可用，整族不输出。
func (e *Exposition) schedulerFamily(suffix, kind, help string, build func(JobStatus) *sample) family {
	f := family{name: metricPrefix + "scheduler_job_" + suffix, help: help, kind: kind, available: e.jobs != nil}
	if e.jobs == nil {
		return f
	}
	for _, st := range e.jobs() {
		if s := build(st); s != nil {
			f.samples = append(f.samples, *s)
		}
	}
	return f
}

// jobLabels 返回作业指标的标签集合。
func jobLabels(name string) []labelPair {
	return []labelPair{{name: "job", value: name}}
}

// boolValue 把布尔量渲染为 0 / 1。
func boolValue(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// timestampValue 把时间渲染为秒级浮点（保留毫秒精度）。
func timestampValue(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', -1, 64)
}

// normalizeMethod 归一化请求方法：未知方法归 other。
func normalizeMethod(method string) string {
	upper := strings.ToUpper(method)
	if knownMethods[upper] {
		return upper
	}
	return labelOther
}

// normalizeStatus 归一化状态码：合法三位码按字面量，其余归 other。
func normalizeStatus(status int) string {
	if status < 100 || status > 599 {
		return labelOther
	}
	return strconv.Itoa(status)
}

// normalizeCacheResult 归一化缓存结果：仅保留 hit / miss，其余归 unknown。
func normalizeCacheResult(result string) string {
	switch result {
	case cacheHit:
		return cacheHit
	case cacheMiss:
		return cacheMiss
	default:
		return cacheUnknown
	}
}

// sortSamples 按标签值序列排序，保证输出稳定。
func sortSamples(samples []sample) {
	sort.Slice(samples, func(i, j int) bool {
		return labelsKey(samples[i].labels) < labelsKey(samples[j].labels)
	})
}

// labelsKey 生成用于排序的标签值拼接键。
func labelsKey(labels []labelPair) string {
	var b strings.Builder
	for _, l := range labels {
		b.WriteString(l.name + "\x00" + l.value + "\x00")
	}
	return b.String()
}

// writeLabels 写出标签集合，值按文本暴露格式转义。
func writeLabels(b *strings.Builder, labels []labelPair) {
	if len(labels) == 0 {
		return
	}
	b.WriteString("{")
	for i, l := range labels {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(l.name + `="` + escapeLabelValue(l.value) + `"`)
	}
	b.WriteString("}")
}

// escapeLabelValue 转义标签值中的反斜杠、双引号与换行。
func escapeLabelValue(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return replacer.Replace(value)
}

// escapeHelp 转义 HELP 文本中的反斜杠与换行。
func escapeHelp(help string) string {
	replacer := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return replacer.Replace(help)
}
