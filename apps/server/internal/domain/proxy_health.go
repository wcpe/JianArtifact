// 本文件实现 proxy 上游的 auto-block 连接状态机（FR-112）。
//
// 借鉴 Nexus BlockingHttpClient：上游失败进入 AUTO_BLOCKED（退避窗口，起始 40s 每档翻倍），
// 窗口内读请求零连接快速失败；后台线程到点 HEAD 探测上游，成功自动恢复 AVAILABLE。
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
	// StatusOffline 手动离线（保留状态，本任务未接入手动管理）。
	StatusOffline RemoteStatus = "OFFLINE"
	// StatusBlocked 手动阻止（保留状态，本任务未接入手动管理）。
	StatusBlocked RemoteStatus = "BLOCKED"
)

// autoBlockInitial 是 auto-block 退避起始时长：首次失败窗口 40s，之后每档翻倍（40s→80s→160s…）。
const autoBlockInitial = 40 * time.Second

// probeTimeout 是后台 HEAD 探测单次超时。
const probeTimeout = 5 * time.Second

// RemoteHealth 是单个 proxy 仓库上游连接状态的对外视图（FR-114 状态展示用）。
type RemoteHealth struct {
	Status       RemoteStatus  // 当前连接状态
	BlockedUntil time.Time     // auto-block 窗口截止时间（非阻止状态为零值）
	BlockedFor   time.Duration // 当前阻止窗口时长（退避档位；非阻止状态为 0）
}

// proxyHealth 管理各 proxy 仓库上游的连接状态机（按仓库 ID 区分）。
// 每个仓库独立维护状态：失败进入 AUTO_BLOCKED 并开启退避窗口，窗口内 shouldBlock
// 返回 true（读请求零连接快速失败）；后台探测线程到点 HEAD 探测上游，成功恢复
// AVAILABLE 并重置退避。内存态、并发安全；探测线程生命周期由状态机统一管理。
type proxyHealth struct {
	mu    sync.Mutex
	check func(ctx context.Context, remoteURL, credentialRef string) error // 上游连通性探测（HEAD），由 AssetService 注入
	base  time.Duration                                                    // 退避起始时长（默认 autoBlockInitial，测试可调小）
	repos map[int64]*remoteState                                           // repoID -> 该仓库上游状态
}

// remoteState 是单个 proxy 仓库上游的连接状态。
type remoteState struct {
	state        RemoteStatus
	blockedUntil time.Time
	curBlock     time.Duration // 当前阻止窗口时长（退避档位）
	autoBlockSeq time.Duration // 下一档退避时长（0 表示未开始）
	stopCh       chan struct{} // 探测线程中断信号；非 nil 表示探测线程在运行
}

// newProxyHealth 构造状态机容器。check 为探测函数，由 AssetService 注入。
func newProxyHealth(check func(ctx context.Context, remoteURL, credentialRef string) error) *proxyHealth {
	return &proxyHealth{check: check, base: autoBlockInitial, repos: map[int64]*remoteState{}}
}

// setBase 调整退避起始时长（测试与调优用；<=0 忽略）。
func (h *proxyHealth) setBase(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d > 0 {
		h.base = d
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

// reset 恢复可用状态：状态置 AVAILABLE，清空窗口与退避序列。需在持锁下调用。
func (st *remoteState) reset() {
	st.state = StatusAvailable
	st.blockedUntil = time.Time{}
	st.curBlock = 0
	st.autoBlockSeq = 0
}

// shouldBlock 报告仓库 id 是否处于阻止窗口内（窗口内读请求零连接快速失败）。
func (h *proxyHealth) shouldBlock(repoID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.repos[repoID]
	return st != nil && st.state == StatusAutoBlocked && time.Now().Before(st.blockedUntil)
}

// recordFailure 记录一次上游失败：进入 AUTO_BLOCKED，blockedUntil = now + 下一退避档，
// 并确保后台探测线程在运行（首次失败时启动）。
func (h *proxyHealth) recordFailure(repoID int64, remoteURL, credentialRef string) {
	h.mu.Lock()
	st := h.repos[repoID]
	if st == nil {
		st = &remoteState{}
		h.repos[repoID] = st
	}
	st.state = StatusAutoBlocked
	st.curBlock = h.nextBlock(st)
	st.blockedUntil = time.Now().Add(st.curBlock)
	needStart := st.stopCh == nil
	if needStart {
		st.stopCh = make(chan struct{})
	}
	h.mu.Unlock()
	if needStart {
		go h.checkStatus(st, remoteURL, credentialRef)
	}
}

// recordSuccess 记录一次上游成功：恢复 AVAILABLE，重置退避，并中断探测线程。
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
// 后台探测线程；失败进入 AUTO_BLOCKED 并开启退避窗口（同 recordFailure 语义，线程由
// 状态机保证在跑）。返回最新状态视图，不等待自动阻止窗口。
func (h *proxyHealth) recheck(repoID int64, remoteURL, credentialRef string) RemoteHealth {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if err := h.check(ctx, remoteURL, credentialRef); err != nil {
		h.recordFailure(repoID, remoteURL, credentialRef)
	} else {
		h.recordSuccess(repoID)
	}
	return h.status(repoID)
}

// checkStatus 是后台探测线程体：等待到 blockedUntil，到点 HEAD 探测上游；
// 成功恢复 AVAILABLE 并重置退避（线程退出）；失败延长窗口继续等待。
// 线程生命周期与 stopCh 绑定：recordSuccess 关闭 stopCh 即中断；线程退出时
// 自行关闭并清空 stopCh（锁内），保证不变量"AUTO_BLOCKED ⇒ 必有探测线程在跑"。
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

		// 已到窗口截止：探测上游（不持锁，探测可能耗时）。
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		err := h.check(ctx, remoteURL, credentialRef)
		cancel()

		h.mu.Lock()
		if err != nil {
			// 探测失败：上游仍不可用，延长窗口继续等待。
			st.curBlock = h.nextBlock(st)
			st.blockedUntil = time.Now().Add(st.curBlock)
			h.mu.Unlock()
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
