package domain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件为「解析结果可观测」提供三样东西：解析失败结论指标、单次 uncertain 的诊断行、
// 以及把成员级失败原因带出递归层的采样器。
//
// 三条设计约束（改动前务必读懂，破坏其一会让可观测结果失真）：
//
//  1. **不改判定本身**。结论只来自既有的 ResolveFailureStatus（recordResolveFailure /
//     ResolveFailureFromContext）：本文件只读它，不新增第二套「为什么失败」的逻辑。
//  2. **采样点唯一**。recordResolveFailure 会在递归层、group 成员层被多次调用，且语义是
//     「一旦 uncertain 就不被覆盖」。因此指标累加与诊断行只在**顶层解析出口**做一次；
//     绝不在 recordResolveFailure 或成员层里顺手加，否则一次 group 请求会被计成多次。
//  3. **不改返回码、超时、阻断语义**（那些属 FIX-1 / FR-154 / FR-155）。

// 成员失败类别闭集。全部取自既有代码分支的判定结果，不新造分类：
//   - 回源侧：timeout（超时）、gone（410 下架）、upstream_error（其余回源失败）；
//   - 跳过侧：offline（成员离线）、blocked（阻止窗口命中）、member_unknown（成员名解析失败）、
//     not_proxy（成员类型非 proxy，因此不参与回源）；
//   - 兜底：unknown（没有更具体的原因可用）。
const (
	resolveFailureKindTimeout       = "timeout"
	resolveFailureKindGone          = "gone"
	resolveFailureKindUpstreamError = "upstream_error"
	resolveFailureKindOffline       = "offline"
	resolveFailureKindBlocked       = "blocked"
	resolveFailureKindMemberUnknown = "member_unknown"
	resolveFailureKindNotProxy      = "not_proxy"
	resolveFailureKindUnknown       = "unknown"
)

// maxResolveFailureMembers 限制单次诊断携带的成员条数：防止病态配置（成员极多）或
// 循环引用让一次请求在内存在无界累积明细。诊断行只需指出「谁拖垮了结论」，
// 超出上限的成员对定位没有增量价值。
const maxResolveFailureMembers = 8

// maxDiagnosticPathRunes 是诊断行里路径的最大字符数；超长路径截断以免单行日志爆量。
const maxDiagnosticPathRunes = 512

// ResolveFailureRecorder 记录一次解析失败的最终结论，供进程内指标累加。
// 由装配层注入 *metrics.Registry；nil 表示不记录（测试与未接线部署）。
// 与 UpstreamBlockRecorder 同构：domain 定义小接口，不反向依赖 metrics 或 api。
type ResolveFailureRecorder interface {
	// ResolveFailure 累加一次按结论分区的解析失败计数；resolution 必须是闭集枚举取值
	// （confirmed / uncertain），闭集外的取值由实现方忽略。
	ResolveFailure(resolution string)
}

// SetResolveFailureRecorder 注入解析失败结论记录器；nil 表示不记录。
func (s *AssetService) SetResolveFailureRecorder(r ResolveFailureRecorder) { s.failureRecorder = r }

// resolveFailureMember 是一次解析里「拖垮结论」的成员明细。
type resolveFailureMember struct {
	// name 是成员仓库名；顶层为 proxy 客户端本仓库时就是该仓库名。
	name string
	// kind 是成员失败类别，取值见上面的闭集常量。
	kind string
}

// resolveDiagnostics 收集单次请求内各成员的失败原因，供顶层出口拼一条诊断行。
//
// 为什么单独一个 ctx 记录器而不复用 resolveFailureStatus 那个：那个记录器只存一个结论，
// 且是跨层共享的判定载体（协议层也读它）；把诊断明细混进去会让「判定」与「可观测」
// 耦合，且改动它的结构会影响协议层消费点。这里只挂不覆盖、无记录器时为无操作。
type resolveDiagnostics struct {
	mu      sync.Mutex
	members []resolveFailureMember
}

// add 追加一条成员明细（同成员同名同类别不重复追加，受 maxResolveFailureMembers 约束）。
func (d *resolveDiagnostics) add(name, kind string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.members) >= maxResolveFailureMembers {
		return
	}
	for _, m := range d.members {
		if m.name == name && m.kind == kind {
			return
		}
	}
	d.members = append(d.members, resolveFailureMember{name: name, kind: kind})
}

// snapshot 复制一份成员明细，避免拼接日志时持锁。
func (d *resolveDiagnostics) snapshot() []resolveFailureMember {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]resolveFailureMember(nil), d.members...)
}

// resolveDiagnosticsContextKey 是成员明细采样器的 context 键（未导出，避免跨包误用）。
type resolveDiagnosticsContextKey struct{}

// withResolveDiagnostics 在 ctx 上挂载成员明细采样器。
func withResolveDiagnostics(ctx context.Context) (context.Context, *resolveDiagnostics) {
	d := &resolveDiagnostics{}
	return context.WithValue(ctx, resolveDiagnosticsContextKey{}, d), d
}

// recordResolveFailureMember 记录一条成员失败原因；未挂载采样器时为无操作。
// 因此后台任务、直接调用 resolve 的路径都不需要关心本机制。
func recordResolveFailureMember(ctx context.Context, name, kind string) {
	if d, ok := ctx.Value(resolveDiagnosticsContextKey{}).(*resolveDiagnostics); ok {
		d.add(name, kind)
	}
}

// Resolve 顶层出口的观测包装见下面 resolveObserved；这里先给出结算逻辑。

// observeResolveOutcome 在顶层解析出口结算一次**失败的**解析：
// 记一次结论指标，并在结论为 uncertain 时输出一条中文诊断行。成功返回不调用本方法。
//
// 为什么只在这里记指标：见文件头约束 2（采样点唯一）。
func (s *AssetService) observeResolveOutcome(ctx context.Context, diag *resolveDiagnostics, repoName, path string, started time.Time, err error) {
	status := ResolveFailureFromContext(ctx)
	// Unknown 不计样本：没有结论就不是失败结论（避免把内部错误也算进解析失败）。
	if status != ResolveFailureConfirmed && status != ResolveFailureUncertain {
		return
	}
	if s.failureRecorder != nil {
		s.failureRecorder.ResolveFailure(string(status))
	}
	// confirmed 是正常 404 流量（扫描器、Gradle 反复探测缺失子模块），逐条记会刷屏。
	if status != ResolveFailureUncertain {
		return
	}
	s.logUncertainResolve(repoName, path, diag, started, err)
}

// logUncertainResolve 输出一条「解析不可判定」诊断行。一个请求一行，不按成员多行。
//
// 脱敏（NFR-06）：只输出仓库名、路径（控制字符转义 + 截断）、成员名与类别、
// 上游 scheme://host 与耗时；**绝不**输出凭据引用名、Authorization、上游 URL 的
// path/query、用户名或请求头。
func (s *AssetService) logUncertainResolve(repoName, path string, diag *resolveDiagnostics, started time.Time, err error) {
	members := diag.snapshot()
	if len(members) == 0 {
		// 非 group 路径（如 proxy 直连）没有成员层明细：用最终错误类别兜底，
		// 并把本仓库当作唯一「成员」输出，保持诊断行形状一致。
		members = []resolveFailureMember{{name: repoName, kind: classifyResolveFailureKind(err)}}
	}
	// 成员是并行回源的，记录顺序取决于 goroutine 调度。按名称排序让同一故障每次产出
	// 同一条诊断行（可直接 diff、可被测试钉住），也让 upstream_host 的取值确定。
	sort.Slice(members, func(i, j int) bool {
		if members[i].name == members[j].name {
			return members[i].kind < members[j].kind
		}
		return members[i].name < members[j].name
	})
	var b strings.Builder
	b.WriteString("WARN 代理解析不可判定")
	b.WriteString(" repo=" + sanitizeDiagnosticField(repoName))
	b.WriteString(" path=" + SanitizeDiagnosticPath(path))
	b.WriteString(" resolution=" + string(ResolveFailureUncertain))
	b.WriteString(" elapsed_ms=" + strconv.FormatInt(time.Since(started).Milliseconds(), 10))
	for _, m := range members {
		b.WriteString(" member=" + sanitizeDiagnosticField(m.name) + ":" + m.kind)
	}
	if host := s.diagnosticUpstreamHost(repoName, members); host != "" {
		b.WriteString(" upstream_host=" + host)
	}
	log.Print(b.String())
}

// diagnosticUpstreamHost 返回诊断用的上游 host：优先取首个触发成员的，
// 取不到再退到本次解析的仓库本身。只给 scheme://host（见 upstreamHost）。
func (s *AssetService) diagnosticUpstreamHost(repoName string, members []resolveFailureMember) string {
	if s.repos == nil {
		return ""
	}
	for _, m := range members {
		if repo, err := s.repos.GetByName(m.name); err == nil {
			if host := upstreamHost(repo); host != "" {
				return host
			}
		}
	}
	if repo, err := s.repos.GetByName(repoName); err == nil {
		return upstreamHost(repo)
	}
	return ""
}

// upstreamHost 从仓库配置的 RemoteURL 中只取 scheme://host。
//
// 为什么只留 host：RemoteURL 已被校验为无 userinfo / query / fragment（故不含凭据），
// 但 path 段常含团队或项目名，属不必要暴露；而 host 必须保留，否则诊断无法定位到
// 是哪台上游出了问题。userinfo 与 path/query 在解析结果里天然不出现，不依赖裁剪逻辑。
func upstreamHost(repo *repository.Repository) string {
	if repo == nil {
		return ""
	}
	cfg, err := repo.DecodeConfig()
	if err != nil || cfg.RemoteURL == "" {
		return ""
	}
	u, err := url.Parse(cfg.RemoteURL)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + u.Host
}

// classifyResolveFailureKind 把最终错误映射为闭集类别。
// 全部落在既有错误身份上（ErrUpstreamTimeout / ErrUpstreamGone / ErrUpstream / ErrNotFound），
// 不引入新的失败分类。
func classifyResolveFailureKind(err error) string {
	switch {
	case errors.Is(err, ErrUpstreamGone):
		return resolveFailureKindGone
	case errors.Is(err, ErrUpstreamTimeout):
		return resolveFailureKindTimeout
	case errors.Is(err, ErrUpstream):
		return resolveFailureKindUpstreamError
	default:
		return resolveFailureKindUnknown
	}
}

// SanitizeDiagnosticPath 转义路径里的控制字符并截断。
//
// 路径来自客户端，不转义会造就日志注入（伪造诊断行、插入换行改变日志切分）；
// 管理接口回显路径时也走同一个助手，避免日志与响应各写一套裁剪逻辑后走样。
// 除控制字符外的字符原样保留：管理员本就能通过仓库制品列表看到全量路径，
// 诊断里给出路径不构成新增暴露面。
func SanitizeDiagnosticPath(path string) string {
	var b strings.Builder
	count := 0
	for _, r := range path {
		if count >= maxDiagnosticPathRunes {
			b.WriteString("...(已截断)")
			break
		}
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(fmt.Sprintf(`\x%02x`, r))
		default:
			b.WriteRune(r)
		}
		count++
	}
	return b.String()
}

// sanitizeDiagnosticField 转义单行字段值里的控制字符与空白，保证诊断行不被切成多行、
// 也不会因为字段值里的空格伪造出额外的 key=value 片段。
func sanitizeDiagnosticField(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(fmt.Sprintf(`\x%02x`, r))
		case r == ' ':
			b.WriteString(`\x20`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// resolveObserved 是顶层解析出口的观测包装：保证 ctx 上有结论记录器与成员明细采样器，
// 并在失败返回时结算一次指标与诊断行。
//
// 顶层必须**自挂载**记录器：现状只有协议层（maven）会给 ctx 挂 WithResolveFailure，
// 其余调用点读回 Unknown。若这里不补挂，这些路径的失败永远读不到结论，指标系统性漏计。
// 只挂不覆盖：协议层已挂的记录器是判定载体，必须原样复用（maven 的 notFound memo 读的
// 正是它），换一个会让上游判定与指标各看一份。
func (s *AssetService) resolveObserved(ctx context.Context, repoName, path string, resolve func(context.Context) (*repository.Asset, io.ReadCloser, error)) (*repository.Asset, io.ReadCloser, error) {
	if _, ok := ctx.Value(resolveFailureContextKey{}).(*resolveFailureRecorder); !ok {
		ctx = WithResolveFailure(ctx)
	}
	ctx, diag := withResolveDiagnostics(ctx)
	started := time.Now()
	asset, rc, err := resolve(ctx)
	if err != nil {
		s.observeResolveOutcome(ctx, diag, repoName, path, started, err)
	}
	return asset, rc, err
}
