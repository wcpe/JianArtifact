// 本文件实现 proxy 上游的 auto-block 连接状态机（FR-112；FR-43 补强半开语义与指标）。
//
// 借鉴 Nexus BlockingHttpClient：上游失败进入 AUTO_BLOCKED（退避窗口，起始 40s 每档翻倍），
// 窗口内读请求零连接快速失败；窗口到期进入 HALF_OPEN（FR-43）——**每轮只放行一个探测**，
// 其余请求在结论返回前等效封锁（快速失败）；探测成功自动恢复 AVAILABLE，失败推进一档。
// 取代 FR-110 的固定 30s TTL proxyFuse：阻止窗口递增、恢复无需等到窗口结束（探测成功即恢复）。
package domain

import (
	"context"
	"sync"
	"time"
)

// RemoteStatus 表示 proxy 仓库上游的连接状态（供 FR-114 状态展示）。
type RemoteStatus string

const (
	// StatusReady 初始状态：尚未发生任何失败。
	StatusReady RemoteStatus = "READY"
	// StatusAvailable 上游可用（回源/探测成功）。
	StatusAvailable RemoteStatus = "AVAILABLE"
	// StatusUnavailable 上游不可用（保留状态，当前状态机未直接产出）。
	StatusUnavailable RemoteStatus = "UNAVAILABLE"
	// StatusAutoBlocked 上游失败后处于自动阻止窗口。
	StatusAutoBlocked RemoteStatus = "AUTO_BLOCKED"
	// StatusHalfOpen 阻止窗口到期、正在探测上游（FR-43 §2.3）。
	// 该状态下**每轮只放行一个探测请求**，其余请求在结论返回前等效封锁（快速失败）；
	// 探测成功回到 AVAILABLE 并重置退避，失败回到 AUTO_BLOCKED 并推进一档。
	StatusHalfOpen RemoteStatus = "HALF_OPEN"
	// StatusOffline 手动离线（保留状态，本任务未接入手动管理）。
	StatusOffline RemoteStatus = "OFFLINE"
	// StatusBlocked 手动阻止（保留状态，本任务未接入手动管理）。
	StatusBlocked RemoteStatus = "BLOCKED"
)

// autoBlockInitial 是 auto-block 退避起始时长的**兜底默认值**：首次失败窗口 40s，
// 之后每档翻倍（40s→80s→160s…）。
// 生产装配经 SetAutoBlockBase 注入 JIAN_AUTO_BLOCK_BASE_SECONDS 的解析结果；
// 本常量只在未注入时兜底（测试构造与未接线的旧路径），因此必须与 config 的
// defaultAutoBlockBase 保持同值——改一处必须同步改另一处。
const autoBlockInitial = 40 * time.Second

// probeTimeout 是单次上游 HEAD 探测的超时；后台线程等待业务探测结论也以它为上限。
const probeTimeout = 5 * time.Second

// UpstreamBlockReason 是上游断路器阻止事件的**闭集枚举**取值（FR-43 §3.3）。
// 该集合与 docs/specs/0.12.0-upstream-circuit-breaker.md 的 reason 标签取值一一对应：
// **新增取值必须先改规格并同步测试与 OPERATIONS.md**，不得在实现期就地塞入自由字符串
// （自由字符串会把无界值带进指标标签，违反 ARCHITECTURE.md 的标签基数受控约定）。
type UpstreamBlockReason string

const (
	// UpstreamBlockReasonProbeFailed 探测失败导致阻止窗口延长。
	UpstreamBlockReasonProbeFailed UpstreamBlockReason = "probe_failed"
	// UpstreamBlockReasonUpstreamError 回源失败触发（或延长）自动阻止。
	UpstreamBlockReasonUpstreamError UpstreamBlockReason = "upstream_error"
)

// UpstreamBlockRecorder 记录一次上游断路器阻止事件，供进程内指标累加（FR-43 §2.2）。
// 由装配层注入 *metrics.Registry；nil 表示不记录（测试与未接线部署）。
type UpstreamBlockRecorder interface {
	// UpstreamBlockEvent 累加一次按原因分区的阻止事件计数；reason 必须是闭集枚举取值。
	UpstreamBlockEvent(reason string)
}

// RemoteHealth 是单个 proxy 仓库上游连接状态的对外视图（FR-114 状态展示用）。
type RemoteHealth struct {
	Status       RemoteStatus  // 当前连接状态
	BlockedUntil time.Time     // auto-block 窗口截止时间（非阻止状态为零值）
	BlockedFor   time.Duration // 当前阻止窗口时长（退避档位；非阻止状态为 0）
}

// proxyHealth 管理各 proxy 仓库上游的连接状态机（按仓库 ID 区分）。
// 每个仓库独立维护状态：失败进入 AUTO_BLOCKED 并开启退避窗口，窗口内 shouldBlock
// 返回 true（读请求零连接快速失败）；窗口到期进入 HALF_OPEN（每轮只放行一个探测）；
// 探测成功恢复 AVAILABLE 并重置退避。内存态、并发安全；探测线程生命周期由状态机统一管理。
type proxyHealth struct {
	mu       sync.Mutex
	check    func(ctx context.Context, remoteURL, credentialRef string) error // 上游连通性探测（HEAD），由 AssetService 注入
	base     time.Duration                                                    // 退避起始时长（默认 autoBlockInitial；装配层与测试可调）
	recorder UpstreamBlockRecorder                                            // 阻止事件记录器；nil 表示不记录（FR-43）
	repos    map[int64]*remoteState                                           // repoID -> 该仓库上游状态
}

// remoteState 是单个 proxy 仓库上游的连接状态。
//
// 半开轮次（FR-43 §2.3）的两个关键字段：
//   - probeInFlight：本轮半开的探测名额是否已被占用。**判定与置位必须在同一把锁内**
//     完成（见 beginUpstream），否则并发请求会同时通过闸门；
//   - roundDone：本轮结论通道，结论落定即关闭。业务请求抢到名额时，后台线程改等它，
//     从而保证「每轮只有一个探测」，也不会出现同轮两个失败结论各推进一档退避。
type remoteState struct {
	state         RemoteStatus
	blockedUntil  time.Time
	curBlock      time.Duration // 当前阻止窗口时长（退避档位）
	autoBlockSeq  time.Duration // 下一档退避时长（0 表示未开始）
	stopCh        chan struct{} // 探测线程中断信号；非 nil 表示探测线程在运行
	probeInFlight bool          // 本轮半开探测名额是否已被占用（闸门）
	roundDone     chan struct{} // 本轮结论通道；进入半开时新建，结论落定时关闭
}

// newProxyHealth 构造状态机容器。check 为探测函数，由 AssetService 注入。
func newProxyHealth(check func(ctx context.Context, remoteURL, credentialRef string) error) *proxyHealth {
	return &proxyHealth{check: check, base: autoBlockInitial, repos: map[int64]*remoteState{}}
}

// setBase 调整退避起始时长（装配层注入配置、测试缩短窗口用；<=0 忽略）。
func (h *proxyHealth) setBase(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d > 0 {
		h.base = d
	}
}

// setBlockRecorder 注入阻止事件记录器（FR-43 §2.2）；nil 表示不记录。
func (h *proxyHealth) setBlockRecorder(r UpstreamBlockRecorder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recorder = r
}

// recordBlockEvent 累加一次阻止事件计数；未接线时静默忽略。
// 刻意在**不持 h.mu** 的情况下调用：记录器自带锁，避免与指标渲染路径形成锁序纠缠。
func (h *proxyHealth) recordBlockEvent(reason UpstreamBlockReason) {
	h.mu.Lock()
	recorder := h.recorder
	h.mu.Unlock()
	if recorder != nil {
		recorder.UpstreamBlockEvent(string(reason))
	}
}

// nextBlock 返回本次阻止窗口时长（当前档位）并推进序列（起始 base，每档翻倍）。
// 需在持锁下调用。
func (h *proxyHealth) nextBlock(st *remoteState) time.Duration {
	if st.autoBlockSeq == 0 {
		st.autoBlockSeq = h.base * 2 // 预置下一档
		return h.base
	}
	cur := st.autoBlockSeq
	st.autoBlockSeq = cur * 2
	return cur
}

// enterHalfOpen 进入一轮新的半开：状态置 HALF_OPEN，名额清空，新建本轮结论通道。
// **只在当前不是 HALF_OPEN 时调用**：重复调用会把已在飞行的探测名额与结论通道重置，
// 闸门随之失效、结论也无从观察。需在持锁下调用。
func (st *remoteState) enterHalfOpen() {
	st.settleRound() // 上一轮若有等待者，先行唤醒，避免其滞留到超时才重判
	st.state = StatusHalfOpen
	st.probeInFlight = false
	st.roundDone = make(chan struct{})
}

// settleRound 关闭本轮结论通道（幂等）；需在持锁下调用。
func (st *remoteState) settleRound() {
	if st.roundDone != nil {
		close(st.roundDone)
		st.roundDone = nil
	}
}

// reset 恢复可用状态：状态置 AVAILABLE，清空窗口与退避序列，释放名额并收口本轮。
// 需在持锁下调用。
func (st *remoteState) reset() {
	st.state = StatusAvailable
	st.blockedUntil = time.Time{}
	st.curBlock = 0
	st.autoBlockSeq = 0
	st.probeInFlight = false
	st.settleRound()
}

// advanceBlock 推进一档退避并回到 AUTO_BLOCKED：窗口从当前时刻重新计时，释放半开名额
// 并收口本轮（下一轮半开重新抢名额、重新建结论通道）。需在持锁下调用。
func (h *proxyHealth) advanceBlock(st *remoteState) {
	st.state = StatusAutoBlocked
	st.curBlock = h.nextBlock(st)
	st.blockedUntil = time.Now().Add(st.curBlock)
	st.probeInFlight = false
	st.settleRound()
}

// shouldBlock 报告仓库 id 当前是否应拒绝回源（读请求零连接快速失败）。
// 这是**非占用式**判定：用于外层过滤（proxy 读入口、group 成员预筛），
// 不消耗 HALF_OPEN 的探测名额——真正消耗名额的是 beginUpstream。
//
// 判据（FR-43 §2.3 扩展后）：
//   - READY / AVAILABLE 等非阻止态 → 放行；
//   - AUTO_BLOCKED 且窗口未到 → 封锁；
//   - AUTO_BLOCKED 且窗口已到（后台线程尚未切到 HALF_OPEN 的窄窗口）→ 放行到回源点，
//     由 beginUpstream 在同一把锁内抢名额，因此不会退化成「窗口一到就全放行」；
//   - HALF_OPEN 且名额已被占用 → 封锁（等效快速失败）。
func (h *proxyHealth) shouldBlock(repoID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.repos[repoID]
	if st == nil {
		return false
	}
	switch st.state {
	case StatusAutoBlocked:
		return time.Now().Before(st.blockedUntil)
	case StatusHalfOpen:
		return st.probeInFlight
	default:
		return false
	}
}

// beginUpstream 是**占用式**闸门：判定与置位在同一把锁内完成，返回 true 表示本次调用
// 获准发起上游连接（调用方随即真正回源），false 表示应快速失败（ErrUpstream）。
//
// 这是「每轮半开只放行一个探测请求」的落点，也是本任务的核心修复：旧的 shouldBlock
// 只看 blockedUntil，窗口一到所有真实业务流量无条件放行；现在窗口到期只放行**一个**
// 探测请求，其余请求在结论返回前一律返回 false（等效封锁、快速失败）。
//
// 名额在结论落定时释放：成功走 reset（→AVAILABLE）、失败走 advanceBlock（→AUTO_BLOCKED），
// 两者都释放名额并收口本轮；「上游可达但资源缺失」（404/410）走 releaseProbe 单独释放名额。
func (h *proxyHealth) beginUpstream(repoID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.repos[repoID]
	if st == nil {
		return true // 从未失败过：直通
	}
	switch st.state {
	case StatusAutoBlocked:
		if time.Now().Before(st.blockedUntil) {
			return false // 窗口内：零连接快速失败
		}
		// 窗口已到但后台线程尚未切态：视同半开，并顺带把状态切到 HALF_OPEN，
		// 让对外状态与实际闸门行为一致（不再把半开期误报为 AUTO_BLOCKED）。
		st.enterHalfOpen()
	case StatusHalfOpen:
		// 已处于半开：继续走下面的名额抢占。
	default:
		return true // READY / AVAILABLE：直通
	}
	if st.probeInFlight {
		return false // 本轮名额已被其它请求占用：等效于封锁
	}
	st.probeInFlight = true
	return true
}

// releaseProbe 释放 HALF_OPEN 的探测名额并收口本轮，但**不改动状态与退避窗口**。
// 供「上游可达但资源缺失」的收口路径调用（404/410：现有语义不据此改状态，
// 以免把上游可达误报成可用或不可用），避免名额被占住让后续请求整轮被无限封锁。
// 对非半开路径（roundDone 为空）是幂等空操作。
func (h *proxyHealth) releaseProbe(repoID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st := h.repos[repoID]; st != nil {
		st.probeInFlight = false
		st.settleRound()
	}
}

// blockedCount 返回当前处于阻止态（AUTO_BLOCKED 或 HALF_OPEN）的仓库数（FR-43 §2.2）。
// 按状态而非窗口判定：窗口已到但尚未切态的仓库仍算「阻止中」，与对外状态一致。
func (h *proxyHealth) blockedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, st := range h.repos {
		if st.state == StatusAutoBlocked || st.state == StatusHalfOpen {
			count++
		}
	}
	return count
}

// recordFailure 记录一次回源失败：推进一档退避回到 AUTO_BLOCKED（窗口从当前时刻重新计时），
// 按 reason 累加阻止事件计数，并确保后台探测线程在运行（首次失败时启动）。
func (h *proxyHealth) recordFailure(repoID int64, remoteURL, credentialRef string, reason UpstreamBlockReason) {
	h.mu.Lock()
	st := h.repos[repoID]
	if st == nil {
		st = &remoteState{}
		h.repos[repoID] = st
	}
	h.advanceBlock(st)
	needStart := st.stopCh == nil
	if needStart {
		st.stopCh = make(chan struct{})
	}
	h.mu.Unlock()
	h.recordBlockEvent(reason)
	if needStart {
		go h.checkStatus(st, remoteURL, credentialRef)
	}
}

// recordSuccess 记录一次上游成功：恢复 AVAILABLE，重置退避并释放名额、收口本轮，
// 同时中断探测线程。成功恒为正确结论，不受轮次约束。
// 线程随后经 stopCh 感知中断退出；若线程正在探测，探测完成后也会按状态退出。
func (h *proxyHealth) recordSuccess(repoID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.repos[repoID]
	if st == nil {
		return
	}
	st.reset()
	if st.stopCh != nil {
		close(st.stopCh)
		st.stopCh = nil
	}
}

// status 返回仓库 id 的上游连接状态视图（供 FR-114 展示）。
func (h *proxyHealth) status(repoID int64) RemoteHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.repos[repoID]
	if st == nil {
		return RemoteHealth{Status: StatusReady}
	}
	return RemoteHealth{Status: st.state, BlockedUntil: st.blockedUntil, BlockedFor: st.curBlock}
}

// recheck 手动立即探测上游并更新状态（FR-114 手动重测）：成功恢复 AVAILABLE 并中断
// 后台探测线程；失败推进一档退避并回到 AUTO_BLOCKED（同 recordFailure 语义，线程由
// 状态机保证在跑）。返回最新状态视图，不等待自动阻止窗口。
// 手动重测是**同步探测**，不等窗口、也不占用 HALF_OPEN 的业务探测名额。
func (h *proxyHealth) recheck(repoID int64, remoteURL, credentialRef string) RemoteHealth {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if err := h.check(ctx, remoteURL, credentialRef); err != nil {
		h.recordFailure(repoID, remoteURL, credentialRef, UpstreamBlockReasonProbeFailed)
	} else {
		h.recordSuccess(repoID)
	}
	return h.status(repoID)
}

// checkStatus 是后台探测线程体：等待到 blockedUntil，到点先切 HALF_OPEN（对外可见
// 「正在探测上游」）再取名额探测；成功恢复 AVAILABLE 并重置退避（线程退出），
// 失败推进一档退避并回到 AUTO_BLOCKED 继续等待。
//
// 不变量：AUTO_BLOCKED 与 HALF_OPEN ⇒ 必有探测线程在跑。线程生命周期与 stopCh 绑定：
// recordSuccess 关闭 stopCh 即中断；线程退出时自行关闭并清空 stopCh（锁内）。
// 业务与后台共用同一个探测名额，故每轮半开**只有一个**上游探测在飞。
func (h *proxyHealth) checkStatus(st *remoteState, remoteURL, credentialRef string) {
	for {
		// 等待到窗口截止（不持锁，允许 recordSuccess 中断）。
		h.mu.Lock()
		wait := time.Until(st.blockedUntil)
		stop := st.stopCh
		h.mu.Unlock()
		if stop == nil {
			return // stopCh 已被 recordSuccess 清除，线程应退出
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-stop:
				timer.Stop()
				return // 被中断（如已恢复成功）
			}
			continue // 重新评估截止时间（窗口可能已被顺延）
		}

		// 已到窗口截止：先切到 HALF_OPEN（仅当尚不在半开时），使对外状态与闸门行为一致。
		h.mu.Lock()
		if st.stopCh == nil {
			h.mu.Unlock()
			return // 等待期间已被恢复并中断
		}
		if st.state != StatusHalfOpen {
			st.enterHalfOpen()
		}
		if st.probeInFlight {
			// 名额已被业务请求抢走：它就是本轮的唯一探测，改等它的结论，
			// 不再并发多发一次探测（否则同一轮出现两个失败结论，退避会被推进两档）。
			// 以 probeTimeout 为上限等待：业务探测若异常挂死，本线程也不会永久失联。
			roundDone, stop := st.roundDone, st.stopCh
			h.mu.Unlock()
			if roundDone == nil {
				continue // 本轮已收口：立即重判（新轮次或已回到等待）
			}
			timer := time.NewTimer(probeTimeout)
			select {
			case <-roundDone:
			case <-stop:
			case <-timer.C:
			}
			timer.Stop()
			continue
		}
		st.probeInFlight = true // 后台线程自己占用本轮名额
		h.mu.Unlock()

		// 探测上游（不持锁，探测可能耗时）。
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		err := h.check(ctx, remoteURL, credentialRef)
		cancel()

		h.mu.Lock()
		if st.stopCh == nil {
			h.mu.Unlock()
			return // 探测期间已被 recordSuccess 恢复：不再改写状态
		}
		if err != nil {
			// 探测失败：上游仍不可用，推进一档后回到 AUTO_BLOCKED 继续等待。
			h.advanceBlock(st)
			h.mu.Unlock()
			h.recordBlockEvent(UpstreamBlockReasonProbeFailed)
			continue
		}
		// 探测成功：恢复 AVAILABLE 并退出线程。线程退出时自 close stopCh 并置 nil，
		// 后续若再有失败，recordFailure 会重新启动探测线程（不变量：AUTO_BLOCKED ⇒ 必有线程）。
		st.reset()
		if st.stopCh != nil {
			close(st.stopCh)
			st.stopCh = nil
		}
		h.mu.Unlock()
		return
	}
}
