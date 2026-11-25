package domain

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func TestFreezeInitialNotFrozen(t *testing.T) {
	t.Parallel()
	c := NewFreezeController(nil)
	if c.State().Frozen {
		t.Fatal("初始应未冻结")
	}
	if err := c.RequireBusinessWrite(); err != nil {
		t.Fatalf("初始 RequireBusinessWrite 应成功，得 %v", err)
	}
}

func TestFreezeBlocksWrite(t *testing.T) {
	t.Parallel()
	c := NewFreezeController(nil)
	c.Freeze(time.Time{}, "relocation")
	if err := c.RequireBusinessWrite(); !errors.Is(err, ErrWriteFrozen) {
		t.Fatalf("冻结后应返回 ErrWriteFrozen，得 %v", err)
	}
	if !c.State().Frozen {
		t.Fatal("冻结后 State 应 Frozen=true")
	}
}

func TestUnfreezeRestoresWrite(t *testing.T) {
	t.Parallel()
	c := NewFreezeController(nil)
	c.Freeze(time.Time{}, "relocation")
	c.Unfreeze()
	if err := c.RequireBusinessWrite(); err != nil {
		t.Fatalf("解冻后 RequireBusinessWrite 应成功，得 %v", err)
	}
	if c.State().Frozen {
		t.Fatal("解冻后 State 应 Frozen=false")
	}
}

func TestFreezeAutoExpiresPast(t *testing.T) {
	t.Parallel()
	c := NewFreezeController(nil)
	c.Freeze(time.Now().Add(-time.Second), "relocation")
	if c.State().Frozen {
		t.Fatal("已超时的冻结应视为未冻结")
	}
	if err := c.RequireBusinessWrite(); err != nil {
		t.Fatalf("超时冻结下写入应放行，得 %v", err)
	}
}

func TestFreezeAutoExpiresNear(t *testing.T) {
	t.Parallel()
	c := NewFreezeController(nil)
	c.Freeze(time.Now().Add(time.Millisecond), "relocation")
	time.Sleep(5 * time.Millisecond)
	if c.State().Frozen {
		t.Fatal("极近超时后应为未冻结")
	}
	if err := c.RequireBusinessWrite(); err != nil {
		t.Fatalf("超时冻结下写入应放行，得 %v", err)
	}
}

func TestFreezeConcurrency(t *testing.T) {
	t.Parallel()
	c := NewFreezeController(nil)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Freeze(time.Time{}, "relocation")
			_ = c.State()
			_ = c.RequireBusinessWrite()
			c.Unfreeze()
			_ = c.State()
		}()
	}
	wg.Wait()
	if c.State().Frozen {
		t.Fatal("并发压测后应为未冻结态")
	}
}

func TestFreezePersistenceRoundTrip(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/write-freeze.json"
	c1 := NewFreezeControllerWithPersistence(nil, path)
	until := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	c1.Freeze(until, "搬迁窗口")
	// 模拟进程重启：同路径新实例应回读到冻结态（核心回归：重启不再静默放写）。
	c2 := NewFreezeControllerWithPersistence(nil, path)
	st := c2.State()
	if !st.Frozen {
		t.Fatal("重启后应回读到冻结态")
	}
	if st.Reason != "搬迁窗口" {
		t.Fatalf("回读 reason 不符：%q", st.Reason)
	}
	if err := c2.RequireBusinessWrite(); !errors.Is(err, ErrWriteFrozen) {
		t.Fatalf("回读冻结后 RequireBusinessWrite 应返回 ErrWriteFrozen，得 %v", err)
	}
}

func TestFreezePersistenceUnfreezePersists(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/write-freeze.json"
	c1 := NewFreezeControllerWithPersistence(nil, path)
	c1.Freeze(time.Time{}, "搬迁窗口")
	c1.Unfreeze()
	c2 := NewFreezeControllerWithPersistence(nil, path)
	if c2.State().Frozen {
		t.Fatal("解冻落盘后重启实例不应再冻结")
	}
}

func TestFreezePersistenceMissingFileIsFresh(t *testing.T) {
	t.Parallel()
	c := NewFreezeControllerWithPersistence(nil, t.TempDir()+"/nonexistent.json")
	if c.State().Frozen {
		t.Fatal("状态文件不存在时应全新未冻结")
	}
}

func TestFreezePersistenceCorruptFileFailsClosed(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/write-freeze.json"
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewFreezeControllerWithPersistence(nil, path)
	// 损坏按安全失败方向兜底：冻结且需手动解冻——宁可误拒写，不在搬迁窗口静默放写。
	if !c.State().Frozen {
		t.Fatal("状态文件损坏时应按冻结兜底")
	}
	if err := c.RequireBusinessWrite(); !errors.Is(err, ErrWriteFrozen) {
		t.Fatalf("损坏兜底态应拒写，得 %v", err)
	}
}

func TestFreezePersistenceExpiredUntilAutoThaws(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/write-freeze.json"
	c1 := NewFreezeControllerWithPersistence(nil, path)
	// 用过去的 until 冻结并落盘，重启实例应因超时自动视为未冻结（自动解冻兜底跨重启存活）。
	c1.Freeze(time.Now().Add(-time.Minute), "已过期窗口")
	c2 := NewFreezeControllerWithPersistence(nil, path)
	if c2.State().Frozen {
		t.Fatal("until 已过期的冻结状态重启后应自动解冻")
	}
}
