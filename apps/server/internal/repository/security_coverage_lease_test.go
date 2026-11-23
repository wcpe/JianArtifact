package repository

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 租约持久记录上界回归：反复重启和接管后 security_audit_worker 只保留最近
// securityAuditLeaseRetain 代；任何旧代写者不能重放为写者。
func TestSecurityAuditLeaseBoundedAcrossRestarts(t *testing.T) {
	db := openSecurityEventDB(t)
	r := NewSecurityCoverageRepo(db)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	const restarts = 40
	workers := make([]string, 0, restarts)
	for i := 0; i < restarts; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		cp, err := r.Open(ctx, "", at)
		if err != nil {
			t.Fatalf("第 %d 次接管开启检查点失败：%v", i, err)
		}
		if cp.Worker == "" {
			t.Fatalf("第 %d 次接管未发放写者标识：%+v", i, cp)
		}
		b := SecurityAuditBatch{Number: 1, ID: NewSecurityEventID(),
			HeartbeatAt: at.Add(time.Second), CoveredThrough: at.Add(time.Second)}
		if _, err := r.Write(ctx, cp.Worker, b); err != nil {
			t.Fatalf("第 %d 次接管首批落库失败：%v", i, err)
		}
		workers = append(workers, cp.Worker)
	}
	var rows int
	if err := db.Get(&rows, `SELECT count(*) FROM security_audit_worker`); err != nil {
		t.Fatal(err)
	}
	if rows > SecurityAuditLeaseRetain {
		t.Fatalf("持久租约记录无界：%d 次接管留下 %d 行（上限 %d）", restarts, rows, SecurityAuditLeaseRetain)
	}
	// 失效写者不能重放：旧代 Open/Write 一律按被接管拒绝，无论其行是否已逐出。
	last := workers[len(workers)-1]
	stale := SecurityAuditBatch{Number: 2, ID: NewSecurityEventID(),
		HeartbeatAt: base.Add(time.Hour), CoveredThrough: base.Add(time.Hour)}
	for i, w := range workers[:len(workers)-1] {
		if _, err := r.Open(ctx, w, base); !errors.Is(err, ErrSecurityCoverageLease) {
			t.Fatalf("第 %d 代失效写者重放 Open 未被拒：%v", i, err)
		}
		if _, err := r.Write(ctx, w, stale); !errors.Is(err, ErrSecurityCoverageLease) {
			t.Fatalf("第 %d 代失效写者重放 Write 未被拒：%v", i, err)
		}
	}
	// 现任写者重复 Open 幂等，不被误判为失效。
	cp, err := r.Open(ctx, last, base)
	if err != nil || cp.Worker != last {
		t.Fatalf("现任写者幂等重开失败：%+v，%v", cp, err)
	}
}

// 定向竞争：多写者并发接管只产生单调递增代次、单一现任写者，且租约行有界。
// 需配合 go test -race 验证无数据竞争。
func TestSecurityAuditLeaseConcurrentTakeover(t *testing.T) {
	db := openSecurityEventDB(t)
	r := NewSecurityCoverageRepo(db)
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	const contenders = 16
	var wg sync.WaitGroup
	opened := make([]string, 0, contenders)
	var mu sync.Mutex
	errs := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				cp, err := r.Open(ctx, "", at.Add(time.Duration(i)*time.Second))
				if errors.Is(err, ErrSecurityCoverageBusy) {
					continue // 低优先级写让路，与运行期写者的退避语义一致
				}
				if err != nil {
					errs <- fmt.Errorf("接管 %d：%w", i, err)
					return
				}
				mu.Lock()
				opened = append(opened, cp.Worker)
				mu.Unlock()
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var checkpoint struct {
		Worker string `db:"worker_id"`
		Epoch  int64  `db:"lease_epoch"`
	}
	if err := db.Get(&checkpoint, `SELECT worker_id, lease_epoch FROM security_audit_checkpoint`); err != nil {
		t.Fatal(err)
	}
	// 任一代次至多一名写者；现任必须是最后发放的代次。
	if checkpoint.Worker == "" || checkpoint.Epoch != int64(contenders) {
		t.Fatalf("并发接管后代次或现任写者错误：%+v", checkpoint)
	}
	winner := fmt.Sprintf("w%d-", contenders)
	if len(checkpoint.Worker) < len(winner) || checkpoint.Worker[:len(winner)] != winner {
		t.Fatalf("现任写者不是最高代次：%s", checkpoint.Worker)
	}
	// 只有现任写者能推进批次；其余全部被拒。
	batch := SecurityAuditBatch{Number: 1, ID: NewSecurityEventID(), HeartbeatAt: at.Add(time.Minute), CoveredThrough: at.Add(time.Minute)}
	succeeded := 0
	for _, w := range opened {
		if _, err := r.Write(ctx, w, batch); err == nil {
			succeeded++
		} else if !errors.Is(err, ErrSecurityCoverageLease) {
			t.Fatalf("失效写者得到非租约错误：%v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("并发接管后应有且仅有一名可写写者，实际 %d", succeeded)
	}
	var rows int
	if err := db.Get(&rows, `SELECT count(*) FROM security_audit_worker`); err != nil {
		t.Fatal(err)
	}
	if rows > SecurityAuditLeaseRetain {
		t.Fatalf("并发接管后租约记录越界：%d 行（上限 %d）", rows, SecurityAuditLeaseRetain)
	}
}
