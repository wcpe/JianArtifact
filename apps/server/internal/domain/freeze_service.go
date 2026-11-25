package domain

import (
	"encoding/json"
	"errors"
	"log"
	"os"
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

	// persistPath 非空时冻结状态落盘（原子写：临时文件 + rename），进程重启后回读。
	// 搬迁窗口跨重启丢冻结等于窗口期静默放写，持久化是「停写」语义成立的前提。
	persistPath string

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

// persistedFreezeState 是冻结状态的落盘形制。字段与 FreezeState 对齐，
// 独立定义避免把 API 表述层结构耦进存储契约。
type persistedFreezeState struct {
	Frozen   bool      `json:"frozen"`
	FrozenAt time.Time `json:"frozenAt"`
	Until    time.Time `json:"until"`
	Reason   string    `json:"reason"`
}

// NewFreezeControllerWithPersistence 构造带落盘的冻结控制器：
// 启动时从 persistPath 回读状态（文件不存在即全新未冻结），此后每次 Freeze/Unfreeze 原子落盘。
// 回读失败（损坏/不可解析）按「冻结且需手动解冻」兜底——宁可误拒写（管理员一次解冻即恢复），
// 不可在搬迁窗口期静默放写；这是冻结语义的安全失败方向。
func NewFreezeControllerWithPersistence(base BusinessWriteGate, persistPath string) *FreezeController {
	c := &FreezeController{base: base, persistPath: persistPath}
	raw, err := os.ReadFile(persistPath)
	if errors.Is(err, os.ErrNotExist) {
		return c
	}
	if err != nil {
		log.Printf("冻结状态回读失败（%v），按手动冻结兜底", err)
		c.frozen = true
		c.reason = "冻结状态文件读取失败，按安全失败方向兜底"
		return c
	}
	var st persistedFreezeState
	if jsonErr := json.Unmarshal(raw, &st); jsonErr != nil {
		log.Printf("冻结状态文件损坏（%v），按手动冻结兜底", jsonErr)
		c.frozen = true
		c.reason = "冻结状态文件损坏，按安全失败方向兜底"
		return c
	}
	c.frozen = st.Frozen
	c.frozenAt = st.FrozenAt
	c.until = st.Until
	c.reason = st.Reason
	return c
}

// persistLocked 把当前状态原子落盘。调用方必须已持写锁。
// 失败只记日志不影响内存态生效：本进程内冻结语义依旧成立，
// 降级的是「重启后记得」——比拒绝冻结操作本身更可取（运维多数场景不重启）。
func (c *FreezeController) persistLocked() {
	if c.persistPath == "" {
		return
	}
	raw, err := json.Marshal(persistedFreezeState{
		Frozen:   c.frozen,
		FrozenAt: c.frozenAt,
		Until:    c.until,
		Reason:   c.reason,
	})
	if err != nil {
		log.Printf("冻结状态序列化失败：%v", err)
		return
	}
	tmp := c.persistPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("冻结状态落盘失败：%v", err)
		return
	}
	if err := os.Rename(tmp, c.persistPath); err != nil {
		log.Printf("冻结状态落盘改名失败：%v", err)
	}
	// 清理改名失败残留的临时文件，避免目录里越积越多。
	_ = os.Remove(tmp)
}

// Freeze 进入冻结窗口。until 为零值表示手动冻结、不自动解冻；否则到点自动解冻。
func (c *FreezeController) Freeze(until time.Time, reason string) FreezeState {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frozen = true
	c.frozenAt = time.Now()
	c.until = until
	c.reason = reason
	c.persistLocked()
	return c.snapshotLocked()
}

// Unfreeze 退出冻结窗口。不影响底层 base 的静态只读策略。
func (c *FreezeController) Unfreeze() FreezeState {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frozen = false
	c.until = time.Time{}
	c.reason = ""
	c.persistLocked()
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
