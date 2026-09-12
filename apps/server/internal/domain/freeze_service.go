package domain

import (
	"errors"
	"sync"
	"time"
)

// ErrWriteFrozen 表示写入已冻结（搬迁冻结窗口期间拒绝本地业务写）。
// 类型化以便 HTTP 层按错误码映射为 write_frozen。
var ErrWriteFrozen = errors.New("写入已冻结")

// FreezeState 快照冻结窗口的当前状态。
type FreezeState struct {
	Frozen   bool
	Until    time.Time // 零值表示手动冻结、不自动解冻
	FrozenAt time.Time
	Reason   string
}

// FreezeController 在静态角色只读栅栏之上叠加运行时冻结窗口：可手动冻结、可解冻、
// 可设定超时自动解冻（防遗忘导致服务假死——这类开关最危险的失败模式）。
// 整个进程共享一份实例，HTTP 中间件与领域服务必须观察同一实例，否则状态不一致。
//
// 并发安全：会被多请求并发调用（Freeze/Unfreeze/State/RequireBusinessWrite）。
type FreezeController struct {
	base BusinessWriteGate // 底层静态角色只读栅栏（可为 nil）

	mu       sync.RWMutex
	frozen   bool
	frozenAt time.Time
	until    time.Time
	reason   string
}

// NewFreezeController 构造冻结控制器；base 为底层静态写栅栏，可为 nil（单节点兼容）。
func NewFreezeController(base BusinessWriteGate) *FreezeController {
	return &FreezeController{base: base}
}

// Freeze 进入冻结窗口。until 为零值表示手动冻结、不自动解冻；否则到点自动解冻。
func (c *FreezeController) Freeze(until time.Time, reason string) FreezeState {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frozen = true
	c.frozenAt = time.Now()
	c.until = until
	c.reason = reason
	return c.snapshotLocked()
}

// Unfreeze 退出冻结窗口。不影响底层 base 的静态只读策略。
func (c *FreezeController) Unfreeze() FreezeState {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frozen = false
	c.until = time.Time{}
	c.reason = ""
	return c.snapshotLocked()
}

// State 返回当前冻结状态。超时（now >= until）即视为未冻结，实现自动解冻兜底。
func (c *FreezeController) State() FreezeState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshotLocked()
}

func (c *FreezeController) snapshotLocked() FreezeState {
	st := FreezeState{FrozenAt: c.frozenAt, Until: c.until, Reason: c.reason}
	if !c.frozen {
		return st
	}
	// 超时即视为未冻结：忘记解冻时写入自动恢复，避免服务假死。
	if !c.until.IsZero() && time.Now().After(c.until) {
		st.Frozen = false
		return st
	}
	st.Frozen = true
	return st
}

// RequireBusinessWrite 实现 BusinessWriteGate：先看冻结（冻结/已超时为未冻结），
// 冻结中返回 ErrWriteFrozen；再委托 base。两条件取或——解冻不会解开 base 的静态只读。
func (c *FreezeController) RequireBusinessWrite() error {
	if c.State().Frozen {
		return ErrWriteFrozen
	}
	if c.base != nil {
		return c.base.RequireBusinessWrite()
	}
	return nil
}
