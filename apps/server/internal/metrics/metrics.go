// Package metrics 提供进程内指标累计与 Prometheus 文本暴露格式渲染。
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
	rejectionMetric   = metricPrefix + "publish_rejections_total"
	blockEventsMetric = metricPrefix + "upstream_block_events_total"
	blockedRepoMetric = metricPrefix + "upstream_blocked_repositories"
	runtimeGoroutines = metricPrefix + "runtime_goroutines"
	runtimeHeapBytes  = metricPrefix + "runtime_heap_bytes"
	runtimeGCTotal    = metricPrefix + "runtime_gc_total"
)

// 解析结果维度的指标名，独立成块以免打乱上面既有族的对齐（也是与阻断事件维度的物理分界）。
const (
	// resolveFailuresMetric 是「请求解析结果」维度的失败计数，与阻断事件维度**互不重叠**：
	// 前者回答「这次解析的结论是什么」，后者回答「状态机发生了几次阻止」。同一次上游故障
	// 可能让两者各自加一，但两者不可相加、不可互相推导。
	resolveFailuresMetric = metricPrefix + "resolve_failures_total"
	// negativeCacheMetric 报告当前负缓存有效条目数；无标签（仓库名/路径不得作为标签）。
	negativeCacheMetric = metricPrefix + "negative_cache_entries"
)

// 发布拒绝原因是**闭集枚举**：v1 只有 quota 一个取值。
// 该集合与 docs/specs/0.11.0-prometheus-metrics.md 的 reason 标签取值一一对应：
// **新增取值必须先改规格并同步测试与 OPERATIONS.md**，不允许实现期就地塞入自由字符串
// （自由字符串会把无界值带进指标标签，违反该规格的标签基数受控约定）。
var knownRejectionReasons = []string{rejectionQuota}

// rejectionQuota 是配额（发布额度或仓库存储配额）拒绝。
const rejectionQuota = "quota"

// 上游断路器判定事件原因同样是**闭集枚举**：
//   - probeFailed：后台/手动探测失败，阻止窗口延长一档；
//   - upstreamError：回源失败触发首次（或再次）自动阻止；
//   - upstreamTimeout：回源超时，被判为**不阻断**（不构成上游不可达证据）；
//   - slowTransfer：「已拿到响应头 + 有传输进展 + 未在超时内传完」= 带宽不足，不阻断；
//   - transferAborted：「已拿到响应头 + 中途被断开」= 连接不稳定，不阻断。
//
// 该集合与 docs/specs/0.12.0-upstream-circuit-breaker.md §3.3 一一对应，
// upstream_timeout 由 docs/specs/0.13.0-upstream-slow-not-unreachable.md §3.6 引入，
// slow_transfer / transfer_aborted 由
// docs/specs/0.13.0-upstream-failure-classification.md §2.3 引入。
// **新增取值必须先改规格并同步测试与 OPERATIONS.md**；闭集外的取值直接忽略，
// 以此保证标签基数受控（仓库名/路径一律不得作为标签，见 ARCHITECTURE.md 指标约定）。
var knownBlockReasons = []string{
	blockReasonProbeFailed,
	blockReasonUpstreamError,
	blockReasonTimeout,
	blockReasonSlowTransfer,
	blockReasonTransferAborted,
}

const (
	// blockReasonProbeFailed 表示探测失败导致阻止窗口延长。
	blockReasonProbeFailed = "probe_failed"
	// blockReasonUpstreamError 表示回源失败触发自动阻止。
	blockReasonUpstreamError = "upstream_error"
	// blockReasonTimeout 表示回源超时被判定不阻断（只记事件类别，不改阻断状态）。
	blockReasonTimeout = "upstream_timeout"
	// blockReasonSlowTransfer 表示「已拿到响应头、有传输进展但未传完」被判定不阻断。
	// 语义与 upstream_timeout 刻意区分：它是带宽不足，处置是等带宽或调大回源超时。
	blockReasonSlowTransfer = "slow_transfer"
	// blockReasonTransferAborted 表示「已拿到响应头、传输中途被断开」被判定不阻断。
	// 处置方向与 slow_transfer 不同：查上游主动断连或中间设备。
	blockReasonTransferAborted = "transfer_aborted"
)

// 解析失败结论同样是**闭集枚举**，且刻意只有两个取值：
//   - confirmed：已确认（上游或本地）资源不存在——正常的 404 流量；
//   - uncertain：失败可能由离线、阻止窗口或上游故障导致——不可判定。
//
// 取值直接对应 domain.ResolveFailureStatus 的两个常量；`Unknown`（没有结论）**不计样本**，
// 没有结论就不是「失败结论」。标签名取 resolution 而非 status：既有
// jianartifact_protocol_requests_total{status} 的 status 是 HTTP 状态码，同名不同义
// 会让 PromQL 阅读者误判。
//
// 本集合**不随阻断事件的 reason 闭集扩展而扩展**（那属另一个维度）。
var knownResolutions = []string{resolutionConfirmed, resolutionUncertain}

const (
	// resolutionConfirmed 表示解析失败已确认资源不存在（正常 404）。
	resolutionConfirmed = "confirmed"
	// resolutionUncertain 表示解析失败不可判定（可能只是暂时不可用）。
	resolutionUncertain = "uncertain"
)

// protocolKey 是制品协议请求计数的标签组合（值均已归一化）。
type protocolKey struct {
	method      string
	status      string
	cacheResult string
}

// Registry 累计进程内计数器；并发安全。
type Registry struct {
	mu         sync.Mutex
	protocol   map[protocolKey]uint64
	rejections map[string]uint64
	// blockEvents 是上游断路器阻止事件计数，按闭集原因分区。
	blockEvents map[string]uint64
	// resolveFailures 是解析失败结论计数，按闭集结论分区（confirmed / uncertain）。
	resolveFailures map[string]uint64
}

// New 创建空指标登记表。
func New() *Registry {
	return &Registry{
		protocol:        make(map[protocolKey]uint64),
		rejections:      make(map[string]uint64),
		blockEvents:     make(map[string]uint64),
		resolveFailures: make(map[string]uint64),
	}
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

// PublishRejection 记录一次发布拒绝，按原因分区。
// reason 必须是闭集枚举取值（v1 只有 quota）；闭集外的取值**直接忽略**，
// 以保证标签基数受控、暴露面与规格完全一致。
func (r *Registry) PublishRejection(reason string) {
	if !isKnownRejectionReason(reason) {
		return
	}
	r.mu.Lock()
	r.rejections[reason]++
	r.mu.Unlock()
}

// isKnownRejectionReason 判定原因是否属于闭集枚举。
func isKnownRejectionReason(reason string) bool {
	for _, known := range knownRejectionReasons {
		if reason == known {
			return true
		}
	}
	return false
}

// UpstreamBlockEvent 记录一次上游断路器判定事件，按原因分区。
// reason 必须是闭集枚举取值（probe_failed / upstream_error / upstream_timeout /
// slow_transfer / transfer_aborted）；闭集外的取值**直接忽略**，与 PublishRejection 同构，
// 以保证标签基数受控、暴露面与规格完全一致。upstream_timeout / slow_transfer /
// transfer_aborted 只表示「发生过一次被正确豁免的传输侧失败」，不表示发生过阻断——
// 该族语义是「判定事件类别」，不是「阻断发生次数」。
func (r *Registry) UpstreamBlockEvent(reason string) {
	if !isKnownBlockReason(reason) {
		return
	}
	r.mu.Lock()
	r.blockEvents[reason]++
	r.mu.Unlock()
}

// isKnownBlockReason 判定原因是否属于上游阻止事件的闭集枚举。
func isKnownBlockReason(reason string) bool {
	for _, known := range knownBlockReasons {
		if reason == known {
			return true
		}
	}
	return false
}

// ResolveFailure 记录一次解析失败，按结论分区累加。
//
// resolution 必须是闭集枚举取值（confirmed / uncertain）；闭集外的取值**直接忽略**，
// 与 PublishRejection / UpstreamBlockEvent 同构，以保证标签基数受控、
// 暴露面与规格完全一致。
//
// **采样点唯一性由调用方保证**：一次请求只能调用本方法一次。解析失败判定会在递归层、
// group 成员层被多次记录（且语义是「一旦 uncertain 就不被覆盖」），因此只能在**顶层
// 解析出口**读回最终结论后累加一次——不得在每层失败处顺手加一行，否则一次 group 请求
// 会被计成多次（见 domain.AssetService 顶层出口的说明与钉住该语义的测试）。
func (r *Registry) ResolveFailure(resolution string) {
	if !isKnownResolution(resolution) {
		return
	}
	r.mu.Lock()
	r.resolveFailures[resolution]++
	r.mu.Unlock()
}

// isKnownResolution 判定结论是否属于解析失败的闭集枚举。
func isKnownResolution(resolution string) bool {
	for _, known := range knownResolutions {
		if resolution == known {
			return true
		}
	}
	return false
}

// resolveFailureSnapshot 复制一份解析失败计数快照，避免渲染期间持锁。
func (r *Registry) resolveFailureSnapshot() map[string]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]uint64, len(r.resolveFailures))
	for k, v := range r.resolveFailures {
		out[k] = v
	}
	return out
}

// rejectionSnapshot 复制一份发布拒绝计数快照，避免渲染期间持锁。
func (r *Registry) rejectionSnapshot() map[string]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]uint64, len(r.rejections))
	for k, v := range r.rejections {
		out[k] = v
	}
	return out
}

// blockEventSnapshot 复制一份上游阻止事件计数快照，避免渲染期间持锁。
func (r *Registry) blockEventSnapshot() map[string]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]uint64, len(r.blockEvents))
	for k, v := range r.blockEvents {
		out[k] = v
	}
	return out
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
	// blockedRepositories 是当前处于阻止态（AUTO_BLOCKED / HALF_OPEN）的仓库数；
	// nil 表示未接入状态机，该族整族不输出（无值可报，不虚构 0）。
	blockedRepositories func() int
	// negativeCacheEntries 是当前负缓存有效条目数；nil 表示未接线，该族整族不输出。
	negativeCacheEntries func() int
}

// NewExposition 构造渲染器；jobs 与 blockedRepositories 均可为 nil。
func NewExposition(registry *Registry, jobs func() []JobStatus) *Exposition {
	return &Exposition{registry: registry, jobs: jobs, runtime: readRuntimeStats}
}

// SetBlockedRepositories 注入「当前阻止态仓库数」取数回调。
// 由装配层在状态机就绪后调用；未调用（nil）时该指标族整族不输出。
// 回调在每次抓取时调用，不缓存——抓取路径必须只读内存、不查库、不访问网络。
func (e *Exposition) SetBlockedRepositories(f func() int) { e.blockedRepositories = f }

// SetNegativeCacheEntries 注入「当前负缓存有效条目数」取数回调。
// 与 SetBlockedRepositories 同构：未接线（nil）时该族整族不输出；
// 回调在每次抓取时调用，只读内存态（内部会顺带清理过期条目），
// 不查库、不访问网络。
func (e *Exposition) SetNegativeCacheEntries(f func() int) { e.negativeCacheEntries = f }

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
		e.rejectionFamily(),
		e.blockEventFamily(),
		e.blockedRepositoriesFamily(),
		e.resolveFailureFamily(),
		e.negativeCacheEntriesFamily(),
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

// rejectionFamily 汇总发布拒绝计数；进程存在即视为可用，
// 首抓无样本时仍输出 HELP / TYPE，样本行只反映真实数据。
func (e *Exposition) rejectionFamily() family {
	f := family{
		name:      rejectionMetric,
		help:      "发布被拒绝的累计次数（按拒绝原因分区；原因取值是闭集枚举）",
		kind:      "counter",
		available: true,
	}
	for reason, count := range e.registry.rejectionSnapshot() {
		f.samples = append(f.samples, sample{
			labels: []labelPair{{name: "reason", value: reason}},
			value:  strconv.FormatUint(count, 10),
		})
	}
	return f
}

// blockEventFamily 汇总上游断路器判定事件计数（reason 含「被判为不阻断」的
// upstream_timeout，故该族语义是**判定事件类别**而非「阻断发生次数」）；
// 进程存在即视为可用，首抓无样本时仍输出 HELP / TYPE，样本行只反映真实数据。
func (e *Exposition) blockEventFamily() family {
	f := family{
		name:      blockEventsMetric,
		help:      "上游断路器判定事件累计次数（按原因分区；原因取值是闭集枚举）",
		kind:      "counter",
		available: true,
	}
	for reason, count := range e.registry.blockEventSnapshot() {
		f.samples = append(f.samples, sample{
			labels: []labelPair{{name: "reason", value: reason}},
			value:  strconv.FormatUint(count, 10),
		})
	}
	return f
}

// blockedRepositoriesFamily 报告当前处于阻止态（AUTO_BLOCKED / HALF_OPEN）的仓库数。
// 无标签：仓库名/路径不得作为标签（ARCHITECTURE.md 标签基数约定）。
// 未接入状态机（回调为 nil）时该族不可用，整族不输出——无值可报，不虚构 0。
func (e *Exposition) blockedRepositoriesFamily() family {
	f := family{
		name:      blockedRepoMetric,
		help:      "当前处于阻止态（自动阻止或半开试探）的上游仓库数",
		kind:      "gauge",
		available: e.blockedRepositories != nil,
	}
	if e.blockedRepositories == nil {
		return f
	}
	f.samples = []sample{{value: strconv.Itoa(e.blockedRepositories())}}
	return f
}

// resolveFailureFamily 汇总解析失败结论计数；进程存在即视为可用，
// 首抓无样本时仍输出 HELP / TYPE，样本行只反映真实数据。
//
// 与 blockEventFamily 的边界：本族是「请求解析结果」维度，只回答这次解析的结论；
// 同一次上游故障可能让两族各自加一，但两者不可相加、不可互相推导。
func (e *Exposition) resolveFailureFamily() family {
	f := family{
		name:      resolveFailuresMetric,
		help:      "解析失败累计次数（按结论分区；结论取值是闭集枚举：confirmed / uncertain）",
		kind:      "counter",
		available: true,
	}
	for resolution, count := range e.registry.resolveFailureSnapshot() {
		f.samples = append(f.samples, sample{
			labels: []labelPair{{name: "resolution", value: resolution}},
			value:  strconv.FormatUint(count, 10),
		})
	}
	return f
}

// negativeCacheEntriesFamily 报告当前负缓存有效条目数（无标签）。
// 无标签：仓库名/路径不得作为标签（ARCHITECTURE.md 标签基数约定）。
// 未接线（回调为 nil）时该族不可用，整族不输出——无值可报，不虚构 0。
func (e *Exposition) negativeCacheEntriesFamily() family {
	f := family{
		name:      negativeCacheMetric,
		help:      "当前 404 负缓存的有效条目数（清理过期后统计）",
		kind:      "gauge",
		available: e.negativeCacheEntries != nil,
	}
	if e.negativeCacheEntries == nil {
		return f
	}
	f.samples = []sample{{value: strconv.Itoa(e.negativeCacheEntries())}}
	return f
}

// schedulerFamily 汇总某个调度器作业指标；suffix 为指标名后缀。// 未接入调度器（jobs 为 nil）时该族不可用，整族不输出。
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
