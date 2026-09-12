package domain

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFreezeInitialNotFrozen(t *testing.T) {
	c := NewFreezeController(nil)
	if c.State().Frozen {
		t.Fatal("初始应未冻结")
	}
	if err := c.RequireBusinessWrite(); err != nil {
		t.Fatalf("初始 RequireBusinessWrite 应成功，得 %v", err)
	}
}

func TestFreezeBlocksWrite(t *testing.T) {
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
