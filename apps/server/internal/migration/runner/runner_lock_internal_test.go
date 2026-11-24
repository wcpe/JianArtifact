package runner

import (
	"testing"
	"time"
)

// TestAcquireTaskLockWaitsForTailWindow 钉住收尾窗口语义：
// 锁已持有时 acquire 在预算内等待释放；预算耗尽仍持锁则放弃（真执行中语义）。
func TestAcquireTaskLockWaitsForTailWindow(t *testing.T) {
	r := &Runner{}
	const taskID = 7

	// 场景一：锁在窗口内释放 → 拿到锁。
	lk := r.taskLock(taskID)
	lk.Lock()
	acquired := make(chan bool, 1)
	go func() { acquired <- r.acquireTaskLock(taskID, 2*time.Second, time.Millisecond) }()
	time.Sleep(50 * time.Millisecond)
	lk.Unlock()
	if !<-acquired {
		t.Fatal("窗口内释放的锁应被等到")
	}
	lk.Unlock() // acquire 成功后锁在调用方手里，必须归还

	// 场景二：锁始终被持有 → 预算耗尽返回 false（执行中拒绝语义不变）。
	lk.Lock()
	start := time.Now()
	if r.acquireTaskLock(taskID, 80*time.Millisecond, 10*time.Millisecond) {
		lk.Unlock()
		t.Fatal("持续持锁应判定拿不到锁")
	}
	lk.Unlock()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("等锁超出预算太多：%v", elapsed)
	}
}
